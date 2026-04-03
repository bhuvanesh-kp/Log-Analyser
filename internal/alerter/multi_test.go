package alerter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
)

func TestMultiAlerter_Name(t *testing.T) {
	ma := alerter.NewMultiAlerter()
	assert.Equal(t, "multi", ma.Name())
}

func TestMultiAlerter_Send_CallsAllAlerters(t *testing.T) {
	m1 := newMock("m1")
	m2 := newMock("m2")
	ma := alerter.NewMultiAlerter(m1, m2)

	require.NoError(t, ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))

	assert.Equal(t, 1, m1.calls(), "m1 should be called once")
	assert.Equal(t, 1, m2.calls(), "m2 should be called once")
}

func TestMultiAlerter_Send_PassesSameAnomaly(t *testing.T) {
	m1 := newMock("m1")
	m2 := newMock("m2")
	ma := alerter.NewMultiAlerter(m1, m2)

	anom := testAnomaly(analyzer.KindSilence, analyzer.SeverityCritical)
	require.NoError(t, ma.Send(context.Background(), anom))

	assert.Equal(t, anom, m1.last(), "m1 should receive the exact anomaly")
	assert.Equal(t, anom, m2.last(), "m2 should receive the exact anomaly")
}

func TestMultiAlerter_Send_ContinuesOnOneFailure(t *testing.T) {
	failing := newMock("failing")
	failing.err = errors.New("delivery failed")
	succeeding := newMock("ok")

	ma := alerter.NewMultiAlerter(failing, succeeding)
	_ = ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))

	assert.Equal(t, 1, failing.calls())
	assert.Equal(t, 1, succeeding.calls(), "succeeding alerter should still be called even if another fails")
}

func TestMultiAlerter_Send_ReturnsErrorIfAnyFails(t *testing.T) {
	failing := newMock("failing")
	failing.err = errors.New("delivery failed")
	succeeding := newMock("ok")

	ma := alerter.NewMultiAlerter(failing, succeeding)
	err := ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	assert.Error(t, err)
}

func TestMultiAlerter_Send_ReturnsNilIfAllSucceed(t *testing.T) {
	m1 := newMock("m1")
	m2 := newMock("m2")
	ma := alerter.NewMultiAlerter(m1, m2)

	err := ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	assert.NoError(t, err)
}

func TestMultiAlerter_Send_EmptyAlerters(t *testing.T) {
	ma := alerter.NewMultiAlerter()
	err := ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	assert.NoError(t, err)
}

func TestMultiAlerter_Send_PropagatesContext(t *testing.T) {
	type ctxKey struct{}
	m1 := newMock("m1")
	ma := alerter.NewMultiAlerter(m1)

	ctx := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	require.NoError(t, ma.Send(ctx, testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))

	assert.Equal(t, "sentinel", m1.lastCtx().Value(ctxKey{}), "context value should be propagated to child alerters")
}

func TestMultiAlerter_Send_CollectsAllErrors(t *testing.T) {
	f1 := newMock("f1")
	f1.err = errors.New("error one")
	f2 := newMock("f2")
	f2.err = errors.New("error two")

	ma := alerter.NewMultiAlerter(f1, f2)
	err := ma.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "error one")
	assert.Contains(t, err.Error(), "error two")
}
