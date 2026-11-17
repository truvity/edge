package staticsite

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLiveHandler is a handler over the usual test site with live files in a
// directory of their own. write puts a file there; the caller refreshes.
func newLiveHandler(t *testing.T, names []string, mod func(*Config)) (h *Handler, live *Live, write func(name, body string), logs *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	logs = &bytes.Buffer{}
	h = newTestHandler(t, site, func(c *Config) {
		c.LiveDir = dir
		c.LiveFiles = names
		if mod != nil {
			mod(c)
		}
		require.Empty(t, c.validateLive())
	})
	live = NewLive(h.cfg, slog.New(slog.NewTextHandler(logs, nil)), h.m)
	h.SetLive(live)
	write = func(name, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	return h, live, write, logs
}

// liveReloads reads static_site_live_reloads_total{file,result} from the registry.
func liveReloads(t *testing.T, m *Metrics, file, result string) float64 {
	t.Helper()
	families, err := m.Reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "static_site_live_reloads_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["file"] == file && labels["result"] == result {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestLiveFileIsServedAndFollowsChanges(t *testing.T) {
	h, live, write, _ := newLiveHandler(t, []string{"flags.json"}, nil)

	live.Refresh()
	assert.Equal(t, 404, get(h, "GET", "/flags.json").Code, "nothing is served before the first valid file")

	write("flags.json", `{"a":{"enabled":true}}`)
	live.Refresh()
	w := get(h, "GET", "/flags.json")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, `{"a":{"enabled":true}}`, w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	first := w.Header().Get("ETag")
	require.NotEmpty(t, first)

	write("flags.json", `{"a":{"enabled":false}}`)
	live.Refresh()
	w = get(h, "GET", "/flags.json")
	assert.Equal(t, `{"a":{"enabled":false}}`, w.Body.String())
	assert.NotEqual(t, first, w.Header().Get("ETag"))
}

func TestLiveFileKeepsTheLastGoodVersion(t *testing.T) {
	h, live, write, logs := newLiveHandler(t, []string{"flags.json"}, func(c *Config) { c.LiveMaxBytes = 64 })
	good := `{"a":{"enabled":true}}`
	write("flags.json", good)
	live.Refresh()
	require.Equal(t, good, get(h, "GET", "/flags.json").Body.String())

	// In this order no two neighbours fail the same way, so each is logged.
	for _, tc := range []struct{ name, bad string }{
		{"half-written", `{"a":{"enab`},
		{"empty", ""},
		{"not json", "maintenance=on"},
		{"blank", " \n"},
		{"too large", `{"a":"` + strings.Repeat("x", 80) + `"}`},
	} {
		name, bad := tc.name, tc.bad
		write("flags.json", bad)
		live.Refresh()
		live.Refresh() // an unchanged bad file is neither served nor logged again
		assert.Equal(t, good, get(h, "GET", "/flags.json").Body.String(), name)
	}
	assert.Equal(t, 5, strings.Count(logs.String(), "live file not accepted"), "one line per distinct problem")
	assert.Equal(t, float64(5), liveReloads(t, h.m, "flags.json", liveResultInvalid))
	assert.Equal(t, float64(1), liveReloads(t, h.m, "flags.json", liveResultOK))

	// The writer going away does not take the file with it.
	require.NoError(t, os.Remove(filepath.Join(h.cfg.LiveDir, "flags.json")))
	live.Refresh()
	assert.Equal(t, good, get(h, "GET", "/flags.json").Body.String())

	// And a good file is picked up again after any of that.
	write("flags.json", `{}`)
	live.Refresh()
	assert.Equal(t, `{}`, get(h, "GET", "/flags.json").Body.String())
}

func TestLiveFileIsNeverReadThroughALink(t *testing.T) {
	h, live, _, logs := newLiveHandler(t, []string{"flags.json"}, nil)
	outside := filepath.Join(t.TempDir(), "secret.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{"secret":true}`), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(h.cfg.LiveDir, "flags.json")))
	live.Refresh()
	assert.Equal(t, 404, get(h, "GET", "/flags.json").Code)
	assert.Contains(t, logs.String(), "symbolic link")

	require.NoError(t, os.Remove(filepath.Join(h.cfg.LiveDir, "flags.json")))
	require.NoError(t, os.Mkdir(filepath.Join(h.cfg.LiveDir, "flags.json"), 0o755))
	live.Refresh()
	assert.Equal(t, 404, get(h, "GET", "/flags.json").Code)
	assert.Contains(t, logs.String(), "not a regular file")
}

func TestOnlyListedLiveNamesAreServed(t *testing.T) {
	h, live, write, _ := newLiveHandler(t, []string{"flags.json"}, nil)
	write("flags.json", `{}`)
	write("other.json", `{"not":"listed"}`)
	live.Refresh()
	assert.Equal(t, 200, get(h, "GET", "/flags.json").Code)
	assert.Equal(t, 404, get(h, "GET", "/other.json").Code)
	assert.Equal(t, 404, get(h, "GET", "/sub/flags.json").Code, "a live file is at the root only")
	assert.Equal(t, 200, get(h, "GET", "/data.json").Code, "the artifact is served as before")
}

func TestLiveNameWinsOverTheArtifactAndFallbacks(t *testing.T) {
	// data.json is in the artifact; listing it hands the name to the live file.
	h, live, write, _ := newLiveHandler(t, []string{"data.json", "state"}, func(c *Config) { c.SPAFallback = true })
	live.Refresh()
	w := get(h, "GET", "/data.json")
	assert.Equal(t, 404, w.Code, "the artifact's copy is not served under a live name")
	assert.Equal(t, "not found\n", w.Body.String(), "a plain 404, not the 404 page")
	assert.Equal(t, 404, get(h, "GET", "/state").Code, "no SPA fallback for a live name")

	write("data.json", `{"live":true}`)
	live.Refresh()
	assert.Equal(t, `{"live":true}`, get(h, "GET", "/data.json").Body.String())
}

func TestLiveFileIsNotSubstitutedOrParsedByType(t *testing.T) {
	h, live, write, _ := newLiveHandler(t, []string{"flags.json", "banner.txt"}, nil)
	write("flags.json", `{"id":"{{gtm}}"}`)
	write("banner.txt", "{{gtm}} is not JSON, and need not be")
	live.Refresh()
	assert.Equal(t, `{"id":"{{gtm}}"}`, get(h, "GET", "/flags.json").Body.String(), "no placeholder is filled in a live file")
	w := get(h, "GET", "/banner.txt")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
}

func TestLiveFileConditionalHeadAndPrivate(t *testing.T) {
	h, live, write, _ := newLiveHandler(t, []string{"flags.json"}, func(c *Config) { c.CachePrivate = true })
	write("flags.json", `{"a":1}`)
	live.Refresh()
	w := get(h, "GET", "/flags.json")
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	etag := w.Header().Get("ETag")

	w = get(h, "GET", "/flags.json", "If-None-Match", etag)
	assert.Equal(t, 304, w.Code)
	assert.Empty(t, w.Body.String())

	w = get(h, "HEAD", "/flags.json")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "7", w.Header().Get("Content-Length"))
	assert.Empty(t, w.Body.String())

	assert.Equal(t, 405, get(h, "POST", "/flags.json").Code)
}

func TestNoLiveFilesByDefault(t *testing.T) {
	cfg := Defaults()
	assert.Nil(t, NewLive(&cfg, nopLogger(), NewMetrics()))
	var none *Live
	assert.False(t, none.Listed("/flags.json"))
	assert.Nil(t, none.file("/flags.json"))
	assert.Empty(t, none.Names())
}

func TestLiveConfig(t *testing.T) {
	const dir, files = "STATIC_SITE_LIVE_DIR", "STATIC_SITE_LIVE_FILES"
	env := func(kv ...string) func(string) string {
		m := map[string]string{"STATIC_SITE_DIR": "/srv"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}

	c, err := FromEnv(env("STATIC_SITE_LIVE_DIR", "/var/run/live", "STATIC_SITE_LIVE_FILES", "flags.json, banner.txt",
		"STATIC_SITE_LIVE_INTERVAL", "2s", "STATIC_SITE_LIVE_MAX_BYTES", "1024"))
	require.NoError(t, err)
	assert.Equal(t, "/var/run/live", c.LiveDir)
	assert.Equal(t, []string{"flags.json", "banner.txt"}, c.LiveFiles)
	assert.Equal(t, "2s", c.LiveInterval.String())
	assert.Equal(t, int64(1024), c.LiveMaxBytes)

	c, err = FromEnv(env())
	require.NoError(t, err)
	assert.Empty(t, c.LiveDir)
	assert.Equal(t, DefaultLiveInterval, c.LiveInterval)
	assert.Equal(t, int64(DefaultLiveMaxBytes), c.LiveMaxBytes)

	for name, tc := range map[string]struct {
		kv   []string
		want string
	}{
		"dir without files": {[]string{dir, "/x"}, "set together"},
		"files without dir": {[]string{files, "flags.json"}, "set together"},
		"a path":            {[]string{dir, "/x", files, "a/flags.json"}, "not a plain file name"},
		"dot-dot":           {[]string{dir, "/x", files, ".."}, "not a plain file name"},
		"hidden":            {[]string{dir, "/x", files, ".env"}, "not a plain file name"},
		"twice":             {[]string{dir, "/x", files, "a.json,a.json"}, "listed twice"},
		"too fast":          {[]string{dir, "/x", files, "a.json", "STATIC_SITE_LIVE_INTERVAL", "100ms"}, "shorter than"},
		"bad size":          {[]string{dir, "/x", files, "a.json", "STATIC_SITE_LIVE_MAX_BYTES", "0"}, "not a positive integer"},
	} {
		_, err := FromEnv(env(tc.kv...))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}
}
