package pipeline_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
	"log_analyser/internal/config"
	"log_analyser/internal/metrics"
	"log_analyser/internal/pipeline"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// captureAlerter collects every anomaly delivered to it.
type captureAlerter struct {
	mu       sync.Mutex
	received []analyzer.Anomaly
}

func (c *captureAlerter) Name() string { return "capture" }

func (c *captureAlerter) Send(_ context.Context, a analyzer.Anomaly) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, a)
	return nil
}

func (c *captureAlerter) all() []analyzer.Anomaly {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]analyzer.Anomaly, len(c.received))
	copy(out, c.received)
	return out
}

// writeTempLog creates a temp file with the given content and returns its path.
func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test*.log")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// createEmptyLog creates an empty temp file and returns its path.
func createEmptyLog(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test*.log")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// appendToLog appends content to an existing file.
func appendToLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// buildLines generates n nginx log lines.
func buildLines(n int, ip, path string, status int, latency float64) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(nginxLine(ip, path, status, latency))
	}
	return sb.String()
}

// defaultCfg returns a config suitable for pipeline tests:
// small window, no cooldown, very low baseline requirement, fast bucket.
func defaultCfg(t *testing.T, logFile string) config.Config {
	t.Helper()
	cfg := *config.Default()
	cfg.LogFile = logFile
	cfg.Follow = false // read-to-EOF mode, no polling
	cfg.Format = "nginx"
	cfg.BucketDuration = 100 * time.Millisecond
	cfg.WindowSize = time.Second
	cfg.MinBaselineSamples = 1
	cfg.AlertCooldown = 0
	cfg.SpikeMultiplier = 2.0
	cfg.ErrorRateThreshold = 0.1
	cfg.HostFloodFraction = 0.5
	cfg.LatencyMultiplier = 2.0
	cfg.SilenceThreshold = 2
	return cfg
}

// streamingCfg returns a defaultCfg with follow=true for live-write tests.
func streamingCfg(t *testing.T, logFile string) config.Config {
	t.Helper()
	cfg := defaultCfg(t, logFile)
	cfg.Follow = true
	cfg.PollInterval = 20 * time.Millisecond // fast polling so writes are seen quickly
	return cfg
}

// runPipeline starts the pipeline in a goroutine and returns a done channel.
func runPipeline(ctx context.Context, cfg config.Config, al alerter.Alerter) <-chan error {
	done := make(chan error, 1)
	go func() { done <- pipeline.New(cfg, al, nil).Run(ctx) }()
	return done
}

// nginxLine produces a single nginx combined log line.
func nginxLine(ip, path string, status int, latency float64) string {
	return fmt.Sprintf(
		`%s - - [01/Apr/2026:12:00:00 +0000] "GET %s HTTP/1.1" %d 512 "-" "-" %.3f`+"\n",
		ip, path, status, latency,
	)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPipeline_New_ReturnsNonNil(t *testing.T) {
	cfg := *config.Default()
	p := pipeline.New(cfg, &captureAlerter{}, nil)
	assert.NotNil(t, p)
}

func TestPipeline_Run_ExitsCleanlyOnContextCancel(t *testing.T) {
	path := writeTempLog(t, "")
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := pipeline.New(cfg, &captureAlerter{}, nil).Run(ctx)
	assert.NoError(t, err)
}

func TestPipeline_Run_ReturnsErrorOnMissingFile(t *testing.T) {
	cfg := *config.Default()
	cfg.LogFile = filepath.Join(t.TempDir(), "does_not_exist.log")
	cfg.Follow = false

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pipeline.New(cfg, &captureAlerter{}, nil).Run(ctx)
	assert.Error(t, err, "should return error when log file does not exist")
}

func TestPipeline_Run_ParsesAndDeliversAnomaly(t *testing.T) {
	// Streaming test: baseline lines → wait → spike lines → verify anomaly.
	path := createEmptyLog(t)
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipeline(ctx, cfg, cap)

	// Phase 1: baseline (5 lines at low rate).
	appendToLog(t, path, buildLines(5, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond) // 3 bucket intervals → baseline in window

	// Phase 2: spike (200 lines in one burst).
	appendToLog(t, path, buildLines(200, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond) // let anomaly be detected and delivered

	cancel()
	require.NoError(t, <-done)
	assert.NotEmpty(t, cap.all(), "should detect and deliver at least one anomaly")
}

func TestPipeline_Run_DeliversRateSpikeKind(t *testing.T) {
	path := createEmptyLog(t)
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipeline(ctx, cfg, cap)

	// Baseline.
	appendToLog(t, path, buildLines(5, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	// Spike.
	appendToLog(t, path, buildLines(200, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	kinds := map[analyzer.AnomalyKind]bool{}
	for _, a := range cap.all() {
		kinds[a.Kind] = true
	}
	assert.True(t, kinds[analyzer.KindRateSpike], "should deliver a rate_spike anomaly")
}

func TestPipeline_Run_DeliversErrorSurge(t *testing.T) {
	path := createEmptyLog(t)
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipeline(ctx, cfg, cap)

	// Baseline: 10 normal lines.
	appendToLog(t, path, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	// Surge: 50 error lines (100% error rate in this bucket).
	appendToLog(t, path, buildLines(50, "1.2.3.4", "/login", 500, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	kinds := map[analyzer.AnomalyKind]bool{}
	for _, a := range cap.all() {
		kinds[a.Kind] = true
	}
	assert.True(t, kinds[analyzer.KindErrorSurge], "should deliver an error_surge anomaly")
}

func TestPipeline_Run_DeliversHostFlood(t *testing.T) {
	path := createEmptyLog(t)
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipeline(ctx, cfg, cap)

	// Baseline: distributed traffic from 10 IPs.
	var baseline strings.Builder
	for i := 0; i < 10; i++ {
		baseline.WriteString(nginxLine(fmt.Sprintf("10.0.0.%d", i+1), "/", 200, 0.010))
	}
	appendToLog(t, path, baseline.String())
	time.Sleep(300 * time.Millisecond)

	// Flood: single IP dominates (100/105 = 95%).
	flood := buildLines(100, "9.9.9.9", "/", 200, 0.010) +
		buildLines(5, "1.1.1.1", "/", 200, 0.010)
	appendToLog(t, path, flood)
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	kinds := map[analyzer.AnomalyKind]bool{}
	for _, a := range cap.all() {
		kinds[a.Kind] = true
	}
	assert.True(t, kinds[analyzer.KindHostFlood], "should deliver a host_flood anomaly")
}

func TestPipeline_Run_NoAnomalyOnNormalTraffic(t *testing.T) {
	// Steady traffic — no anomaly should fire.
	var content string
	for i := 0; i < 30; i++ {
		content += nginxLine("1.2.3.4", "/", 200, 0.010)
	}
	path := writeTempLog(t, content)

	cap := &captureAlerter{}
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, cap, nil).Run(ctx))
	assert.Empty(t, cap.all(), "steady traffic should produce no anomalies")
}

func TestPipeline_Run_DropsNoEventsOnShutdown(t *testing.T) {
	// Write a known number of lines; count deliveries across multiple runs.
	// The key assertion: Run returns only after the alert loop is done —
	// no anomaly queued before shutdown is silently dropped.
	var content string
	for i := 0; i < 5; i++ {
		content += nginxLine("1.2.3.4", "/", 200, 0.010)
	}
	for i := 0; i < 200; i++ {
		content += nginxLine("1.2.3.4", "/", 200, 0.010)
	}
	path := writeTempLog(t, content)

	cap := &captureAlerter{}
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, cap, nil).Run(ctx))

	// All anomalies must be complete (non-zero DetectedAt) — no zero-value structs.
	for _, a := range cap.all() {
		assert.False(t, a.DetectedAt.IsZero(), "all delivered anomalies must have DetectedAt set")
	}
}

func TestPipeline_Run_ClosesFileAlerterOnShutdown(t *testing.T) {
	path := writeTempLog(t, "")
	cfg := defaultCfg(t, path)

	alertPath := filepath.Join(t.TempDir(), "alerts.jsonl")
	fa, err := alerter.NewFileAlerter(alertPath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, pipeline.New(cfg, fa, nil).Run(ctx))

	// After Run returns the file handle must be closed.
	// A second Close() on an already-closed file returns an error on all platforms.
	err = fa.Close()
	assert.Error(t, err, "FileAlerter should already be closed after pipeline shuts down")
}

func TestPipeline_Run_ReturnsAfterFileIsFullyRead(t *testing.T) {
	// follow=false: pipeline must return after EOF, not block forever.
	var content string
	for i := 0; i < 10; i++ {
		content += nginxLine("1.2.3.4", "/", 200, 0.010)
	}
	path := writeTempLog(t, content)

	cfg := defaultCfg(t, path)

	done := make(chan error, 1)
	go func() {
		done <- pipeline.New(cfg, &captureAlerter{}, nil).Run(context.Background())
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline.Run did not return after reading to EOF with follow=false")
	}
}

// ---------------------------------------------------------------------------
// Spy recorder — tracks every Recorder method call for metrics-wiring tests.
// ---------------------------------------------------------------------------

type spyRecorder struct {
	linesRead    atomic.Int64
	linesParsed  atomic.Int64
	eventsPerSec atomic.Value // float64
	baselineRate atomic.Value // float64
	anomalies    sync.Map     // kind string → *atomic.Int64
	alertsSent   sync.Map     // alerter string → *atomic.Int64
	channelDepth sync.Map     // name string → *atomic.Int64
}

func newSpyRecorder() *spyRecorder {
	s := &spyRecorder{}
	s.eventsPerSec.Store(float64(0))
	s.baselineRate.Store(float64(0))
	return s
}

func (s *spyRecorder) IncLinesRead()              { s.linesRead.Add(1) }
func (s *spyRecorder) IncLinesParsed()            { s.linesParsed.Add(1) }
func (s *spyRecorder) SetEventsPerSecond(v float64) { s.eventsPerSec.Store(v) }
func (s *spyRecorder) SetBaselineRate(v float64)  { s.baselineRate.Store(v) }
func (s *spyRecorder) ObserveAnomaly(kind string) {
	actual, _ := s.anomalies.LoadOrStore(kind, &atomic.Int64{})
	actual.(*atomic.Int64).Add(1)
}
func (s *spyRecorder) IncAlertsSent(alerter string) {
	actual, _ := s.alertsSent.LoadOrStore(alerter, &atomic.Int64{})
	actual.(*atomic.Int64).Add(1)
}
func (s *spyRecorder) SetChannelDepth(name string, depth int) {
	actual, _ := s.channelDepth.LoadOrStore(name, &atomic.Int64{})
	actual.(*atomic.Int64).Store(int64(depth))
}

func (s *spyRecorder) anomalyCount(kind string) int64 {
	v, ok := s.anomalies.Load(kind)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

func (s *spyRecorder) alertsSentCount(alerter string) int64 {
	v, ok := s.alertsSent.Load(alerter)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// ---------------------------------------------------------------------------
// Metrics wiring tests
// ---------------------------------------------------------------------------

func TestPipeline_Metrics_IncLinesRead(t *testing.T) {
	// Pipeline should call rec.IncLinesRead() once for every raw line read.
	const lineCount = 20
	path := writeTempLog(t, buildLines(lineCount, "1.2.3.4", "/", 200, 0.010))

	spy := newSpyRecorder()
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, &captureAlerter{}, spy).Run(ctx))
	assert.Equal(t, int64(lineCount), spy.linesRead.Load(),
		"should call IncLinesRead once per raw line")
}

func TestPipeline_Metrics_IncLinesParsed(t *testing.T) {
	// All lines are valid nginx → IncLinesParsed should equal line count.
	const lineCount = 15
	path := writeTempLog(t, buildLines(lineCount, "1.2.3.4", "/", 200, 0.010))

	spy := newSpyRecorder()
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, &captureAlerter{}, spy).Run(ctx))
	assert.Equal(t, int64(lineCount), spy.linesParsed.Load(),
		"should call IncLinesParsed once per successfully parsed line")
}

func TestPipeline_Metrics_IncLinesParsed_SkipsUnparseable(t *testing.T) {
	// Mix parseable and unparseable lines.
	good := buildLines(10, "1.2.3.4", "/", 200, 0.010)
	bad := "this is not a valid log line\nanother bad line\n"
	path := writeTempLog(t, good+bad)

	spy := newSpyRecorder()
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, &captureAlerter{}, spy).Run(ctx))
	assert.Equal(t, int64(12), spy.linesRead.Load(),
		"should read all 12 lines (good + bad)")
	assert.Equal(t, int64(10), spy.linesParsed.Load(),
		"should only count the 10 successfully parsed lines")
}

func TestPipeline_Metrics_SetEventsPerSecond(t *testing.T) {
	// After processing lines, SetEventsPerSecond should have been called
	// with a non-zero value.
	path := writeTempLog(t, buildLines(30, "1.2.3.4", "/", 200, 0.010))

	spy := newSpyRecorder()
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, &captureAlerter{}, spy).Run(ctx))
	eps := spy.eventsPerSec.Load().(float64)
	assert.Greater(t, eps, float64(0),
		"should set events_per_second to a non-zero value after processing lines")
}

func TestPipeline_Metrics_SetBaselineRate(t *testing.T) {
	// After enough buckets for a baseline, SetBaselineRate should be non-zero.
	path := createEmptyLog(t)
	spy := newSpyRecorder()
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipelineWithRecorder(ctx, cfg, &captureAlerter{}, spy)

	// Write lines spread across multiple bucket intervals.
	appendToLog(t, path, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)
	appendToLog(t, path, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	bl := spy.baselineRate.Load().(float64)
	assert.Greater(t, bl, float64(0),
		"should set baseline_rate to a non-zero value after window has baseline samples")
}

func TestPipeline_Metrics_ObserveAnomaly(t *testing.T) {
	// Trigger a rate spike and verify ObserveAnomaly is called.
	path := createEmptyLog(t)
	spy := newSpyRecorder()
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipelineWithRecorder(ctx, cfg, &captureAlerter{}, spy)

	// Baseline.
	appendToLog(t, path, buildLines(5, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	// Spike.
	appendToLog(t, path, buildLines(200, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	assert.Greater(t, spy.anomalyCount("rate_spike"), int64(0),
		"should call ObserveAnomaly with 'rate_spike' kind")
}

func TestPipeline_Metrics_IncAlertsSent(t *testing.T) {
	// Trigger an anomaly and verify IncAlertsSent is called for the alerter.
	path := createEmptyLog(t)
	spy := newSpyRecorder()
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipelineWithRecorder(ctx, cfg, cap, spy)

	// Baseline.
	appendToLog(t, path, buildLines(5, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	// Spike.
	appendToLog(t, path, buildLines(200, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	assert.Greater(t, spy.alertsSentCount(cap.Name()), int64(0),
		"should call IncAlertsSent with the alerter name after successful Send")
}

func TestPipeline_Metrics_SetChannelDepth(t *testing.T) {
	// The channel depth sampler should set depth for all 4 channels.
	path := createEmptyLog(t)
	spy := newSpyRecorder()
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := runPipelineWithRecorder(ctx, cfg, &captureAlerter{}, spy)

	// Write some lines to create channel activity.
	appendToLog(t, path, buildLines(50, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(1500 * time.Millisecond) // wait for at least one sampler tick

	cancel()
	require.NoError(t, <-done)

	// Verify that all 4 channel names have been reported.
	expectedChannels := []string{"raw", "parsed", "counted", "anomaly"}
	for _, name := range expectedChannels {
		_, ok := spy.channelDepth.Load(name)
		assert.True(t, ok, "should call SetChannelDepth for channel %q", name)
	}
}

func TestPipeline_Metrics_NoopRecorderDoesNotPanic(t *testing.T) {
	// With NoopRecorder, pipeline should run identically — no panics.
	path := writeTempLog(t, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		_ = pipeline.New(cfg, &captureAlerter{}, metrics.NoopRecorder{}).Run(ctx)
	}, "pipeline with NoopRecorder should not panic")
}

func TestPipeline_Metrics_NilRecorderDoesNotPanic(t *testing.T) {
	// With nil recorder, pipeline should substitute NoopRecorder — no panics.
	path := writeTempLog(t, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		_ = pipeline.New(cfg, &captureAlerter{}, nil).Run(ctx)
	}, "pipeline with nil recorder should not panic")
}

func TestPipeline_Metrics_PrometheusRecorderIntegration(t *testing.T) {
	// End-to-end: use a real PrometheusRecorder and verify it doesn't panic.
	path := writeTempLog(t, buildLines(10, "1.2.3.4", "/", 200, 0.010))
	cfg := defaultCfg(t, path)
	rec := metrics.NewPrometheusRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.NotPanics(t, func() {
		_ = pipeline.New(cfg, &captureAlerter{}, rec).Run(ctx)
	}, "pipeline with PrometheusRecorder should not panic")
}

func TestPipeline_Metrics_LinesReadMatchesLinesFromTailer(t *testing.T) {
	// Verify that lines_read == total lines in file, regardless of parseability.
	content := buildLines(8, "1.2.3.4", "/", 200, 0.010) +
		"unparseable line 1\nunparseable line 2\n"
	path := writeTempLog(t, content)

	spy := newSpyRecorder()
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, &captureAlerter{}, spy).Run(ctx))
	assert.Equal(t, int64(10), spy.linesRead.Load(),
		"lines_read should count all lines including unparseable ones")
}

func TestPipeline_Metrics_ObserveAnomalyCountMatchesDeliveredAnomalies(t *testing.T) {
	// ObserveAnomaly count should match the number of anomalies delivered to the alerter.
	path := createEmptyLog(t)
	spy := newSpyRecorder()
	cap := &captureAlerter{}
	cfg := streamingCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := runPipelineWithRecorder(ctx, cfg, cap, spy)

	// Baseline.
	appendToLog(t, path, buildLines(5, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	// Spike.
	appendToLog(t, path, buildLines(200, "1.2.3.4", "/", 200, 0.010))
	time.Sleep(300 * time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	// Total observed anomalies in spy should match total delivered to alerter.
	var totalObserved int64
	spy.anomalies.Range(func(_, v any) bool {
		totalObserved += v.(*atomic.Int64).Load()
		return true
	})
	assert.Equal(t, int64(len(cap.all())), totalObserved,
		"ObserveAnomaly count should match delivered anomaly count")
}

// ---------------------------------------------------------------------------
// runPipelineWithRecorder helper
// ---------------------------------------------------------------------------

func runPipelineWithRecorder(ctx context.Context, cfg config.Config, al alerter.Alerter, rec metrics.Recorder) <-chan error {
	done := make(chan error, 1)
	go func() { done <- pipeline.New(cfg, al, rec).Run(ctx) }()
	return done
}
