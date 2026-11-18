package staticsite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
)

type tarEntry struct {
	name, body, link string
	typ              byte
}

func makeTar(t *testing.T, gz bool, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Close() error
	} = nopCloser{&buf}
	if gz {
		w = gzip.NewWriter(&buf)
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, w.Close())
	return buf.Bytes()
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

// pushLayers pushes one manifest of the given layers to an in-process
// registry and returns its host, repository and digest.
func pushLayers(t *testing.T, layers map[string]struct {
	mt, title string
	data      []byte
	unpack    bool
}, order []string) (host string, dgst digest.Digest) {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "http://")

	store := memory.New()
	ctx := context.Background()
	var descs []ocispec.Descriptor
	for _, k := range order {
		l := layers[k]
		d := content.NewDescriptorFromBytes(l.mt, l.data)
		d.Annotations = map[string]string{}
		if l.title != "" {
			d.Annotations[annotationTitle] = l.title
		}
		if l.unpack {
			d.Annotations[annotationUnpack] = "true"
		}
		require.NoError(t, store.Push(ctx, d, bytes.NewReader(l.data)))
		descs = append(descs, d)
	}
	root, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.example.static-site.v1", oras.PackManifestOptions{Layers: descs})
	require.NoError(t, err)
	require.NoError(t, store.Tag(ctx, root, "v1"))

	repo, err := remote.NewRepository(host + "/static/www")
	require.NoError(t, err)
	repo.PlainHTTP = true
	_, err = oras.Copy(ctx, store, "v1", repo, "v1", oras.DefaultCopyOptions)
	require.NoError(t, err)
	return host, root.Digest
}

type layer = struct {
	mt, title string
	data      []byte
	unpack    bool
}

func pullCfg(host, ref, dgst string) *Config {
	c := Defaults()
	c.ArtifactRef = host + "/static/www" + ref
	c.ArtifactDigest = dgst
	c.PlainHTTP = true
	return &c
}

func TestPullTarGzByTagAndDigest(t *testing.T) {
	data := makeTar(t, true, []tarEntry{
		{name: "./", typ: tar.TypeDir},
		{name: "./index.html", body: "<h1>{{gtm}}</h1>"},
		{name: "assets/app.0123abcd.js", body: "js"},
	})
	host, dgst := pushLayers(t, map[string]layer{"a": {mt: ocispec.MediaTypeImageLayerGzip, title: "www", data: data, unpack: true}}, []string{"a"})

	for name, cfg := range map[string]*Config{
		"tag":    pullCfg(host, ":v1", ""),
		"digest": pullCfg(host, "", dgst.String()),
		// The digest wins over a tag that points elsewhere.
		"digest over tag": pullCfg(host, ":nonexistent", dgst.String()),
	} {
		t.Run(name, func(t *testing.T) {
			dest := t.TempDir()
			got, err := Pull(t.Context(), cfg, dest)
			require.NoError(t, err)
			assert.Equal(t, dgst, got)
			b, err := os.ReadFile(filepath.Join(dest, "index.html"))
			require.NoError(t, err)
			assert.Equal(t, "<h1>{{gtm}}</h1>", string(b))
			assert.FileExists(t, filepath.Join(dest, "assets", "app.0123abcd.js"))
		})
	}
}

func TestPullSingleFileLayersAndServe(t *testing.T) {
	host, dgst := pushLayers(t, map[string]layer{
		"i": {mt: "text/html", title: "index.html", data: []byte("<p>{{gtm}}</p>")},
		"c": {mt: "text/css", title: "css/site.css", data: []byte("a{}")},
	}, []string{"i", "c"})
	cfg := pullCfg(host, "", dgst.String())
	cfg.Placeholders = map[string]string{"gtm": "GTM-9"}
	s := NewServer(cfg, nopLogger())
	cfg.ContentDir = t.TempDir()
	require.NoError(t, s.Load(t.Context()))
	w := get(s.SiteHandler(), "GET", "/")
	assert.Equal(t, "<p>GTM-9</p>", w.Body.String())
	assert.Equal(t, 200, get(s.SiteHandler(), "GET", "/css/site.css").Code)
	assert.Equal(t, dgst.String(), s.handler.Site().Digest)

	// A failed reload keeps serving the old content.
	cfg.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)
	require.Error(t, s.Load(t.Context()))
	assert.Equal(t, 200, get(s.SiteHandler(), "GET", "/").Code)
}

func TestPullRefusals(t *testing.T) {
	cases := map[string][]tarEntry{
		"traversal": {{name: "../evil", body: "x"}},
		"absolute":  {{name: "/etc/evil", body: "x"}},
		"symlink":   {{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"hardlink":  {{name: "l", typ: tar.TypeLink, link: "index.html"}},
		"nested ..": {{name: "a/../../evil", body: "x"}},
		"backslash": {{name: `a\..\evil`, body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			host, dgst := pushLayers(t, map[string]layer{"a": {mt: ocispec.MediaTypeImageLayerGzip, data: makeTar(t, true, entries), unpack: true}}, []string{"a"})
			dest := filepath.Join(t.TempDir(), "d")
			require.NoError(t, os.Mkdir(dest, 0o755))
			_, err := Pull(t.Context(), pullCfg(host, "", dgst.String()), dest)
			require.Error(t, err)
			assert.NoFileExists(t, filepath.Join(filepath.Dir(dest), "evil"))
		})
	}
}

func TestPullLimits(t *testing.T) {
	data := makeTar(t, true, []tarEntry{{name: "a", body: strings.Repeat("x", 100000)}, {name: "b", body: strings.Repeat("x", 100000)}})
	host, dgst := pushLayers(t, map[string]layer{"a": {mt: ocispec.MediaTypeImageLayerGzip, data: data, unpack: true}}, []string{"a"})
	cfg := pullCfg(host, "", dgst.String())
	cfg.MaxBytes = 150000
	_, err := Pull(t.Context(), cfg, t.TempDir())
	assert.ErrorContains(t, err, "larger than")

	cfg = pullCfg(host, "", dgst.String())
	cfg.MaxFiles = 1
	_, err = Pull(t.Context(), cfg, t.TempDir())
	assert.ErrorContains(t, err, "more than 1 files")
}

func TestPullCountsDirectoriesTowardTheFileLimit(t *testing.T) {
	data := makeTar(t, true, []tarEntry{{name: "d1/", typ: tar.TypeDir}, {name: "d2/", typ: tar.TypeDir}, {name: "d3/", typ: tar.TypeDir}})
	host, dgst := pushLayers(t, map[string]layer{"a": {mt: ocispec.MediaTypeImageLayerGzip, data: data, unpack: true}}, []string{"a"})
	cfg := pullCfg(host, "", dgst.String())
	cfg.MaxFiles = 2
	_, err := Pull(t.Context(), cfg, t.TempDir())
	assert.ErrorContains(t, err, "more than 2 files")
}

func TestPullErrors(t *testing.T) {
	host, _ := pushLayers(t, map[string]layer{"a": {mt: "text/plain", title: "x", data: []byte("x")}}, []string{"a"})
	_, err := Pull(t.Context(), pullCfg(host, ":nope", ""), t.TempDir())
	assert.Error(t, err)
	_, err = Pull(t.Context(), pullCfg(host, "", ""), t.TempDir())
	assert.ErrorContains(t, err, "no tag")
	host, dgst := pushLayers(t, map[string]layer{"a": {mt: "text/plain", data: []byte("x")}}, []string{"a"})
	_, err = Pull(t.Context(), pullCfg(host, "", dgst.String()), t.TempDir())
	assert.ErrorContains(t, err, "title")
}
