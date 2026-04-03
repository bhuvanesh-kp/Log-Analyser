package pipeline

import (
	"context"

	"log_analyser/internal/alerter"
	"log_analyser/internal/config"
)

// Pipeline wires all stages together and manages goroutine lifecycle.
type Pipeline struct {
	cfg     config.Config
	alerter alerter.Alerter
}

// New constructs a Pipeline from cfg. alerter is the delivery target for
// anomalies (typically a MultiAlerter wrapping console/webhook/file alerters).
func New(cfg config.Config, al alerter.Alerter) *Pipeline {
	return &Pipeline{cfg: cfg, alerter: al}
}

// Run starts all pipeline goroutines and blocks until the pipeline has fully
// shut down. Shutdown is triggered by ctx cancellation (SIGINT/SIGTERM in
// production). Returns nil on clean shutdown or an error if a stage fails to
// start (e.g. tailer cannot open the log file).
func (p *Pipeline) Run(ctx context.Context) error {
	panic("not implemented")
}
