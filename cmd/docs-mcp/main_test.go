package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/internal/docspack"
)

func runCmd(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestPackWritesAPackAReaderAccepts(t *testing.T) {
	in := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(in, "a.md"), []byte("# A\n\nhello world\n"), 0o644))
	out := filepath.Join(t.TempDir(), "pack")

	code, stdout, stderr := runCmd(t, map[string]string{"SOURCE_DATE_EPOCH": "1700000000"}, "pack", "--in", in, "--out", out, "--title", "Example Docs")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "packed 1 documents")
	assert.Contains(t, stdout, "version 1")

	p, err := docspack.Load(out, docspack.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Example Docs", p.Manifest.Title)
	assert.Equal(t, "2023-11-14T22:13:20Z", p.Manifest.CreatedAt)

	// A second run into the same directory is refused: no stale files.
	code, _, stderr = runCmd(t, nil, "pack", "--in", in, "--out", out)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "not empty")
}

func TestPackRefusals(t *testing.T) {
	for name, args := range map[string][]string{
		"no flags":    {"pack"},
		"no out":      {"pack", "--in", "x"},
		"extra":       {"pack", "--in", "x", "--out", "y", "z"},
		"unknown":     {"pack", "--in", "x", "--out", "y", "--nope"},
		"bad command": {"frobnicate"},
		"nothing":     {},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, _ := runCmd(t, nil, args...)
			assert.Equal(t, 2, code)
		})
	}
	code, _, stderr := runCmd(t, map[string]string{"SOURCE_DATE_EPOCH": "yesterday"}, "pack", "--in", t.TempDir(), "--out", filepath.Join(t.TempDir(), "o"))
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "SOURCE_DATE_EPOCH")

	code, _, stderr = runCmd(t, nil, "pack", "--in", t.TempDir(), "--out", filepath.Join(t.TempDir(), "o"))
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "no Markdown files")
}

func TestServeRefusesBadConfiguration(t *testing.T) {
	code, _, stderr := runCmd(t, nil, "serve")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "invalid configuration")
}

func TestVersion(t *testing.T) {
	code, stdout, _ := runCmd(t, nil, "version")
	assert.Equal(t, 0, code)
	assert.Equal(t, "dev\n", stdout)
}
