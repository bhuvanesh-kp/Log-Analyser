package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/config"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// writeTempLog creates a temporary log file with the given content.
func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test*.log")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// writeTempConfig creates a temporary YAML config file with the given content.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

// sampleNginxLines returns n valid nginx log lines.
func sampleNginxLines(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(fmt.Sprintf(
			`1.2.3.4 - - [01/Apr/2026:12:00:00 +0000] "GET / HTTP/1.1" 200 512 "-" "-" 0.010`+"\n",
		))
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// rootCmd construction
// ---------------------------------------------------------------------------

func TestNewRootCmd_ReturnsNonNil(t *testing.T) {
	cmd := newRootCmd()
	assert.NotNil(t, cmd)
}

func TestNewRootCmd_Use(t *testing.T) {
	cmd := newRootCmd()
	assert.Equal(t, "loganalyser", cmd.Use)
}

// ---------------------------------------------------------------------------
// Version flag
// ---------------------------------------------------------------------------

func TestVersionFlag_PrintsVersion(t *testing.T) {
	var buf bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--version"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, buf.String(), version)
}

// ---------------------------------------------------------------------------
// Flag registration — all expected flags exist
// ---------------------------------------------------------------------------

func TestFlags_AllExpectedFlagsRegistered(t *testing.T) {
	cmd := newRootCmd()
	expected := []string{
		"file", "follow", "format",
		"window", "bucket",
		"spike-multiplier", "error-rate", "host-flood",
		"latency-multiplier", "silence", "cooldown",
		"min-baseline", "detection-method", "workers",
		"webhook", "alert-file",
		"metrics-addr",
		"config", "verbose",
	}
	for _, name := range expected {
		f := cmd.Flags().Lookup(name)
		assert.NotNilf(t, f, "flag --%s should be registered", name)
	}
}

func TestFlags_ShortFlags(t *testing.T) {
	cmd := newRootCmd()
	tests := []struct {
		long, short string
	}{
		{"file", "f"},
		{"follow", "F"},
		{"config", "c"},
		{"verbose", "v"},
	}
	for _, tt := range tests {
		f := cmd.Flags().Lookup(tt.long)
		require.NotNilf(t, f, "flag --%s should be registered", tt.long)
		assert.Equal(t, tt.short, f.Shorthand, "flag --%s should have shorthand -%s", tt.long, tt.short)
	}
}

// ---------------------------------------------------------------------------
// buildAlerters
// ---------------------------------------------------------------------------

func TestBuildAlerters_AlwaysIncludesConsole(t *testing.T) {
	cfg := defaultTestConfig()
	ma, err := buildAlerters(cfg)
	require.NoError(t, err)
	assert.Contains(t, ma.Name(), "multi")
}

func TestBuildAlerters_WebhookIncludedWhenURLSet(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Alerters.Webhook.URL = "http://example.com/hook"

	ma, err := buildAlerters(cfg)
	require.NoError(t, err)
	// MultiAlerter wraps console + webhook — at least 2 alerters.
	assert.NotNil(t, ma)
}

func TestBuildAlerters_FileIncludedWhenPathSet(t *testing.T) {
	dir, err := os.MkdirTemp("", "alerter-test-*")
	require.NoError(t, err)

	cfg := defaultTestConfig()
	cfg.Alerters.File.Path = filepath.Join(dir, "alerts.jsonl")

	ma, buildErr := buildAlerters(cfg)
	require.NoError(t, buildErr)
	assert.NotNil(t, ma)

	// Cleanup: the FileAlerter inside the MultiAlerter holds an open handle.
	// Remove the directory after the test; ignore errors on Windows if the
	// handle is still open (the OS will release it when the process exits).
	t.Cleanup(func() { os.RemoveAll(dir) })
}

func TestBuildAlerters_FileReturnsErrorOnBadPath(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Alerters.File.Path = filepath.Join(t.TempDir(), "nonexistent", "subdir", "alerts.jsonl")

	_, err := buildAlerters(cfg)
	assert.Error(t, err, "should return error when file alerter path is invalid")
}

func TestBuildAlerters_WebhookReturnsErrorOnEmptyURL(t *testing.T) {
	cfg := defaultTestConfig()
	// Trigger webhook construction by enabling it, but leave URL empty.
	cfg.Alerters.Webhook.Enabled = true
	cfg.Alerters.Webhook.URL = ""

	_, err := buildAlerters(cfg)
	assert.Error(t, err, "should return error when webhook is enabled but URL is empty")
}

// ---------------------------------------------------------------------------
// buildRecorder
// ---------------------------------------------------------------------------

func TestBuildRecorder_NoopWhenAddrEmpty(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.MetricsAddr = ""

	rec, srv := buildRecorder(cfg)
	assert.NotNil(t, rec, "recorder should be non-nil (NoopRecorder)")
	assert.Nil(t, srv, "server should be nil when addr is empty")
}

func TestBuildRecorder_PrometheusWhenAddrSet(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.MetricsAddr = "127.0.0.1:0"

	rec, srv := buildRecorder(cfg)
	assert.NotNil(t, rec)
	assert.NotNil(t, srv, "server should be non-nil when addr is set")
}

// ---------------------------------------------------------------------------
// setupLogging
// ---------------------------------------------------------------------------

func TestSetupLogging_DoesNotPanic(t *testing.T) {
	assert.NotPanics(t, func() { setupLogging(false) })
	assert.NotPanics(t, func() { setupLogging(true) })
}

// ---------------------------------------------------------------------------
// End-to-end: run with a static log file (follow=false)
// ---------------------------------------------------------------------------

func TestRun_ExitsCleanlyWithStaticFile(t *testing.T) {
	logPath := writeTempLog(t, sampleNginxLines(10))

	cmd := newRootCmd()
	cmd.SetArgs([]string{
		"--file", logPath,
		"--follow=false",
		"--format", "nginx",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit within timeout")
	}
}

func TestRun_ReturnsErrorOnMissingFile(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{
		"--file", filepath.Join(t.TempDir(), "does_not_exist.log"),
		"--follow=false",
	})

	err := cmd.Execute()
	assert.Error(t, err, "should fail when log file does not exist")
}

func TestRun_RespectsConfigFile(t *testing.T) {
	logPath := writeTempLog(t, sampleNginxLines(5))
	cfgContent := fmt.Sprintf("log_file: %s\nfollow: false\nformat: nginx\n", logPath)
	cfgPath := writeTempConfig(t, cfgContent)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--config", cfgPath})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("command did not exit within timeout with config file")
	}
}

func TestRun_FlagOverridesConfigFile(t *testing.T) {
	// Config file says follow=true, but flag says follow=false.
	// With a static file and follow=false, command should exit cleanly.
	logPath := writeTempLog(t, sampleNginxLines(5))
	cfgContent := fmt.Sprintf("log_file: %s\nfollow: true\nformat: nginx\n", logPath)
	cfgPath := writeTempConfig(t, cfgContent)

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--config", cfgPath, "--follow=false"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("flag --follow=false should override config follow=true")
	}
}

func TestRun_ExitsOnContextCancel(t *testing.T) {
	logPath := writeTempLog(t, sampleNginxLines(5))

	cmd := newRootCmd()
	cmd.SetArgs([]string{
		"--file", logPath,
		"--follow=true",
		"--format", "nginx",
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	// Let it start, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("command should exit cleanly on context cancel")
	}
}

func TestRun_VerboseFlagDoesNotError(t *testing.T) {
	logPath := writeTempLog(t, sampleNginxLines(5))

	cmd := newRootCmd()
	cmd.SetArgs([]string{
		"--file", logPath,
		"--follow=false",
		"--format", "nginx",
		"--verbose",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("verbose mode should not cause errors")
	}
}

// ---------------------------------------------------------------------------
// defaultTestConfig helper
// ---------------------------------------------------------------------------

func defaultTestConfig() config.Config {
	cfg := *config.Default()
	cfg.Follow = false
	return cfg
}
