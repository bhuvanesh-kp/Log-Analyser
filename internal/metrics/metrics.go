package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "log_analyser"

// Recorder is the instrumentation interface used by pipeline stages.
// All methods are safe to call concurrently.
type Recorder interface {
	IncLinesRead()
	IncLinesParsed()
	SetEventsPerSecond(v float64)
	SetBaselineRate(v float64)
	ObserveAnomaly(kind string)
	IncAlertsSent(alerter string)
	SetChannelDepth(name string, depth int)
}

// ---------------------------------------------------------------------------
// NoopRecorder — zero-cost implementation when metrics are disabled.
// ---------------------------------------------------------------------------

// NoopRecorder satisfies Recorder with no-op methods.
// Use when --metrics-addr is not set.
type NoopRecorder struct{}

func (NoopRecorder) IncLinesRead()                     {}
func (NoopRecorder) IncLinesParsed()                   {}
func (NoopRecorder) SetEventsPerSecond(float64)        {}
func (NoopRecorder) SetBaselineRate(float64)           {}
func (NoopRecorder) ObserveAnomaly(string)             {}
func (NoopRecorder) IncAlertsSent(string)              {}
func (NoopRecorder) SetChannelDepth(string, int)       {}

// ---------------------------------------------------------------------------
// PrometheusRecorder — real implementation backed by a custom registry.
// ---------------------------------------------------------------------------

// PrometheusRecorder registers and updates Prometheus metrics on a private
// registry. Each instance is fully isolated — safe for parallel tests.
type PrometheusRecorder struct {
	registry *prometheus.Registry

	linesRead    prometheus.Counter
	linesParsed  prometheus.Counter
	eventsPerSec prometheus.Gauge
	baselineRate prometheus.Gauge
	anomalies    *prometheus.CounterVec
	alertsSent   *prometheus.CounterVec
	channelDepth *prometheus.GaugeVec
}

// NewPrometheusRecorder creates a PrometheusRecorder with all collectors
// registered on a fresh, private registry.
func NewPrometheusRecorder() *PrometheusRecorder {
	reg := prometheus.NewRegistry()

	linesRead := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "lines_read_total",
		Help:      "Total number of raw lines read by the tailer.",
	})
	linesParsed := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "lines_parsed_total",
		Help:      "Total number of lines successfully parsed.",
	})
	eventsPerSec := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "events_per_second",
		Help:      "Current event rate from the latest counter bucket.",
	})
	baselineRate := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "baseline_rate",
		Help:      "Sliding-window mean event rate used as baseline.",
	})
	anomalies := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "anomalies_total",
		Help:      "Total anomalies detected, labeled by kind.",
	}, []string{"kind"})
	alertsSent := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "alerts_sent_total",
		Help:      "Total alerts delivered, labeled by alerter name.",
	}, []string{"alerter"})
	channelDepth := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "pipeline_channel_depth",
		Help:      "Current buffered items in each pipeline channel.",
	}, []string{"channel"})

	reg.MustRegister(linesRead, linesParsed, eventsPerSec, baselineRate,
		anomalies, alertsSent, channelDepth)

	return &PrometheusRecorder{
		registry:     reg,
		linesRead:    linesRead,
		linesParsed:  linesParsed,
		eventsPerSec: eventsPerSec,
		baselineRate: baselineRate,
		anomalies:    anomalies,
		alertsSent:   alertsSent,
		channelDepth: channelDepth,
	}
}

func (r *PrometheusRecorder) IncLinesRead()              { r.linesRead.Inc() }
func (r *PrometheusRecorder) IncLinesParsed()            { r.linesParsed.Inc() }
func (r *PrometheusRecorder) SetEventsPerSecond(v float64) { r.eventsPerSec.Set(v) }
func (r *PrometheusRecorder) SetBaselineRate(v float64)  { r.baselineRate.Set(v) }
func (r *PrometheusRecorder) ObserveAnomaly(kind string) { r.anomalies.WithLabelValues(kind).Inc() }
func (r *PrometheusRecorder) IncAlertsSent(alerter string) {
	r.alertsSent.WithLabelValues(alerter).Inc()
}
func (r *PrometheusRecorder) SetChannelDepth(name string, depth int) {
	r.channelDepth.WithLabelValues(name).Set(float64(depth))
}

// Handler returns an http.Handler that serves the /metrics endpoint
// using this recorder's private registry.
func (r *PrometheusRecorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

// ---------------------------------------------------------------------------
// Server — HTTP server exposing /metrics.
// ---------------------------------------------------------------------------

// Server wraps an http.Server that exposes Prometheus metrics.
type Server struct {
	http *http.Server
}

// NewServer creates a Server that listens on addr and serves /metrics
// from the given PrometheusRecorder's private registry.
func NewServer(addr string, rec *PrometheusRecorder) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", rec.Handler())

	return &Server{
		http: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

// Handler returns the server's root HTTP handler (useful for httptest).
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// ListenAndServe starts listening and blocks until the server is shut down.
// Returns http.ErrServerClosed on graceful shutdown.
func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

// Shutdown gracefully stops the server, draining in-flight requests.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.http.Shutdown(ctx)
}
