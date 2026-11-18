package docsmcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truvity/edge/v4/internal/docspack"
	"github.com/truvity/edge/v4/internal/staticsite"
)

const maxRequestBytes = 1 << 20

// Server wires the pack, the MCP handler, the ops endpoints and the load.
type Server struct {
	cfg      *Config
	log      *slog.Logger
	version  string
	metrics  *Metrics
	pack     atomic.Pointer[docspack.Pack]
	draining atomic.Bool
	mcp      atomic.Pointer[mcp.Server]
}

// New returns a server with no pack loaded yet. version is the build's.
func New(cfg *Config, version string, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log, version: version, metrics: NewMetrics()}
	s.mcp.Store(s.newMCP(""))
	return s
}

// newMCP builds the MCP server, its instructions naming the documentation set
// by title when the pack has one. The tools read the pack the Server holds,
// not the one the MCP server was built for.
func (s *Server) newMCP(title string) *mcp.Server {
	text := instructions
	impl := &mcp.Implementation{Name: "docs-mcp", Version: s.version}
	if title != "" {
		text = "Documentation: " + title + ". " + instructions
		impl.Title = title
	}
	srv := mcp.NewServer(impl, &mcp.ServerOptions{Instructions: text})
	s.addTools(srv)
	return srv
}

// Load fetches (or reads) the pack, checks it and swaps it in. On failure the
// previous pack, if any, keeps serving.
func (s *Server) Load(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			s.metrics.Loaded("", nil, time.Time{})
		}
	}()
	dir, digest := s.cfg.Dir, ""
	if dir == "" {
		if err := os.MkdirAll(s.cfg.ContentDir, 0o755); err != nil {
			return err
		}
		dir, err = os.MkdirTemp(s.cfg.ContentDir, "pack-")
		if err != nil {
			return err
		}
		// The pack is in memory once loaded; the unpacked copy goes, so that
		// a memory-backed volume holds it only while it is read.
		defer func() { _ = os.RemoveAll(dir) }()
		sc := s.cfg.PullConfig()
		pctx, cancel := context.WithTimeout(ctx, s.cfg.PullTimeout)
		defer cancel()
		d, perr := staticsite.PullLogged(pctx, &sc, dir, s.log)
		if perr != nil {
			return fmt.Errorf("pull: %w", perr)
		}
		digest = d.String()
	}
	p, err := docspack.Load(dir, docspack.LoadOptions{MaxBytes: s.cfg.MaxBytes})
	if err != nil {
		return err
	}
	s.pack.Store(p)
	s.mcp.Store(s.newMCP(p.Manifest.Title))
	st := p.Stats()
	s.metrics.Loaded(digest, &st, time.Now())
	s.log.Info("pack loaded", "digest", digest, "format_version", p.Manifest.FormatVersion, "documents", st.Docs, "sections", st.Sections,
		"terms", st.Terms, "bytes", st.Bytes)
	return nil
}

// Ready reports whether a pack is loaded and the server is not shutting down.
func (s *Server) Ready() bool { return s.pack.Load() != nil && !s.draining.Load() }

// OpsHandler serves /healthz, /readyz and /metrics.
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("/metrics", s.metrics.Handler())
	return mux
}

// MCPHandler serves the MCP endpoint at /mcp, stateless, answering with JSON.
func (s *Server) MCPHandler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp.Load() }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		Logger:       s.log,
		// The SDK's own guard refuses a non-loopback Host on a loopback
		// listener, which is what every request through the sidecar carries
		// (the public host name). allowHosts below is the guard that knows it.
		DisableLocalhostProtection: true,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		h.ServeHTTP(w, r)
	}))
	return s.metrics.Instrument(allowHosts(s.cfg.AllowedHosts, mux), s.requestLog)
}

func (s *Server) requestLog(r *http.Request, code int, d time.Duration) {
	level := slog.LevelDebug
	if code >= 400 {
		level = slog.LevelInfo
	}
	s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms", d.Milliseconds())
}

// allowHosts answers only requests whose Host header is one of hosts (port
// ignored); with none listed it answers all.
func allowHosts(hosts []string, next http.Handler) http.Handler {
	if len(hosts) == 0 {
		return next
	}
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(hostOnly(h))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(hostOnly(r.Host))] {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly drops the port of a host or host:port, and the brackets of an IPv6
// literal.
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

func newHTTP(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// Run listens on both ports and blocks until SIGINT or SIGTERM (graceful) or
// a listener fails. The ops port comes up first so liveness works while the
// pack is being pulled; the MCP port opens, and readiness turns true, once
// the pack is loaded.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ops := newHTTP(s.cfg.OpsAddr, s.OpsHandler())
	api := newHTTP(s.cfg.Addr, s.MCPHandler())
	errc := make(chan error, 2)
	go func() { errc <- ops.ListenAndServe() }()

	if err := s.Load(ctx); err != nil {
		_ = ops.Close()
		return fmt.Errorf("initial load: %w", err)
	}
	go func() { errc <- api.ListenAndServe() }()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		s.log.Info("shutting down")
		s.draining.Store(true)
		// Let the endpoints controller see not-ready first.
		time.Sleep(s.cfg.ShutdownDelay)
		sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		err := api.Shutdown(sctx)
		_ = ops.Shutdown(sctx)
		return err
	}
}
