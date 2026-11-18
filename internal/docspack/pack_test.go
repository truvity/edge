package docspack

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var corpus = map[string]string{
	"index.md": "---\ntitle: Welcome\n---\n# Welcome\n\nThis site documents the example platform.\n",
	"guides/install.md": "# Install\n\nDownload the binary and run the installer.\n\n## Requirements\n\nA 64-bit machine with network access.\n\n" +
		"## Troubleshooting\n\nIf the installer fails, read the log.\n",
	"guides/auth.md":    "# Authentication\n\nTokens are issued by the issuer. A token expires after an hour.\n\n## Refresh tokens\n\nRefresh before expiry.\n",
	"reference/api.mdx": "# API\n\nThe installer exposes no API. Authentication is described elsewhere.\n",
	".hidden/secret.md": "# Secret\n\nnever packed\n",
	"node_modules/x.md": "# Dep\n",
	"notes.txt":         "not markdown",
	"llms.txt":          "# Example\n\n- [Install](/guides/install/)\n",
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return root
}

func buildCorpus(t *testing.T) (string, Summary) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pack")
	sum, err := Build(BuildOptions{In: writeTree(t, corpus), Out: out})
	require.NoError(t, err)
	return out, sum
}

func TestBuildAndLoad(t *testing.T) {
	out, sum := buildCorpus(t)
	assert.Equal(t, 4, sum.Docs)

	p, err := Load(out, LoadOptions{})
	require.NoError(t, err)
	m := p.Manifest
	assert.Equal(t, Format, m.Format)
	assert.Equal(t, FormatVersion, m.FormatVersion)
	assert.Empty(t, m.CreatedAt)
	var routes []string
	for i := range m.Docs {
		routes = append(routes, m.Docs[i].Route)
	}
	assert.Equal(t, []string{"guides/auth.md", "guides/install.md", "index.md", "reference/api.mdx"}, routes, "sorted; dot directories and node_modules left out")
	assert.NotNil(t, m.LLMS)
	assert.Nil(t, m.LLMSFull)
	assert.Equal(t, sum.Sections, p.Stats().Sections)
	assert.Equal(t, "Welcome", m.Docs[2].Title)
	assert.Equal(t, "/guides/install/", m.Docs[1].URL)
}

func TestBuildIsDeterministic(t *testing.T) {
	in := writeTree(t, corpus)
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	_, err := Build(BuildOptions{In: in, Out: a})
	require.NoError(t, err)
	// Touch the input: file times must not reach the pack.
	require.NoError(t, os.Chtimes(filepath.Join(in, "index.md"), time.Unix(1, 0), time.Unix(1, 0)))
	_, err = Build(BuildOptions{In: in, Out: b})
	require.NoError(t, err)

	for _, name := range []string{ManifestName, IndexName, "docs/index.md", "llms.txt"} {
		x, err := os.ReadFile(filepath.Join(a, name))
		require.NoError(t, err)
		y, err := os.ReadFile(filepath.Join(b, name))
		require.NoError(t, err)
		assert.Equal(t, x, y, name)
	}
}

func TestBuildRecordsTimeOnlyWhenGiven(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pack")
	_, err := Build(BuildOptions{In: writeTree(t, corpus), Out: out, CreatedAt: time.Unix(1700000000, 0)})
	require.NoError(t, err)
	p, err := Load(out, LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "2023-11-14T22:13:20Z", p.Manifest.CreatedAt)
}

func TestBuildRefusals(t *testing.T) {
	in := writeTree(t, corpus)

	_, err := Build(BuildOptions{In: filepath.Join(in, "nope"), Out: filepath.Join(t.TempDir(), "o")})
	assert.ErrorContains(t, err, "not a directory")

	_, err = Build(BuildOptions{In: in, Out: filepath.Join(in, "pack")})
	assert.ErrorContains(t, err, "inside the input")

	full := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(full, "x"), nil, 0o644))
	_, err = Build(BuildOptions{In: in, Out: full})
	assert.ErrorContains(t, err, "not empty")

	_, err = Build(BuildOptions{In: t.TempDir(), Out: filepath.Join(t.TempDir(), "o")})
	assert.ErrorContains(t, err, "no Markdown files")

	link := writeTree(t, map[string]string{"a.md": "# A\n"})
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(link, "b.md")))
	_, err = Build(BuildOptions{In: link, Out: filepath.Join(t.TempDir(), "o")})
	assert.ErrorContains(t, err, "symbolic link")

	_, err = Build(BuildOptions{In: in, Out: filepath.Join(t.TempDir(), "o"), MaxBytes: 100})
	assert.ErrorContains(t, err, "over the 100 limit")
}

func TestLoadRefusesAnUnknownFormatVersion(t *testing.T) {
	out, _ := buildCorpus(t)
	raw, err := os.ReadFile(filepath.Join(out, ManifestName))
	require.NoError(t, err)
	// A future format may have a different shape: only the header is read.
	require.NoError(t, os.WriteFile(filepath.Join(out, ManifestName), []byte(`{"format":"docs-mcp-pack","formatVersion":2,"docs":{"new":"shape"}}`), 0o644))
	_, err = Load(out, LoadOptions{})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.ErrorContains(t, err, "version 2")

	require.NoError(t, os.WriteFile(filepath.Join(out, ManifestName), []byte(`{"format":"something-else","formatVersion":1}`), 0o644))
	_, err = Load(out, LoadOptions{})
	assert.ErrorIs(t, err, ErrUnsupported)

	require.NoError(t, os.WriteFile(filepath.Join(out, ManifestName), raw, 0o644))
	_, err = Load(out, LoadOptions{})
	assert.NoError(t, err)
}

func TestLoadRefusesADamagedPack(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, out string){
		"a document changed": func(t *testing.T, out string) {
			require.NoError(t, os.WriteFile(filepath.Join(out, "docs/index.md"), []byte("# Welcome\n\nThis site documents the evil platform.\n"), 0o644))
		},
		"a document missing": func(t *testing.T, out string) { require.NoError(t, os.Remove(filepath.Join(out, "docs/index.md"))) },
		"the index changed": func(t *testing.T, out string) {
			require.NoError(t, os.WriteFile(filepath.Join(out, IndexName), []byte("{}"), 0o644))
		},
		"a symlinked document": func(t *testing.T, out string) {
			require.NoError(t, os.Remove(filepath.Join(out, "docs/index.md")))
			require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(out, "docs/index.md")))
		},
		"no manifest": func(t *testing.T, out string) { require.NoError(t, os.Remove(filepath.Join(out, ManifestName))) },
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := buildCorpus(t)
			damage(t, out)
			_, err := Load(out, LoadOptions{})
			assert.Error(t, err)
		})
	}
}

func TestLoadHoldsThePackToItsByteLimit(t *testing.T) {
	out, _ := buildCorpus(t)
	_, err := Load(out, LoadOptions{MaxBytes: 200})
	assert.Error(t, err)
}

func TestLoadRefusesAManifestThatPointsOutside(t *testing.T) {
	out, _ := buildCorpus(t)
	raw, err := os.ReadFile(filepath.Join(out, ManifestName))
	require.NoError(t, err)
	bad := []byte(string(raw))
	bad = []byte(replaceOnce(string(bad), `"route": "index.md"`, `"route": "../x.md"`))
	require.NoError(t, os.WriteFile(filepath.Join(out, ManifestName), bad, 0o644))
	_, err = Load(out, LoadOptions{})
	assert.Error(t, err)
}

func replaceOnce(s, old, repl string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + repl + s[i+len(old):]
		}
	}
	return s
}

func loaded(t *testing.T) *Pack {
	t.Helper()
	out, _ := buildCorpus(t)
	p, err := Load(out, LoadOptions{})
	require.NoError(t, err)
	return p
}

func TestSearchRanksHeadingsAndTerms(t *testing.T) {
	p := loaded(t)

	hits, err := p.Search("install", SearchOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, "guides/install.md", hits[0].Route, "the Install section outranks passing mentions")
	assert.Equal(t, "install", hits[0].Anchor)
	assert.Equal(t, []string{"Install"}, hits[0].Heading)
	assert.Contains(t, hits[0].Snippet, "installer")

	hits, err = p.Search("refresh tokens expiry", SearchOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	assert.Equal(t, "guides/auth.md", hits[0].Route)
	assert.Equal(t, "refresh-tokens", hits[0].Anchor)
	assert.Equal(t, []string{"Authentication", "Refresh tokens"}, hits[0].Heading)
}

func TestSearchPrefixAndSubdirAndLimit(t *testing.T) {
	p := loaded(t)

	hits, err := p.Search("authent", SearchOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, hits, "a prefix of a term finds it")

	hits, err = p.Search("install", SearchOptions{Subdir: "reference"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "reference/api.mdx", hits[0].Route)

	hits, err = p.Search("the", SearchOptions{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, hits, 1)

	hits, err = p.Search("zzzzqqq", SearchOptions{})
	require.NoError(t, err)
	assert.Empty(t, hits)

	_, err = p.Search("a - !", SearchOptions{})
	assert.Error(t, err)
	_, err = p.Search("install", SearchOptions{Subdir: "../x"})
	assert.Error(t, err)
}

func TestSearchIsDeterministic(t *testing.T) {
	p := loaded(t)
	a, err := p.Search("token installer", SearchOptions{})
	require.NoError(t, err)
	for range 5 {
		b, err := p.Search("token installer", SearchOptions{})
		require.NoError(t, err)
		assert.Equal(t, a, b)
	}
}

func TestReadAndResolve(t *testing.T) {
	p := loaded(t)

	for _, route := range []string{"guides/install.md", "guides/install", "/guides/install/", "./guides/install.md"} {
		info, text, err := p.Read(route, "")
		require.NoError(t, err, route)
		assert.Equal(t, "guides/install.md", info.Route)
		assert.Contains(t, text, "## Troubleshooting")
	}
	_, text, err := p.Read("guides/install.md", "requirements")
	require.NoError(t, err)
	assert.Equal(t, "## Requirements\n\nA 64-bit machine with network access.\n\n", text)

	_, text, err = p.Read("guides/install.md", "#install")
	require.NoError(t, err)
	assert.Contains(t, text, "Troubleshooting", "a section with its subsections")

	_, _, err = p.Read("guides/install.md", "nope")
	require.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "requirements")

	for _, bad := range []string{"", "nope.md", "../../etc/passwd", "guides"} {
		_, _, err = p.Read(bad, "")
		assert.ErrorIs(t, err, ErrNotFound, bad)
	}

	_, text, err = p.Read("index", "")
	require.NoError(t, err)
	assert.Contains(t, text, "title: Welcome", "the file is returned as it is, front matter included")
}

func TestDocsListingAndLLMS(t *testing.T) {
	p := loaded(t)
	assert.Len(t, p.Docs(""), 4)
	docs := p.Docs("guides")
	require.Len(t, docs, 2)
	assert.Equal(t, "guides/auth.md", docs[0].Route)
	assert.Empty(t, p.Docs("guide"), "a directory, not a prefix of a name")

	name, text := p.LLMS()
	assert.Equal(t, LLMSName, name)
	assert.Contains(t, text, "[Install]")

	out := filepath.Join(t.TempDir(), "p")
	_, err := Build(BuildOptions{In: writeTree(t, map[string]string{"a.md": "# A\n"}), Out: out})
	require.NoError(t, err)
	bare, err := Load(out, LoadOptions{})
	require.NoError(t, err)
	name, text = bare.LLMS()
	assert.Equal(t, "outline", name)
	assert.Contains(t, text, "a.md: A")
}
