package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/metrics"
)

// ---------------------------------------------------------------------------
// Recorder interface tests — NoopRecorder
// ---------------------------------------------------------------------------

func TestNoopRecorder_ImplementsRecorder(t *testing.T) {
	var r metrics.Recorder = metrics.NoopRecorder{}
	assert.NotNil(t, r)
}

func TestNoopRecorder_MethodsDoNotPanic(t *testing.T) {
	var r metrics.NoopRecorder
	assert.NotPanics(t, func() {
		r.IncLinesRead()
		r.IncLinesParsed()
		r.SetEventsPerSecond(42.0)
		r.SetBaselineRate(10.0)
		r.ObserveAnomaly("rate_spike")
		r.IncAlertsSent("console")
		r.SetChannelDepth("raw", 100)
	})
}

// ---------------------------------------------------------------------------
// PrometheusRecorder — construction and interface compliance
// ---------------------------------------------------------------------------

func TestNewPrometheusRecorder_ReturnsNonNil(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	assert.NotNil(t, rec)
}

func TestPrometheusRecorder_ImplementsRecorder(t *testing.T) {
	var r metrics.Recorder = metrics.NewPrometheusRecorder()
	assert.NotNil(t, r)
}

// ---------------------------------------------------------------------------
// PrometheusRecorder — counter and gauge behaviour
// ---------------------------------------------------------------------------

func TestPrometheusRecorder_IncLinesRead_IncrementsCounter(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.IncLinesRead()
	rec.IncLinesRead()

	body := scrapeMetrics(t, rec)
	assertMetricValue(t, body, "log_analyser_lines_read_total", "2")
}

func TestPrometheusRecorder_IncLinesParsed_IncrementsCounter(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.IncLinesParsed()

	body := scrapeMetrics(t, rec)
	assertMetricValue(t, body, "log_analyser_lines_parsed_total", "1")
}

func TestPrometheusRecorder_SetEventsPerSecond_SetsGauge(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.SetEventsPerSecond(123.5)

	body := scrapeMetrics(t, rec)
	assertMetricValue(t, body, "log_analyser_events_per_second", "123.5")
}

func TestPrometheusRecorder_SetBaselineRate_SetsGauge(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.SetBaselineRate(50.0)

	body := scrapeMetrics(t, rec)
	assertMetricValue(t, body, "log_analyser_baseline_rate", "50")
}

func TestPrometheusRecorder_ObserveAnomaly_IncrementsLabeledCounter(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.ObserveAnomaly("rate_spike")
	rec.ObserveAnomaly("rate_spike")
	rec.ObserveAnomaly("error_surge")

	body := scrapeMetrics(t, rec)
	assertMetricLine(t, body, `log_analyser_anomalies_total{kind="rate_spike"}`, "2")
	assertMetricLine(t, body, `log_analyser_anomalies_total{kind="error_surge"}`, "1")
}

func TestPrometheusRecorder_IncAlertsSent_IncrementsLabeledCounter(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.IncAlertsSent("console")
	rec.IncAlertsSent("webhook")
	rec.IncAlertsSent("console")

	body := scrapeMetrics(t, rec)
	assertMetricLine(t, body, `log_analyser_alerts_sent_total{alerter="console"}`, "2")
	assertMetricLine(t, body, `log_analyser_alerts_sent_total{alerter="webhook"}`, "1")
}

func TestPrometheusRecorder_SetChannelDepth_SetsLabeledGauge(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.SetChannelDepth("raw", 512)
	rec.SetChannelDepth("parsed", 256)

	body := scrapeMetrics(t, rec)
	assertMetricLine(t, body, `log_analyser_pipeline_channel_depth{channel="raw"}`, "512")
	assertMetricLine(t, body, `log_analyser_pipeline_channel_depth{channel="parsed"}`, "256")
}

func TestPrometheusRecorder_GaugeOverwrite_UsesLatestValue(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.SetEventsPerSecond(10.0)
	rec.SetEventsPerSecond(99.0)

	body := scrapeMetrics(t, rec)
	assertMetricValue(t, body, "log_analyser_events_per_second", "99")
}

// ---------------------------------------------------------------------------
// PrometheusRecorder — registry isolation
// ---------------------------------------------------------------------------

func TestPrometheusRecorder_IsolatedRegistries(t *testing.T) {
	rec1 := metrics.NewPrometheusRecorder()
	rec2 := metrics.NewPrometheusRecorder()

	rec1.IncLinesRead()
	rec1.IncLinesRead()
	rec1.IncLinesRead()

	rec2.IncLinesRead()

	body1 := scrapeMetrics(t, rec1)
	body2 := scrapeMetrics(t, rec2)

	assertMetricValue(t, body1, "log_analyser_lines_read_total", "3")
	assertMetricValue(t, body2, "log_analyser_lines_read_total", "1")
}

// ---------------------------------------------------------------------------
// Server — construction and /metrics endpoint
// ---------------------------------------------------------------------------

func TestNewServer_ReturnsNonNil(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	srv := metrics.NewServer(":0", rec)
	assert.NotNil(t, srv)
}

func TestServer_MetricsEndpoint_Returns200(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	srv := metrics.NewServer(":0", rec)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestServer_MetricsEndpoint_ContainsPromFormat(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	rec.IncLinesRead()
	srv := metrics.NewServer(":0", rec)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), "log_analyser_lines_read_total")
}

func TestServer_MetricsEndpoint_ContentTypeTextPlain(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	srv := metrics.NewServer(":0", rec)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	assert.True(t, strings.Contains(ct, "text/plain") || strings.Contains(ct, "text/openmetrics"),
		"Content-Type should be prometheus text format, got: %s", ct)
}

func TestServer_Shutdown_ReturnsNil(t *testing.T) {
	rec := metrics.NewPrometheusRecorder()
	srv := metrics.NewServer("127.0.0.1:0", rec)

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)

	err := srv.Shutdown()
	assert.NoError(t, err)

	// ListenAndServe should return http.ErrServerClosed.
	srvErr := <-done
	assert.ErrorIs(t, srvErr, http.ErrServerClosed)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// scrapeMetrics creates a test HTTP server from the recorder's handler,
// performs a GET /metrics, and returns the response body as a string.
func scrapeMetrics(t *testing.T, rec *metrics.PrometheusRecorder) string {
	t.Helper()
	handler := rec.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String()
}

// assertMetricValue checks that a metric line like "metric_name <value>"
// appears in the scraped body (no labels).
func assertMetricValue(t *testing.T, body, metricName, expectedValue string) {
	t.Helper()
	target := metricName + " " + expectedValue
	assert.True(t, strings.Contains(body, target),
		"expected %q in metrics output, got:\n%s", target, body)
}

// assertMetricLine checks that a labeled metric like
// metric_name{label="val"} <value> appears in the scraped body.
func assertMetricLine(t *testing.T, body, metricWithLabels, expectedValue string) {
	t.Helper()
	target := metricWithLabels + " " + expectedValue
	assert.True(t, strings.Contains(body, target),
		"expected %q in metrics output, got:\n%s", target, body)
}
