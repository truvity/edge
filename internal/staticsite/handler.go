package staticsite

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Handler serves the current Site. It holds no state beyond the pointer, so
// a reload is a single atomic swap.
type Handler struct {
	cfg  *Config
	site atomic.Pointer[Site]
	m    *Metrics
}

// NewHandler returns a handler with no content: it answers 503 until Set.
func NewHandler(cfg *Config, m *Metrics) *Handler { return &Handler{cfg: cfg, m: m} }

// Set swaps in a new snapshot.
func (h *Handler) Set(s *Site) { h.site.Store(s) }

// Site returns the current snapshot, nil before the first load.
func (h *Handler) Site() *Site { return h.site.Load() }

func (h *Handler) setHeaders(w http.Header) {
	for k, v := range h.cfg.Headers {
		if v != "" {
			w.Set(k, v)
		}
	}
}

// privateNoStore is the Cache-Control of every response when CachePrivate is on.
const privateNoStore = "private, no-store"

// noStore is the Cache-Control of a failure, and of every response under
// CachePrivate.
func (h *Handler) noStore() string {
	if h.cfg.CachePrivate {
		return privateNoStore
	}
	return "no-store"
}

func (h *Handler) fail(w http.ResponseWriter, code int, msg string) {
	h.setHeaders(w.Header())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", h.noStore())
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n")
}

// cleanPath turns a request path into a site key, or refuses it. A dot-dot
// segment, a NUL or a backslash is refused rather than normalised: nobody
// legitimate sends one.
func cleanPath(p string) (string, bool) {
	if p == "" || p[0] != '/' || strings.ContainsAny(p, "\x00\\") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c, true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	site := h.site.Load()
	if site == nil {
		h.fail(w, http.StatusServiceUnavailable, "content not loaded")
		return
	}
	p, ok := cleanPath(r.URL.Path)
	if !ok {
		h.fail(w, http.StatusBadRequest, "bad path")
		return
	}
	key := strings.TrimSuffix(p, "/")
	if key == "" {
		key = "/"
	}
	var e *entry
	switch {
	case strings.HasSuffix(p, "/") || key == "/":
		e = site.files[strings.TrimSuffix(key, "/")+"/index.html"]
	case site.files[key] != nil:
		e = site.files[key]
	case site.dirs[key] && site.files[key+"/index.html"] != nil:
		// Relative links in the page resolve against the slash form.
		h.setHeaders(w.Header())
		w.Header().Set("Location", key+"/")
		if h.cfg.CachePrivate {
			w.Header().Set("Cache-Control", privateNoStore)
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		w.WriteHeader(http.StatusMovedPermanently)
		return
	}
	if e == nil && h.cfg.SPAFallback && !strings.Contains(path.Base(key), ".") {
		e = site.files["/index.html"]
	}
	if e == nil {
		h.notFound(w, r, site)
		return
	}
	h.serve(w, r, e, http.StatusOK)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, site *Site) {
	if h.cfg.NotFoundPage != "" {
		if e := site.files["/"+strings.TrimPrefix(h.cfg.NotFoundPage, "/")]; e != nil {
			h.serve(w, r, e, http.StatusNotFound)
			return
		}
	}
	h.fail(w, http.StatusNotFound, "not found")
}

func (h *Handler) cacheControl(e *entry, status int) string {
	switch {
	case h.cfg.CachePrivate:
		return privateNoStore
	case status != http.StatusOK:
		return "no-store"
	case e.hashed:
		return "public, max-age=31536000, immutable"
	}
	cc := fmt.Sprintf("public, max-age=%d", int(h.cfg.HTMLMaxAge/time.Second))
	if swr := int(h.cfg.StaleWhileRevalidate / time.Second); swr > 0 {
		cc += ", stale-while-revalidate=" + strconv.Itoa(swr)
	}
	return cc
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, e *entry, status int) {
	hd := w.Header()
	h.setHeaders(hd)
	hd.Set("Content-Type", e.ctype)
	hd.Set("Cache-Control", h.cacheControl(e, status))
	if status != http.StatusOK {
		hd.Set("Content-Length", strconv.FormatInt(e.size, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		if e.body != nil {
			_, _ = w.Write(e.body)
		} else if f, err := os.Open(e.disk); err == nil {
			defer func() { _ = f.Close() }()
			_, _ = io.Copy(w, f)
		}
		return
	}
	hd.Set("ETag", e.etag)
	// A zero modtime keeps Last-Modified off: replicas unpack at different
	// instants, and the strong ETag is the same on all of them.
	if e.body != nil {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(e.body))
		return
	}
	f, err := os.Open(e.disk)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "content unavailable")
		return
	}
	defer func() { _ = f.Close() }()
	http.ServeContent(w, r, "", time.Time{}, f)
}
