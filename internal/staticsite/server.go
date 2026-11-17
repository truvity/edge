package staticsite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// Server wires the handler, the ops endpoints and the load/reload cycle.
type Server struct {
	cfg      *Config
	log      *slog.Logger
	metrics  *Metrics
	handler  *Handler
	draining atomic.Bool
	prevDir  string
	regAuth  *registryAuth
	live     *Live
}

// NewServer returns a server with no content loaded yet.
func NewServer(cfg *Config, log *slog.Logger) *Server {
	m := NewMetrics()
	s := &Server{cfg: cfg, log: log, metrics: m, handler: NewHandler(cfg, m), regAuth: newRegistryAuth(cfg, log)}
	s.live = NewLive(cfg, log, m)
	s.handler.SetLive(s.live)
	return s
}

// Load fetches (or indexes) the content and swaps it in. On failure the
// previous content keeps serving.
func (s *Server) Load(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			s.metrics.Loaded(nil)
		}
	}()
	var (
		root, digest string
		fresh        bool
	)
	if s.cfg.Dir != "" {
		root = s.cfg.Dir
	} else {
		if err := os.MkdirAll(s.cfg.ContentDir, 0o755); err != nil {
			return err
		}
		root, err = os.MkdirTemp(s.cfg.ContentDir, "site-")
		if err != nil {
			return err
		}
		fresh = true
		pctx, cancel := context.WithTimeout(ctx, s.cfg.PullTimeout)
		defer cancel()
		d, perr := pullWith(pctx, s.cfg, root, s.regAuth)
		if perr != nil {
			_ = os.RemoveAll(root)
			return fmt.Errorf("pull: %w", perr)
		}
		digest = d.String()
	}
	site, err := Build(root, s.cfg)
	if err != nil {
		if fresh {
			_ = os.RemoveAll(root)
		}
		return err
	}
	site.Digest = digest
	for _, name := range s.live.Names() {
		if site.files["/"+name] != nil {
			s.log.Warn("the artifact carries a file with the name of a live file; the live file is served", "file", name)
		}
	}
	s.handler.Set(site)
	s.metrics.Loaded(site)
	s.log.Info("content loaded", "digest", digest, "files", len(site.files), "dir", root)
	if fresh {
		// Requests already holding the old snapshot may still be opening its
		// files; let them finish before the directory goes.
		old := s.prevDir
		s.prevDir = root
		if old != "" {
			time.AfterFunc(30*time.Second, func() { _ = os.RemoveAll(old) })
		}
	}
	return nil
}

func (s *Server) requestLog(r *http.Request, code int, d time.Duration) {
	level := slog.LevelDebug
	if code >= 400 {
		level = slog.LevelInfo
	}
	s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms", d.Milliseconds())
}

// OpsHandler serves /healthz, /readyz and /metrics.
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.handler.Site() == nil || s.draining.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("/metrics", s.metrics.Handler())
	return mux
}

// SiteHandler is the instrumented site handler.
func (s *Server) SiteHandler() http.Handler {
	return s.metrics.Instrument(s.handler, s.requestLog)
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
// artifact is still being pulled; readiness waits for content.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ops := newHTTP(s.cfg.OpsAddr, s.OpsHandler())
	site := newHTTP(s.cfg.Addr, s.SiteHandler())
	errc := make(chan error, 2)
	go func() { errc <- ops.ListenAndServe() }()

	if err := s.Load(ctx); err != nil {
		_ = ops.Close()
		return fmt.Errorf("initial load: %w", err)
	}
	if s.live != nil {
		// One pass before the site port opens, so a file that is already
		// there is served from the first request. Readiness does not wait
		// for it: a site is ready without its live files.
		s.live.Refresh()
		go s.live.Run(ctx)
	}
	go func() { errc <- site.ListenAndServe() }()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-hup:
			if err := s.Load(ctx); err != nil {
				s.log.Error("reload failed, keeping the current content", "error", err)
			}
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
			s.log.Info("shutting down")
			s.draining.Store(true)
			// Let the endpoints controller and the CDN see not-ready first.
			time.Sleep(s.cfg.ShutdownDelay)
			sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			err := site.Shutdown(sctx)
			_ = ops.Shutdown(sctx)
			return err
		}
	}
}
