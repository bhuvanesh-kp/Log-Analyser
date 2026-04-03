package alerter_test

import (
	"context"
	"sync"
	"time"

	"log_analyser/internal/analyzer"
)

// testAnomaly returns a ready-made Anomaly for use in tests.
func testAnomaly(kind analyzer.AnomalyKind, severity analyzer.SeverityLevel) analyzer.Anomaly {
	return analyzer.Anomaly{
		DetectedAt:    time.Date(2026, 4, 1, 12, 0, 1, 0, time.UTC),
		Kind:          kind,
		Severity:      severity,
		Message:       "test message",
		CurrentValue:  100,
		BaselineValue: 50,
		ThresholdUsed: 75,
		SpikeRatio:    2.0,
	}
}

// mockAlerter is a test double for the Alerter interface.
type mockAlerter struct {
	mu       sync.Mutex
	name     string
	received []analyzer.Anomaly
	ctxs     []context.Context
	err      error // returned by every Send call
}

func newMock(name string) *mockAlerter { return &mockAlerter{name: name} }

func (m *mockAlerter) Name() string { return m.name }

func (m *mockAlerter) Send(ctx context.Context, a analyzer.Anomaly) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = append(m.received, a)
	m.ctxs = append(m.ctxs, ctx)
	return m.err
}

func (m *mockAlerter) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.received)
}

func (m *mockAlerter) last() analyzer.Anomaly {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.received[len(m.received)-1]
}

func (m *mockAlerter) lastCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctxs[len(m.ctxs)-1]
}
