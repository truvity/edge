package docsmcp

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/truvity/edge/v4/internal/docspack"
)

// Metrics are the server's Prometheus series, on a registry of their own.
type Metrics struct {
	Reg      *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	tools    *prometheus.CounterVec
	loads    *prometheus.CounterVec
	info     *prometheus.GaugeVec
	loadedAt prometheus.Gauge
	docs     prometheus.Gauge
	sections prometheus.Gauge
}

// NewMetrics returns the series on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "docs_mcp_requests_total", Help: "MCP endpoint requests by method and status code."}, []string{"method", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "docs_mcp_request_duration_seconds", Help: "MCP endpoint request duration.",
			Buckets: []float64{.001, .005, .025, .1, .5, 2, 10}}, []string{"method"}),
		tools: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "docs_mcp_tool_calls_total", Help: "Tool calls by tool and result (ok, error)."}, []string{"tool", "result"}),
		loads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "docs_mcp_pack_loads_total", Help: "Pack loads by result."}, []string{"result"}),
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "docs_mcp_pack_info", Help: "1 for the pack digest being served."}, []string{"digest"}),
		loadedAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "docs_mcp_pack_loaded_timestamp_seconds", Help: "When the served pack was loaded."}),
		docs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "docs_mcp_pack_documents", Help: "Documents in the served pack."}),
		sections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "docs_mcp_pack_sections", Help: "Sections in the served pack."}),
	}
	m.Reg.MustRegister(m.requests, m.latency, m.tools, m.loads, m.info, m.loadedAt, m.docs, m.sections,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Loaded records the outcome of a load; st is nil for a failure.
func (m *Metrics) Loaded(digest string, st *docspack.Stats, at time.Time) {
	if st == nil {
		m.loads.WithLabelValues("error").Inc()
		return
	}
	m.loads.WithLabelValues("ok").Inc()
	m.info.Reset()
	m.info.WithLabelValues(digest).Set(1)
	m.loadedAt.Set(float64(at.Unix()))
	m.docs.Set(float64(st.Docs))
	m.sections.Set(float64(st.Sections))
}

// Handler is the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Instrument counts and times every request of next and reports it to log.
func (m *Metrics) Instrument(next http.Handler, log func(r *http.Request, code int, d time.Duration)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		d := time.Since(start)
		m.requests.WithLabelValues(r.Method, strconv.Itoa(sw.code)).Inc()
		m.latency.WithLabelValues(r.Method).Observe(d.Seconds())
		if log != nil {
			log(r, sw.code, d)
		}
	})
}
