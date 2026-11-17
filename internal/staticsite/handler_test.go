package staticsite

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func newTestHandler(t *testing.T, files map[string]string, mod func(*Config)) *Handler {
	t.Helper()
	cfg := Defaults()
	cfg.Placeholders = map[string]string{"gtm": "GTM-ABC123", "tricky": "a b"}
	if mod != nil {
		mod(&cfg)
	}
	site, err := Build(writeTree(t, files), &cfg)
	require.NoError(t, err)
	h := NewHandler(&cfg, NewMetrics())
	h.Set(site)
	return h
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var site = map[string]string{
	"index.html":           `<html><script>var g='{{ gtm }}';var u='{{unknown}}'</script>{{gtm}}</html>`,
	"404.html":             "<p>gone</p>",
	"app.0123abcd.js":      `console.log("{{gtm}}")`,
	"logo.png":             "PNGDATA",
	"docs/index.html":      "docs",
	"docs/guide.html":      "guide",
	"data.json":            `{"id":"{{gtm}}"}`,
	"style.css":            `a{content:"{{tricky}}"}`,
	"static/readme.txt":    "{{gtm}}",
	"vendor/lib-abc.js":    "x",
	"vendor/lib.deadbeef.": "x",
}

func TestContentTypeAndSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, site, nil)
	for path, want := range map[string]string{
		"/":                "text/html; charset=utf-8",
		"/app.0123abcd.js": "text/javascript; charset=utf-8",
		"/logo.png":        "image/png",
		"/data.json":       "application/json",
		"/style.css":       "text/css; charset=utf-8",
	} {
		w := get(h, "GET", path)
		assert.Equal(t, 200, w.Code, path)
		assert.Equal(t, want, w.Header().Get("Content-Type"), path)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"), path)
		assert.NotEmpty(t, w.Header().Get("Referrer-Policy"), path)
	}
}

func TestCacheControl(t *testing.T) {
	h := newTestHandler(t, site, nil)
	assert.Equal(t, "public, max-age=31536000, immutable", get(h, "GET", "/app.0123abcd.js").Header().Get("Cache-Control"))
	assert.Equal(t, "public, max-age=60, stale-while-revalidate=300", get(h, "GET", "/").Header().Get("Cache-Control"))
	assert.Equal(t, "public, max-age=60, stale-while-revalidate=300", get(h, "GET", "/logo.png").Header().Get("Cache-Control"))
	assert.Equal(t, "public, max-age=60, stale-while-revalidate=300", get(h, "GET", "/vendor/lib-abc.js").Header().Get("Cache-Control"))

	h = newTestHandler(t, site, func(c *Config) { c.HTMLMaxAge = 5e9; c.StaleWhileRevalidate = 0 })
	assert.Equal(t, "public, max-age=5", get(h, "GET", "/").Header().Get("Cache-Control"))
}

func TestPrivateCacheControl(t *testing.T) {
	const want = "private, no-store"
	h := newTestHandler(t, site, func(c *Config) { c.CachePrivate = true })
	cases := []struct {
		name, method, path string
		code               int
	}{
		{"html", "GET", "/", 200},
		{"hashed", "GET", "/app.0123abcd.js", 200},
		{"asset", "GET", "/logo.png", 200},
		{"head", "HEAD", "/logo.png", 200},
		{"redirect", "GET", "/docs", 301},
		{"not found page", "GET", "/missing", 404},
		{"method", "POST", "/", 405},
	}
	for _, tc := range cases {
		w := get(h, tc.method, tc.path)
		assert.Equal(t, tc.code, w.Code, tc.name)
		assert.Equal(t, want, w.Header().Get("Cache-Control"), tc.name)
	}

	etag := get(h, "GET", "/").Header().Get("ETag")
	require.NotEmpty(t, etag)
	w := get(h, "GET", "/", "If-None-Match", etag)
	assert.Equal(t, 304, w.Code)
	assert.Equal(t, want, w.Header().Get("Cache-Control"))

	cfg := Defaults()
	cfg.CachePrivate = true
	w = get(NewHandler(&cfg, NewMetrics()), "GET", "/")
	assert.Equal(t, 503, w.Code)
	assert.Equal(t, want, w.Header().Get("Cache-Control"))

	// Default-off responses are unchanged.
	h = newTestHandler(t, site, nil)
	assert.Equal(t, "no-store", get(h, "GET", "/missing").Header().Get("Cache-Control"))
	assert.Equal(t, "public, max-age=3600", get(h, "GET", "/docs").Header().Get("Cache-Control"))
}

func TestHTMLNeverImmutable(t *testing.T) {
	h := newTestHandler(t, map[string]string{"index.html": "x", "page.0123abcd.html": "y"}, nil)
	assert.NotContains(t, get(h, "GET", "/page.0123abcd.html").Header().Get("Cache-Control"), "immutable")
}

func TestETagAndNotModified(t *testing.T) {
	h := newTestHandler(t, site, nil)
	for _, path := range []string{"/", "/logo.png", "/app.0123abcd.js"} {
		w := get(h, "GET", path)
		etag := w.Header().Get("ETag")
		require.NotEmpty(t, etag, path)
		assert.True(t, strings.HasPrefix(etag, `"`), "strong ETag, not W/: %s", etag)
		assert.Empty(t, w.Header().Get("Last-Modified"), path)

		w2 := get(h, "GET", path, "If-None-Match", etag)
		assert.Equal(t, 304, w2.Code, path)
		assert.Empty(t, w2.Body.String(), path)
		assert.Equal(t, etag, w2.Header().Get("ETag"))
		assert.NotEmpty(t, w2.Header().Get("Cache-Control"))

		assert.Equal(t, 200, get(h, "GET", path, "If-None-Match", `"other"`).Code)
	}
}

func TestETagFollowsSubstitutedContent(t *testing.T) {
	a := newTestHandler(t, site, func(c *Config) { c.Placeholders = map[string]string{"gtm": "GTM-AAAA"} })
	b := newTestHandler(t, site, func(c *Config) { c.Placeholders = map[string]string{"gtm": "GTM-BBBB"} })
	assert.NotEqual(t, get(a, "GET", "/").Header().Get("ETag"), get(b, "GET", "/").Header().Get("ETag"))
	// A file with no token has the same ETag in both.
	assert.Equal(t, get(a, "GET", "/logo.png").Header().Get("ETag"), get(b, "GET", "/logo.png").Header().Get("ETag"))
}

func TestPlaceholders(t *testing.T) {
	h := newTestHandler(t, site, nil)
	body := get(h, "GET", "/").Body.String()
	assert.Contains(t, body, `var g='GTM-ABC123'`)
	assert.Contains(t, body, "{{unknown}}", "a name outside the allowlist is left as it is")
	assert.True(t, strings.HasSuffix(body, "GTM-ABC123</html>"))
	assert.Contains(t, get(h, "GET", "/app.0123abcd.js").Body.String(), `"GTM-ABC123"`)
	assert.JSONEq(t, `{"id":"GTM-ABC123"}`, get(h, "GET", "/data.json").Body.String())
	assert.Contains(t, get(h, "GET", "/style.css").Body.String(), `a\20 b`)
	// Not a placeholder type: served verbatim.
	assert.Equal(t, "{{gtm}}", get(h, "GET", "/static/readme.txt").Body.String())
}

func TestNoAllowlistNoSubstitution(t *testing.T) {
	h := newTestHandler(t, site, func(c *Config) { c.Placeholders = nil })
	assert.Contains(t, get(h, "GET", "/").Body.String(), "{{gtm}}")
}

func TestEscaping(t *testing.T) {
	assert.Equal(t, "a&amp;b&lt;c", escape(kindHTML, "a&b<c"))
	assert.Equal(t, `a\x27b\x22c\x3cd\x5c\x24\x60`, escape(kindJS, "a'b\"c<d\\$`"))
	assert.Equal(t, `a\"b\\`, escape(kindJSON, `a"b\`))
	assert.Equal(t, `a\3c `, escape(kindCSS, "a<"))
}

func TestConfigRefusesUnsafePlaceholders(t *testing.T) {
	for _, v := range []string{`a"b`, "a<b", "a\nb", "a'b", "a;b()", strings.Repeat("x", 513)} {
		c := Defaults()
		c.Dir = "/x"
		c.Placeholders = map[string]string{"n": v}
		assert.Error(t, c.Validate(), "%q", v)
	}
	c := Defaults()
	c.Dir = "/x"
	c.Placeholders = map[string]string{"bad name": "v"}
	assert.Error(t, c.Validate())
	c.Placeholders = map[string]string{"gtm": "GTM-ABC123"}
	assert.NoError(t, c.Validate())
}

func TestTraversalRefused(t *testing.T) {
	h := newTestHandler(t, site, nil)
	for _, target := range []string{"/../etc/passwd", "/a/../../x", "/docs/../../x", "/%2e%2e/x", "/a%5cb", "/a%00b", "/..%2f..%2fx"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path = mustUnescape(t, target)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, 400, w.Code, target)
	}
}

func mustUnescape(t *testing.T, s string) string {
	t.Helper()
	p, err := url.PathUnescape(s)
	require.NoError(t, err)
	return p
}

func TestOnlyFilesInTheSiteAreServed(t *testing.T) {
	root := writeTree(t, map[string]string{"index.html": "x"})
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))
	cfg := Defaults()
	s, err := Build(root, &cfg)
	require.NoError(t, err)
	h := NewHandler(&cfg, NewMetrics())
	h.Set(s)
	assert.Equal(t, 404, get(h, "GET", "/link").Code)
}

func TestDirectoriesAndIndex(t *testing.T) {
	h := newTestHandler(t, site, nil)
	assert.Equal(t, "docs", get(h, "GET", "/docs/").Body.String())
	w := get(h, "GET", "/docs")
	assert.Equal(t, 301, w.Code)
	assert.Equal(t, "/docs/", w.Header().Get("Location"))
	// No listing: a directory with no index is a 404.
	assert.Equal(t, 404, get(h, "GET", "/static/").Code)
	assert.Equal(t, 404, get(h, "GET", "/vendor").Code)
	// A protocol-relative path must not become an open redirect.
	w = get(h, "GET", "//docs")
	assert.Equal(t, 301, w.Code)
	assert.Equal(t, "/docs/", w.Header().Get("Location"))
}

func TestNotFoundPageAndSPA(t *testing.T) {
	h := newTestHandler(t, site, nil)
	w := get(h, "GET", "/missing")
	assert.Equal(t, 404, w.Code)
	assert.Equal(t, "<p>gone</p>", w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Empty(t, w.Header().Get("ETag"))
	assert.Equal(t, 404, get(h, "GET", "/missing").Code)

	h = newTestHandler(t, site, func(c *Config) { c.NotFoundPage = "" })
	assert.Equal(t, 404, get(h, "GET", "/missing").Code)

	h = newTestHandler(t, site, func(c *Config) { c.SPAFallback = true })
	assert.Equal(t, 200, get(h, "GET", "/some/route").Code)
	assert.Contains(t, get(h, "GET", "/some/route").Body.String(), "GTM-ABC123")
	assert.Equal(t, 404, get(h, "GET", "/missing.png").Code, "a missing asset is not the app")
}

func TestMethods(t *testing.T) {
	h := newTestHandler(t, site, nil)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		w := get(h, m, "/")
		assert.Equal(t, 405, w.Code, m)
		assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
	}
	w := get(h, "HEAD", "/logo.png")
	assert.Equal(t, 200, w.Code)
	assert.Empty(t, w.Body.String())
	assert.Equal(t, "7", w.Header().Get("Content-Length"))
	assert.NotEmpty(t, w.Header().Get("ETag"))
}

func TestHeaderOverrides(t *testing.T) {
	h := newTestHandler(t, site, func(c *Config) {
		c.Headers["X-Frame-Options"] = ""
		c.Headers["Content-Security-Policy"] = "default-src 'self'"
	})
	w := get(h, "GET", "/")
	assert.Empty(t, w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "default-src 'self'", w.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
}

func TestNoContentIs503(t *testing.T) {
	cfg := Defaults()
	assert.Equal(t, 503, get(NewHandler(&cfg, NewMetrics()), "GET", "/").Code)
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"STATIC_SITE_ARTIFACT_REF":       "registry.example.com/static/www:1",
		"STATIC_SITE_ARTIFACT_DIGEST":    "sha256:" + strings.Repeat("a", 64),
		"STATIC_SITE_HTML_MAX_AGE":       "30s",
		"STATIC_SITE_CACHE_PRIVATE":      "true",
		"STATIC_SITE_PLACEHOLDER_NAMES":  "gtm-id",
		"STATIC_SITE_PLACEHOLDER_GTM_ID": "GTM-1",
		"STATIC_SITE_HEADERS":            `{"X-Frame-Options":"SAMEORIGIN"}`,
	}
	c, err := FromEnv(func(k string) string { return env[k] })
	require.NoError(t, err)
	assert.Equal(t, "GTM-1", c.Placeholders["gtm-id"])
	assert.Equal(t, "SAMEORIGIN", c.Headers["X-Frame-Options"])
	assert.Equal(t, "nosniff", c.Headers["X-Content-Type-Options"])
	assert.True(t, c.CachePrivate)

	env["STATIC_SITE_CACHE_PRIVATE"] = "yes"
	_, err = FromEnv(func(k string) string { return env[k] })
	assert.Error(t, err, "not a boolean")
	env["STATIC_SITE_CACHE_PRIVATE"] = "true"

	env["STATIC_SITE_ARTIFACT_DIGEST"] = "latest"
	_, err = FromEnv(func(k string) string { return env[k] })
	assert.Error(t, err)

	_, err = FromEnv(func(string) string { return "" })
	assert.Error(t, err, "no source")
}

func TestReadyz(t *testing.T) {
	cfg := Defaults()
	cfg.Dir = writeTree(t, map[string]string{"index.html": "x"})
	s := NewServer(&cfg, nopLogger())
	ops := s.OpsHandler()
	assert.Equal(t, 200, get(ops, "GET", "/healthz").Code)
	assert.Equal(t, 503, get(ops, "GET", "/readyz").Code)
	require.NoError(t, s.Load(t.Context()))
	assert.Equal(t, 200, get(ops, "GET", "/readyz").Code)
	assert.Contains(t, get(ops, "GET", "/metrics").Body.String(), "static_site_content_files 1")
	s.draining.Store(true)
	assert.Equal(t, 503, get(ops, "GET", "/readyz").Code)
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func secretDir(t *testing.T, files map[string]string) string {
	t.Helper()
	return writeTree(t, files)
}

func strictEnv(dir string) map[string]string {
	return map[string]string{
		"STATIC_SITE_DIR":                        "/x",
		"STATIC_SITE_PLACEHOLDERS_DIR":           dir,
		"STATIC_SITE_PLACEHOLDERS_KEYS":          "gtmId",
		"STATIC_SITE_PLACEHOLDERS_STRICT":        "true",
		"STATIC_SITE_PLACEHOLDERS_OPTIONAL_KEYS": "extra",
	}
}

func TestPlaceholdersFromDir(t *testing.T) {
	dir := secretDir(t, map[string]string{"gtmId": "GTM-XXXXXXX\n", "stray": "never-served"})
	cfg, err := FromEnv(envOf(strictEnv(dir)))
	require.NoError(t, err)
	assert.Equal(t, "GTM-XXXXXXX", cfg.Placeholders["gtmId"], "one trailing newline trimmed")
	assert.NotContains(t, cfg.Placeholders, "stray", "only declared names are read")
	assert.Equal(t, "", cfg.Placeholders["extra"], "an absent optional name is empty")
	assert.Equal(t, []string{"extra", "gtmId"}, cfg.PlaceholderNames())

	files := map[string]string{
		"index.html": `<p>{{ gtmId }}{{extra}}</p>`,
		"a.txt":      `{{gtmId}}`,
		"logo.png":   "\x89PNG{{gtmId}}",
		"app.js":     `var g='{{gtmId}}'`,
	}
	cfg.Dir = ""
	site, err := Build(writeTree(t, files), &cfg)
	require.NoError(t, err)
	h := NewHandler(&cfg, NewMetrics())
	h.Set(site)
	assert.Equal(t, "<p>GTM-XXXXXXX</p>", get(h, "GET", "/").Body.String())
	assert.Equal(t, `var g='GTM-XXXXXXX'`, get(h, "GET", "/app.js").Body.String())
	assert.Equal(t, "{{gtmId}}", get(h, "GET", "/a.txt").Body.String(), "txt is not a placeholder type")
	assert.Equal(t, "\x89PNG{{gtmId}}", get(h, "GET", "/logo.png").Body.String(), "binary untouched")
	w := get(h, "GET", "/")
	assert.Equal(t, "18", w.Header().Get("Content-Length"))
}

func TestStrictMissingValueFails(t *testing.T) {
	cfg, err := FromEnv(envOf(strictEnv(secretDir(t, map[string]string{}))))
	require.NoError(t, err, "a missing value is only an error if the content uses it")
	cfg.Dir = ""
	_, err = Build(writeTree(t, map[string]string{"logo.png": "x"}), &cfg)
	require.NoError(t, err)
	_, err = Build(writeTree(t, map[string]string{"index.html": "{{gtmId}}"}), &cfg)
	require.ErrorContains(t, err, `placeholder "gtmId" has no value`)
}

func TestStrictUnknownPlaceholderFails(t *testing.T) {
	cfg, err := FromEnv(envOf(strictEnv(secretDir(t, map[string]string{"gtmId": "GTM-XXXXXXX"}))))
	require.NoError(t, err)
	cfg.Dir = ""
	_, err = Build(writeTree(t, map[string]string{"index.html": "{{gtmId}} {{nope}}"}), &cfg)
	require.ErrorContains(t, err, `unknown placeholder "nope"`)
	// Not strict: left as written, as before.
	cfg.PlaceholderStrict = false
	site, err := Build(writeTree(t, map[string]string{"index.html": "{{nope}}"}), &cfg)
	require.NoError(t, err)
	assert.NotNil(t, site)
}

func TestPlaceholderValuesNeverInErrorsOrLogs(t *testing.T) {
	const secret = "GTM-SECRETVALUE"
	dir := secretDir(t, map[string]string{"gtmId": secret, "bad": secret + "<"})
	env := strictEnv(dir)
	env["STATIC_SITE_PLACEHOLDERS_KEYS"] = "gtmId,bad"
	cfg, err := FromEnv(envOf(env))
	require.Error(t, err, "a value outside the safe set is refused")
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, strings.Join(cfg.PlaceholderNames(), ","), secret)

	env["STATIC_SITE_PLACEHOLDERS_KEYS"] = "gtmId"
	cfg, err = FromEnv(envOf(env))
	require.NoError(t, err)
	cfg.Dir = ""
	_, err = Build(writeTree(t, map[string]string{"index.html": "{{gtmId}} {{nope}}"}), &cfg)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)

	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("placeholders", "names", cfg.PlaceholderNames(), "strict", cfg.PlaceholderStrict)
	assert.Contains(t, buf.String(), "gtmId")
	assert.NotContains(t, buf.String(), secret)
}

func TestPlaceholderDirRefusesBadName(t *testing.T) {
	env := strictEnv(secretDir(t, nil))
	env["STATIC_SITE_PLACEHOLDERS_KEYS"] = "../escape"
	_, err := FromEnv(envOf(env))
	assert.Error(t, err)
}

func TestConfiguredContentType(t *testing.T) {
	files := map[string]string{
		"index.html":                             "<html></html>",
		".well-known/apple-app-site-association": `{"applinks":{}}`,
		"LICENSE":                                "text",
	}
	h := newTestHandler(t, files, func(c *Config) {
		c.ContentTypes = map[string]string{"/.well-known/apple-app-site-association": "application/json", "/absent": "text/plain"}
		require.NoError(t, errors.Join(validateContentTypes(c)...))
	})
	w := get(h, "GET", "/.well-known/apple-app-site-association")
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, `{"applinks":{}}`, w.Body.String())
	assert.Equal(t, "application/octet-stream", get(h, "GET", "/LICENSE").Header().Get("Content-Type"), "an unlisted file keeps its type")
	assert.Equal(t, "text/html; charset=utf-8", get(h, "GET", "/").Header().Get("Content-Type"))
	assert.Equal(t, 404, get(h, "GET", "/absent").Code, "a path that is not in the site is not created")
}

func TestContentTypesConfig(t *testing.T) {
	env := func(v string) func(string) string {
		m := map[string]string{"STATIC_SITE_DIR": "/srv", "STATIC_SITE_CONTENT_TYPES": v}
		return func(k string) string { return m[k] }
	}
	c, err := FromEnv(env(`{"/.well-known/assoc":"application/json","/feed":"application/atom+xml; charset=utf-8"}`))
	require.NoError(t, err)
	assert.Equal(t, "application/json", c.ContentTypes["/.well-known/assoc"])

	for name, tc := range map[string]struct{ v, want string }{
		"not json":      {`{`, "STATIC_SITE_CONTENT_TYPES"},
		"relative path": {`{"assoc":"application/json"}`, "not a clean absolute file path"},
		"dot-dot":       {`{"/a/../b":"application/json"}`, "not a clean absolute file path"},
		"a directory":   {`{"/a/":"application/json"}`, "not a clean absolute file path"},
		"header break":  {`{"/a":"text/plain\r\nX-Evil: 1"}`, "is not type/subtype"},
		"no subtype":    {`{"/a":"json"}`, "is not type/subtype"},
		"a parameter":   {`{"/a":"text/html; charset=utf-7"}`, "is not type/subtype"},
	} {
		_, err := FromEnv(env(tc.v))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}
}
