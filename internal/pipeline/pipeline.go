package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
	"log_analyser/internal/config"
	"log_analyser/internal/counter"
	"log_analyser/internal/parser"
	"log_analyser/internal/tailer"
	"log_analyser/internal/window"
)

// Pipeline owns all inter-stage channels and goroutines.
// Create with New, start with Run.
type Pipeline struct {
	cfg     config.Config
	alerter alerter.Alerter
}

// New constructs a Pipeline from cfg.
// al is the delivery target for anomalies — typically a MultiAlerter.
// No goroutines are started until Run is called.
func New(cfg config.Config, al alerter.Alerter) *Pipeline {
	return &Pipeline{cfg: cfg, alerter: al}
}

// Run starts all pipeline goroutines and blocks until shutdown is complete.
//
// Shutdown is triggered by ctx cancellation (SIGINT/SIGTERM in production) or
// by the tailer reaching EOF with follow=false.
//
// Ordered drain guarantee:
//
//	ctx.Cancel / EOF
//	  → tailer.Run returns         → close(rawC)
//	  → parser pool drains rawC    → close(evtC), ctrCancel()
//	  → counter drains evtC        → close(cntC)
//	  → analyzer drains cntC       → close(anomC)
//	  → alert loop drains anomC    → Run returns
//
// Returns nil on clean shutdown, or an error if the log file cannot be opened.
func (p *Pipeline) Run(ctx context.Context) error {
	// Preflight: verify the log file is accessible before spawning goroutines.
	if p.cfg.LogFile != "" {
		f, err := os.Open(p.cfg.LogFile)
		if err != nil {
			return fmt.Errorf("pipeline: open log file: %w", err)
		}
		f.Close()
	}

	// -----------------------------------------------------------------------
	// Channels — owned exclusively by the pipeline.
	// -----------------------------------------------------------------------
	rawC  := make(chan tailer.RawLine, 1024)
	evtC  := make(chan parser.ParsedEvent, 512)
	cntC  := make(chan counter.EventCount, 64)
	anomC := make(chan analyzer.Anomaly, 32)

	// -----------------------------------------------------------------------
	// Stage construction.
	// -----------------------------------------------------------------------
	workers := p.cfg.ParserWorkers
	if workers < 1 {
		workers = runtime.NumCPU()
	}

	tail  := tailer.New(p.cfg.LogFile, p.cfg.Follow, p.cfg.PollInterval)
	pool  := parser.NewPool(p.cfg.Format, workers)
	ctr   := counter.New(p.cfg.BucketDuration)
	win   := window.New(int(p.cfg.WindowSize / p.cfg.BucketDuration))
	anlzr := analyzer.New(p.cfg, win)

	// ctrCtx is cancelled either when the parent ctx is cancelled (SIGTERM)
	// or when the parser pool exits (natural EOF with follow=false).
	// This drives the counter out of its ticker loop when there is no more
	// input to process.
	ctrCtx, ctrCancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	// -----------------------------------------------------------------------
	// Stage 1 — Tailer → rawC
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(rawC)
		tail.Run(ctx, rawC)
	}()

	// -----------------------------------------------------------------------
	// Stage 2 — Parser pool: rawC → evtC
	// Closing evtC signals the counter that the source is exhausted.
	// Calling ctrCancel() after the pool exits wakes the counter's ctx.Done
	// branch so it flushes the final partial bucket and returns.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(evtC)
		defer ctrCancel() // wake counter when pool is done
		pool.Run(ctx, rawC, evtC)
	}()

	// -----------------------------------------------------------------------
	// Stage 3 — Counter: evtC → cntC
	// Uses ctrCtx so it exits both on parent cancellation and on pool-done.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(cntC)
		defer ctrCancel() // idempotent — safe to call more than once
		ctr.Run(ctrCtx, evtC, cntC)
	}()

	// -----------------------------------------------------------------------
	// Stage 4 — Analyzer: cntC → anomC
	// Uses context.Background() so it drains cntC fully before exiting.
	// The counter wrapper always closes cntC, so the analyzer will exit via
	// the !ok branch — no goroutine leak is possible.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(anomC)
		anlzr.Run(context.Background(), cntC, anomC)
	}()

	// -----------------------------------------------------------------------
	// Stage 5 — Alert loop: drains anomC until closed.
	// `range anomC` guarantees every queued anomaly is delivered before this
	// goroutine exits, which means Run only returns after all alerts are sent.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		for a := range anomC {
			if err := p.alerter.Send(ctx, a); err != nil {
				slog.Error("alert delivery failed",
					"alerter", p.alerter.Name(),
					"kind", a.Kind,
					"err", err)
			}
		}
	}()

	// Block until every goroutine has exited.
	wg.Wait()

	// Release any resources held by alerters (e.g. FileAlerter's file handle).
	if c, ok := p.alerter.(io.Closer); ok {
		if err := c.Close(); err != nil {
			slog.Warn("alerter close error", "alerter", p.alerter.Name(), "err", err)
		}
	}

	return nil
}
