package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the process-wide registry. Kept explicit (not the default
// prometheus.DefaultRegisterer) so tests can spin up isolated instances.
type Metrics struct {
	Registry     *prometheus.Registry
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	JobsRunning  prometheus.Gauge
	JobsPending  prometheus.Gauge
	JobsRetries  *prometheus.CounterVec
	JobsDead     *prometheus.CounterVec
}

// NewMetrics creates an isolated process registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "suchi_http_requests_total",
			Help: "Total HTTP requests by method, path template, and status.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "suchi_http_request_duration_seconds",
			Help:    "HTTP handler latency by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		JobsRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "suchi_jobs_running",
			Help: "Current count of jobs in state=running.",
		}),
		JobsPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "suchi_jobs_pending",
			Help: "Current count of jobs in state=pending.",
		}),
		JobsRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "suchi_jobs_retries_total",
			Help: "Jobs that retried after a handler error, by kind.",
		}, []string{"kind"}),
		JobsDead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "suchi_jobs_dead_total",
			Help: "Jobs that hit the attempts cap and were parked dead.",
		}, []string{"kind"}),
	}
	reg.MustRegister(m.HTTPRequests, m.HTTPDuration, m.JobsRunning, m.JobsPending, m.JobsRetries, m.JobsDead)
	return m
}

// Handler exposes /metrics scraping over the process registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}

// HTTPInstrument records bounded ServeMux route patterns.
func (m *Metrics) HTTPInstrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		route := r.Pattern
		if _, path, ok := strings.Cut(route, " "); ok {
			route = path
		}
		if route == "" {
			route = "unmatched"
		}
		m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(sw.status)).Inc()
		m.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}
