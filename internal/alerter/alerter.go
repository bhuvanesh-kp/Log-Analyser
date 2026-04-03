package alerter

import (
	"context"

	"log_analyser/internal/analyzer"
)

// Alerter delivers an anomaly notification to one destination.
type Alerter interface {
	// Name returns a short identifier used in logs and metrics (e.g. "console").
	Name() string
	// Send delivers the anomaly. ctx cancellation must abort in-flight work.
	Send(ctx context.Context, a analyzer.Anomaly) error
}
