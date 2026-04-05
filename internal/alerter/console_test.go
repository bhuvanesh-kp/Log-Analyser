package alerter_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
)

func TestConsoleAlerter_Name(t *testing.T) {
	a := alerter.NewConsoleAlerter(&bytes.Buffer{}, false)
	assert.Equal(t, "console", a.Name())
}

func TestConsoleAlerter_Send_ContainsKind(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Contains(t, buf.String(), "rate_spike")
}

func TestConsoleAlerter_Send_ContainsMessage(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindErrorSurge, analyzer.SeverityWarning)))
	assert.Contains(t, buf.String(), "test message")
}

func TestConsoleAlerter_Send_WarningLabel(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Contains(t, buf.String(), "WARNING")
}

func TestConsoleAlerter_Send_CriticalLabel(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindSilence, analyzer.SeverityCritical)))
	assert.Contains(t, buf.String(), "CRITICAL")
}

func TestConsoleAlerter_Send_NoANSIWhenColorFalse(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.NotContains(t, buf.String(), "\033[")
}

func TestConsoleAlerter_Send_ANSIWhenColorTrue(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, true)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Contains(t, buf.String(), "\033[")
}

func TestConsoleAlerter_Send_YellowForWarning(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, true)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.True(t, strings.Contains(buf.String(), "\033[33m"), "warning should use yellow ANSI code")
}

func TestConsoleAlerter_Send_RedForCritical(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, true)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindSilence, analyzer.SeverityCritical)))
	assert.True(t, strings.Contains(buf.String(), "\033[31m"), "critical should use red ANSI code")
}

func TestConsoleAlerter_Send_ResetsANSIAfterLine(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, true)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Contains(t, buf.String(), "\033[0m", "ANSI reset code should appear after each line")
}

func TestNewConsoleAlerterAuto_DisablesColorWhenNOCOLORSet(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerterAuto(&buf)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.NotContains(t, buf.String(), "\033[", "NO_COLOR should disable ANSI output")
}

func TestNewConsoleAlerterAuto_DisablesColorForNonTTYWriter(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer // bytes.Buffer is not an *os.File, so not a TTY
	a := alerter.NewConsoleAlerterAuto(&buf)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.NotContains(t, buf.String(), "\033[", "non-TTY writer should disable ANSI output")
}

func TestNewConsoleAlerterAuto_DisablesColorForRegularFile(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	f, err := os.CreateTemp(t.TempDir(), "console-*.log")
	require.NoError(t, err)
	defer f.Close()
	a := alerter.NewConsoleAlerterAuto(f)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	data, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "\033[", "regular file (not char device) should disable ANSI output")
}

func TestConsoleAlerter_Send_IncludesTimestamp(t *testing.T) {
	var buf bytes.Buffer
	a := alerter.NewConsoleAlerter(&buf, false)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Regexp(t, `\d{2}:\d{2}`, buf.String(), "output should include a HH:MM timestamp")
}
