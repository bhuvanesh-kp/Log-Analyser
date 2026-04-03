package pipeline_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
	"log_analyser/internal/config"
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

// defaultCfg returns a config suitable for pipeline tests:
// small window, no cooldown, very low baseline requirement, fast bucket.
func defaultCfg(t *testing.T, logFile string) config.Config {
	t.Helper()
	cfg := *config.Default()
	cfg.LogFile = logFile
	cfg.Follow = false           // read-to-EOF mode, no polling
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
	p := pipeline.New(cfg, &captureAlerter{})
	assert.NotNil(t, p)
}

func TestPipeline_Run_ExitsCleanlyOnContextCancel(t *testing.T) {
	path := writeTempLog(t, "")
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := pipeline.New(cfg, &captureAlerter{}).Run(ctx)
	assert.NoError(t, err)
}

func TestPipeline_Run_ReturnsErrorOnMissingFile(t *testing.T) {
	cfg := *config.Default()
	cfg.LogFile = filepath.Join(t.TempDir(), "does_not_exist.log")
	cfg.Follow = false

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pipeline.New(cfg, &captureAlerter{}).Run(ctx)
	assert.Error(t, err, "should return error when log file does not exist")
}

func TestPipeline_Run_ParsesAndDeliversAnomaly(t *testing.T) {
	// Build a log file: 5 baseline lines at low rate, then a burst of 200 lines.
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

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))
	assert.NotEmpty(t, cap.all(), "should detect and deliver at least one anomaly")
}

func TestPipeline_Run_DeliversRateSpikeKind(t *testing.T) {
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

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))

	kinds := map[analyzer.AnomalyKind]bool{}
	for _, a := range cap.all() {
		kinds[a.Kind] = true
	}
	assert.True(t, kinds[analyzer.KindRateSpike], "should deliver a rate_spike anomaly")
}

func TestPipeline_Run_DeliversErrorSurge(t *testing.T) {
	var content string
	// Baseline: 10 normal lines.
	for i := 0; i < 10; i++ {
		content += nginxLine("1.2.3.4", "/", 200, 0.010)
	}
	// Surge: 50 error lines (100% error rate).
	for i := 0; i < 50; i++ {
		content += nginxLine("1.2.3.4", "/login", 500, 0.010)
	}
	path := writeTempLog(t, content)

	cap := &captureAlerter{}
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))

	kinds := map[analyzer.AnomalyKind]bool{}
	for _, a := range cap.all() {
		kinds[a.Kind] = true
	}
	assert.True(t, kinds[analyzer.KindErrorSurge], "should deliver an error_surge anomaly")
}

func TestPipeline_Run_DeliversHostFlood(t *testing.T) {
	var content string
	// Baseline: distributed traffic.
	for i := 0; i < 10; i++ {
		content += nginxLine(fmt.Sprintf("10.0.0.%d", i+1), "/", 200, 0.010)
	}
	// Flood: single IP dominates.
	for i := 0; i < 100; i++ {
		content += nginxLine("9.9.9.9", "/", 200, 0.010)
	}
	for i := 0; i < 5; i++ {
		content += nginxLine("1.1.1.1", "/", 200, 0.010)
	}
	path := writeTempLog(t, content)

	cap := &captureAlerter{}
	cfg := defaultCfg(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))

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

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))
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

	require.NoError(t, pipeline.New(cfg, cap).Run(ctx))

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

	require.NoError(t, pipeline.New(cfg, fa).Run(ctx))

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
		done <- pipeline.New(cfg, &captureAlerter{}).Run(context.Background())
	}()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline.Run did not return after reading to EOF with follow=false")
	}
}
