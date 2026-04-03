package alerter_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
)

func TestFileAlerter_Name(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()
	assert.Equal(t, "file", a.Name())
}

func TestFileAlerter_New_ErrorOnMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "subdir", "alerts.jsonl")
	_, err := alerter.NewFileAlerter(path)
	assert.Error(t, err)
}

func TestFileAlerter_Send_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))

	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "alert file should exist after first Send")
}

func TestFileAlerter_Send_WritesValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var payload map[string]any
	assert.NoError(t, json.Unmarshal(data, &payload), "file content should be valid JSON")
}

func TestFileAlerter_Send_JSONContainsKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindErrorSurge, analyzer.SeverityWarning)))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload))
	assert.Equal(t, "error_surge", payload["kind"])
}

func TestFileAlerter_Send_AppendsMultipleLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindSilence, analyzer.SeverityCritical)))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	lines := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines++
	}
	assert.Equal(t, 2, lines, "each Send should append exactly one line")
}

func TestFileAlerter_Send_EachLineIsValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindSilence, analyzer.SeverityCritical)))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var payload map[string]any
		assert.NoError(t, json.Unmarshal(scanner.Bytes(), &payload), "every line should be valid JSON")
	}
}

func TestFileAlerter_Send_ThreadSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	defer a.Close()

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
		}()
	}
	wg.Wait()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	lines := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var payload map[string]any
		assert.NoError(t, json.Unmarshal(scanner.Bytes(), &payload), "concurrent writes must not corrupt JSON")
		lines++
	}
	assert.Equal(t, goroutines, lines, "all concurrent sends should produce one line each")
}

func TestFileAlerter_Close_NoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.jsonl")
	a, err := alerter.NewFileAlerter(path)
	require.NoError(t, err)
	assert.NoError(t, a.Close())
}
