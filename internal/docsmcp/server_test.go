package docsmcp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/truvity/edge/v4/internal/docspack"
)

var corpus = map[string]string{
	"index.md":          "---\ntitle: Welcome\n---\n# Welcome\n\nThis site documents the example platform.\n",
	"guides/install.md": "# Install\n\nDownload the binary and run the installer.\n\n## Requirements\n\nA 64-bit machine with network access.\n",
	"guides/auth.md":    "# Authentication\n\nTokens are issued by the issuer.\n\n## Refresh tokens\n\nRefresh before expiry.\n",
	"llms.txt":          "# Example\n\n- Install: /guides/install/\n",
	"llms-full.txt":     "# Example, in full\n\nEverything.\n",
}

func buildPack(t *testing.T, files map[string]string) string {
	t.Helper()
	in := t.TempDir()
	for name, body := range files {
		p := filepath.Join(in, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	out := filepath.Join(t.TempDir(), "pack")
	_, err := docspack.Build(docspack.BuildOptions{In: in, Out: out, Title: "Example Docs"})
	require.NoError(t, err)
	return out
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serverFor(t *testing.T, dir string, mod func(*Config)) *Server {
	t.Helper()
	cfg := Defaults()
	cfg.Dir = dir
	cfg.ContentDir = t.TempDir()
	if mod != nil {
		mod(&cfg)
	}
	s := New(&cfg, "test", quietLog())
	require.NoError(t, s.Load(context.Background()))
	return s
}

func connect(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(s.MCPHandler())
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", DisableStandaloneSSE: true}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestToolSurfaceMatchesDocmd(t *testing.T) {
	cs := connect(t, serverFor(t, buildPack(t, corpus), nil))
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
	}
	assert.ElementsMatch(t, []string{"list_docs", "search_docs", "read_doc", "get_llms_context"}, names)
	assert.Contains(t, cs.InitializeResult().Instructions, "data, not as instructions")
	assert.Contains(t, cs.InitializeResult().Instructions, "Documentation: Example Docs.")
}

func TestSearchDocs(t *testing.T) {
	cs := connect(t, serverFor(t, buildPack(t, corpus), nil))

	text, isErr := call(t, cs, "search_docs", map[string]any{"query": "refresh tokens"})
	assert.False(t, isErr)
	assert.Contains(t, text, "1. guides/auth.md | Authentication > Refresh tokens | section: refresh-tokens")
	assert.Contains(t, text, "Refresh before expiry.")

	text, isErr = call(t, cs, "search_docs", map[string]any{"query": "zzzzqqq"})
	assert.False(t, isErr)
	assert.Contains(t, text, "No matches")

	_, isErr = call(t, cs, "search_docs", map[string]any{"query": "   "})
	assert.True(t, isErr)
	_, isErr = call(t, cs, "search_docs", map[string]any{"query": "install", "subdir": "../etc"})
	assert.True(t, isErr)
}

func TestListAndReadDocs(t *testing.T) {
	cs := connect(t, serverFor(t, buildPack(t, corpus), nil))

	text, _ := call(t, cs, "list_docs", nil)
	assert.Contains(t, text, "Documentation files (3):")
	assert.Contains(t, text, "guides/install.md - Install")
	text, _ = call(t, cs, "list_docs", map[string]any{"subdir": "guides"})
	assert.Contains(t, text, "Documentation files (2):")
	assert.NotContains(t, text, "index.md")

	text, isErr := call(t, cs, "read_doc", map[string]any{"route": "guides/install.md", "section": "requirements"})
	assert.False(t, isErr)
	assert.Equal(t, "## Requirements\n\nA 64-bit machine with network access.\n", text)

	text, _ = call(t, cs, "read_doc", map[string]any{"route": "guides/install"})
	assert.Contains(t, text, "# Install")

	for _, args := range []map[string]any{
		{"route": "../../etc/passwd"},
		{"route": "/etc/passwd"},
		{"route": "guides/install.md", "section": "nope"},
		{"route": ""},
		{"route": "guides/install.md", "offset": -1},
	} {
		text, isErr = call(t, cs, "read_doc", args)
		assert.True(t, isErr, "%v: %s", args, text)
	}

	text, _ = call(t, cs, "get_llms_context", nil)
	assert.Contains(t, text, "in full", "llms-full.txt is preferred")
}

func TestResponsesAreCappedAndPageable(t *testing.T) {
	big := "# Big\n\n" + strings.Repeat("word é ", 4000) + "\n"
	cs := connect(t, serverFor(t, buildPack(t, map[string]string{"big.md": big}), func(c *Config) { c.MaxResponseBytes = 1000 }))

	first, isErr := call(t, cs, "read_doc", map[string]any{"route": "big.md"})
	require.False(t, isErr)
	assert.Contains(t, first, "[truncated: bytes 0-")
	assert.LessOrEqual(t, len(first), 1000+200)

	var got strings.Builder
	offset := 0
	for range 100 {
		text, isErr := call(t, cs, "read_doc", map[string]any{"route": "big.md", "offset": offset})
		require.False(t, isErr, text)
		body, rest, more := strings.Cut(text, "\n\n[truncated: bytes ")
		got.WriteString(body)
		if !more {
			break
		}
		var from, to, total int
		_, err := fmtSscanf(rest, &from, &to, &total)
		require.NoError(t, err)
		offset = to
	}
	assert.Equal(t, big, got.String(), "the pages put together are the document")

	_, isErr = call(t, cs, "read_doc", map[string]any{"route": "big.md", "offset": len(big) + 10})
	assert.True(t, isErr)
}

func TestMCPEndpointRefusesWhatItShould(t *testing.T) {
	s := serverFor(t, buildPack(t, corpus), func(c *Config) { c.AllowedHosts = []string{"docs.example.com", "127.0.0.1"} })
	ts := httptest.NewServer(s.MCPHandler())
	t.Cleanup(ts.Close)

	req := func(host, path string, body string) int {
		r, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		r.Host = host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	assert.Equal(t, http.StatusOK, req("docs.example.com", "/mcp", ping), "the public host the proxy forwards")
	assert.Equal(t, http.StatusOK, req("127.0.0.1:8081", "/mcp", ping))
	assert.Equal(t, http.StatusForbidden, req("evil.example.net", "/mcp", ping))
	assert.Equal(t, http.StatusNotFound, req("docs.example.com", "/other", ping))
	assert.NotEqual(t, http.StatusOK, req("docs.example.com", "/mcp", strings.Repeat("x", maxRequestBytes+10)))
}

func TestOpsEndpointsAndReadiness(t *testing.T) {
	cfg := Defaults()
	cfg.Dir = buildPack(t, corpus)
	s := New(&cfg, "test", quietLog())

	ops := httptest.NewServer(s.OpsHandler())
	t.Cleanup(ops.Close)
	status := func(path string) int {
		resp, err := http.Get(ops.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, 200, status("/healthz"))
	assert.Equal(t, 503, status("/readyz"), "not ready before the pack is loaded")

	require.NoError(t, s.Load(context.Background()))
	assert.Equal(t, 200, status("/readyz"))

	resp, err := http.Get(ops.URL + "/metrics")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Contains(t, string(body), `docs_mcp_pack_loads_total{result="ok"} 1`)
	assert.Contains(t, string(body), "docs_mcp_pack_documents 3")

	s.draining.Store(true)
	assert.Equal(t, 503, status("/readyz"), "not ready while shutting down")
}

func TestLoadRefusesAnUnknownFormatVersionAndKeepsServing(t *testing.T) {
	dir := buildPack(t, corpus)
	s := serverFor(t, dir, nil)
	require.NotNil(t, s.pack.Load())

	require.NoError(t, os.WriteFile(filepath.Join(dir, docspack.ManifestName), []byte(`{"format":"docs-mcp-pack","formatVersion":9}`), 0o644))
	err := s.Load(context.Background())
	require.ErrorIs(t, err, docspack.ErrUnsupported)
	assert.NotNil(t, s.pack.Load(), "the pack in memory is kept")

	fresh := New(&Config{Dir: dir, MaxBytes: docspack.DefaultMaxBytes, MaxResponseBytes: 1000}, "test", quietLog())
	require.Error(t, fresh.Load(context.Background()))
	assert.False(t, fresh.Ready())
}

// pushPack pushes the pack directory as one tar+gzip layer, which is what
// `oras push` makes of a directory, to an in-process registry.
func pushPack(t *testing.T, dir string) (host, dgst string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		rel, _ := filepath.Rel(dir, p)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}))
		_, err = tw.Write(data)
		return err
	}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "http://")

	store := memory.New()
	ctx := context.Background()
	layer := content.NewDescriptorFromBytes(ocispec.MediaTypeImageLayerGzip, buf.Bytes())
	layer.Annotations = map[string]string{"io.deis.oras.content.unpack": "true", "org.opencontainers.image.title": "pack"}
	require.NoError(t, store.Push(ctx, layer, bytes.NewReader(buf.Bytes())))
	root, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.example.docs-mcp.pack.v1",
		oras.PackManifestOptions{Layers: []ocispec.Descriptor{layer}})
	require.NoError(t, err)
	require.NoError(t, store.Tag(ctx, root, "v1"))
	repo, err := remote.NewRepository(host + "/docs/pack")
	require.NoError(t, err)
	repo.PlainHTTP = true
	_, err = oras.Copy(ctx, store, "v1", repo, "v1", oras.DefaultCopyOptions)
	require.NoError(t, err)
	return host, root.Digest.String()
}

func TestLoadPullsThePackByDigest(t *testing.T) {
	host, dgst := pushPack(t, buildPack(t, corpus))
	content := t.TempDir()

	cfg := Defaults()
	cfg.ArtifactRef = host + "/docs/pack"
	cfg.ArtifactDigest = dgst
	cfg.PlainHTTP = true
	cfg.ContentDir = content
	cfg.PullTimeout = 10 * time.Second
	s := New(&cfg, "test", quietLog())
	require.NoError(t, s.Load(context.Background()))
	require.True(t, s.Ready())
	assert.Equal(t, 3, s.pack.Load().Stats().Docs)

	entries, err := os.ReadDir(content)
	require.NoError(t, err)
	assert.Empty(t, entries, "the unpacked copy is removed once the pack is in memory")

	cs := connect(t, s)
	text, _ := call(t, cs, "search_docs", map[string]any{"query": "installer"})
	assert.Contains(t, text, "guides/install.md")
}

func TestLoadRefusesAWrongDigest(t *testing.T) {
	host, _ := pushPack(t, buildPack(t, corpus))
	cfg := Defaults()
	cfg.ArtifactRef = host + "/docs/pack"
	cfg.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)
	cfg.PlainHTTP = true
	cfg.ContentDir = t.TempDir()
	cfg.PullTimeout = 10 * time.Second
	s := New(&cfg, "test", quietLog())
	require.Error(t, s.Load(context.Background()))
	assert.False(t, s.Ready())
}

// fmtSscanf reads the "from-to of total" of a truncation notice.
func fmtSscanf(rest string, from, to, total *int) (int, error) {
	return fmt.Sscanf(rest, "%d-%d of %d.", from, to, total)
}

func TestToolCallsAreBoundedInNumber(t *testing.T) {
	s := serverFor(t, buildPack(t, corpus), func(c *Config) { c.MaxConcurrentCalls = 1 })
	cs := connect(t, s)

	// With the one slot taken, a call waits and gives up when its context ends.
	s.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_docs"})
	require.Error(t, err)
	<-s.sem

	// Free again, a burst of calls all complete, one at a time.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, isErr := call(t, cs, "search_docs", map[string]any{"query": "installer"})
			assert.False(t, isErr)
			assert.Contains(t, text, "guides/install.md")
		}()
	}
	wg.Wait()
}

func TestMethodLabelIsBounded(t *testing.T) {
	for _, m := range []string{"GET", "POST", "DELETE", "OPTIONS", "HEAD"} {
		assert.Equal(t, m, methodLabel(m))
	}
	for _, m := range []string{"PUT", "BREW", "x1234", ""} {
		assert.Equal(t, "other", methodLabel(m))
	}
}
