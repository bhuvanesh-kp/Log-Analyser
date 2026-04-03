package alerter

import (
	"context"
	"errors"
	"sync"

	"log_analyser/internal/analyzer"
)

// MultiAlerter fans out to all registered Alerter instances concurrently.
// Each Send call launches one goroutine per alerter so that a slow alerter
// (e.g. webhook retry) does not delay fast ones (console, file).
type MultiAlerter struct {
	alerters []Alerter
}

// NewMultiAlerter returns a MultiAlerter that will fan out to all provided alerters.
func NewMultiAlerter(alerters ...Alerter) *MultiAlerter {
	return &MultiAlerter{alerters: alerters}
}

func (m *MultiAlerter) Name() string { return "multi" }

// Send delivers the anomaly to every alerter concurrently. It waits for all
// deliveries to complete before returning. If one or more alerters fail, the
// errors are joined and returned; successful deliveries are not affected.
func (m *MultiAlerter) Send(ctx context.Context, a analyzer.Anomaly) error {
	if len(m.alerters) == 0 {
		return nil
	}

	errs := make([]error, len(m.alerters))
	var wg sync.WaitGroup
	wg.Add(len(m.alerters))

	for i, al := range m.alerters {
		i, al := i, al // capture loop vars
		go func() {
			defer wg.Done()
			errs[i] = al.Send(ctx, a)
		}()
	}
	wg.Wait()

	return errors.Join(errs...)
}
