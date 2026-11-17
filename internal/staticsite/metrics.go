package staticsite

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics are the server's Prometheus series, on a registry of their own.
type Metrics struct {
	Reg      *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	loads    *prometheus.CounterVec
	info     *prometheus.GaugeVec
	loadedAt prometheus.Gauge
	files    prometheus.Gauge

	liveReloads  *prometheus.CounterVec
	liveLoadedAt *prometheus.GaugeVec
}

// NewMetrics returns the series on a fresh registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		Reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "static_site_requests_total", Help: "Requests by method and status code."}, []string{"method", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "static_site_request_duration_seconds", Help: "Request duration.",
			Buckets: []float64{.001, .005, .025, .1, .5, 2, 10}}, []string{"method"}),
		loads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "static_site_content_loads_total", Help: "Content loads by result."}, []string{"result"}),
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "static_site_content_info", Help: "1 for the artifact digest being served."}, []string{"digest"}),
		loadedAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "static_site_content_loaded_timestamp_seconds", Help: "When the served content was loaded."}),
		files: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "static_site_content_files", Help: "Files in the served content."}),
		liveReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "static_site_live_reloads_total", Help: "Changed live files read, by file and result (ok, invalid, error)."}, []string{"file", "result"}),
		liveLoadedAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "static_site_live_loaded_timestamp_seconds", Help: "When the served version of a live file was accepted."}, []string{"file"}),
	}
	m.Reg.MustRegister(m.requests, m.latency, m.loads, m.info, m.loadedAt, m.files, m.liveReloads, m.liveLoadedAt,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Loaded records the outcome of a load; s is nil for a failure.
func (m *Metrics) Loaded(s *Site) {
	if s == nil {
		m.loads.WithLabelValues("error").Inc()
		return
	}
	m.loads.WithLabelValues("ok").Inc()
	m.info.Reset()
	m.info.WithLabelValues(s.Digest).Set(1)
	m.loadedAt.Set(float64(s.LoadedAt.Unix()))
	m.files.Set(float64(len(s.files)))
}

// LiveReload counts one read of a live file whose content had changed.
func (m *Metrics) LiveReload(file, result string) {
	m.liveReloads.WithLabelValues(file, result).Inc()
}

// LiveLoaded records when the served version of a live file was accepted.
func (m *Metrics) LiveLoaded(file string, at time.Time) {
	m.liveLoadedAt.WithLabelValues(file).Set(float64(at.Unix()))
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
