package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
	"log_analyser/internal/config"
	"log_analyser/internal/counter"
	"log_analyser/internal/metrics"
	"log_analyser/internal/parser"
	"log_analyser/internal/tailer"
	"log_analyser/internal/window"
)

// Pipeline owns all inter-stage channels and goroutines.
// Create with New, start with Run.
type Pipeline struct {
	cfg      config.Config
	alerter  alerter.Alerter
	recorder metrics.Recorder
}

// New constructs a Pipeline from cfg.
// al is the delivery target for anomalies — typically a MultiAlerter.
// rec is the metrics recorder — use metrics.NoopRecorder{} when metrics are disabled.
// No goroutines are started until Run is called.
func New(cfg config.Config, al alerter.Alerter, rec metrics.Recorder) *Pipeline {
	if rec == nil {
		rec = metrics.NoopRecorder{}
	}
	return &Pipeline{cfg: cfg, alerter: al, recorder: rec}
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

	rec := p.recorder

	// -----------------------------------------------------------------------
	// Channels — owned exclusively by the pipeline.
	// -----------------------------------------------------------------------
	//
	// Instrumented channels sit between stages. Forwarding goroutines read
	// from the instrumented channel, call the appropriate Recorder method,
	// and forward the item to the real stage input channel.
	//
	//   Tailer → instrRawC → [fwd: IncLinesRead] → rawC → Parser Pool
	//          → instrEvtC → [fwd: IncLinesParsed] → evtC → Counter
	//          → instrCntC → [fwd: SetEventsPerSecond, SetBaselineRate] → cntC → Analyzer
	//          → anomC → [alert loop: ObserveAnomaly, IncAlertsSent]
	//
	instrRawC := make(chan tailer.RawLine, 1024)
	rawC      := make(chan tailer.RawLine, 1024)
	instrEvtC := make(chan parser.ParsedEvent, 512)
	evtC      := make(chan parser.ParsedEvent, 512)
	instrCntC := make(chan counter.EventCount, 64)
	cntC      := make(chan counter.EventCount, 64)
	anomC     := make(chan analyzer.Anomaly, 32)

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
	// Stage 1 — Tailer → instrRawC
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(instrRawC)
		tail.Run(ctx, instrRawC)
	}()

	// -----------------------------------------------------------------------
	// Forwarding: instrRawC → rawC (IncLinesRead)
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(rawC)
		for r := range instrRawC {
			rec.IncLinesRead()
			rawC <- r
		}
	}()

	// -----------------------------------------------------------------------
	// Stage 2 — Parser pool: rawC → instrEvtC
	// Closing instrEvtC signals downstream that the source is exhausted.
	// Calling ctrCancel() after the pool exits wakes the counter's ctx.Done
	// branch so it flushes the final partial bucket and returns.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(instrEvtC)
		defer ctrCancel() // wake counter when pool is done
		pool.Run(ctx, rawC, instrEvtC)
	}()

	// -----------------------------------------------------------------------
	// Forwarding: instrEvtC → evtC (IncLinesParsed)
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(evtC)
		for e := range instrEvtC {
			rec.IncLinesParsed()
			evtC <- e
		}
	}()

	// -----------------------------------------------------------------------
	// Stage 3 — Counter: evtC → instrCntC
	// Uses ctrCtx so it exits both on parent cancellation and on pool-done.
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(instrCntC)
		defer ctrCancel() // idempotent — safe to call more than once
		ctr.Run(ctrCtx, evtC, instrCntC)
	}()

	// -----------------------------------------------------------------------
	// Forwarding: instrCntC → cntC (SetEventsPerSecond, SetBaselineRate)
	// The window is thread-safe (RWMutex), so Snapshot() here is safe while
	// the analyzer goroutine calls Push().
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(cntC)
		for ec := range instrCntC {
			rec.SetEventsPerSecond(float64(ec.Total))
			snap := win.Snapshot()
			if len(snap) > 0 {
				rec.SetBaselineRate(window.Mean(snap))
			}
			cntC <- ec
		}
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
	// ObserveAnomaly is called for every anomaly; IncAlertsSent only on
	// successful delivery.
	// samplerDone is closed after the alert loop exits, signalling the channel
	// depth sampler to stop — this prevents the sampler from blocking Run in
	// follow=false mode where ctx is never cancelled.
	// -----------------------------------------------------------------------
	samplerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(samplerDone)
		for a := range anomC {
			rec.ObserveAnomaly(string(a.Kind))
			if err := p.alerter.Send(ctx, a); err != nil {
				slog.Error("alert delivery failed",
					"alerter", p.alerter.Name(),
					"kind", a.Kind,
					"err", err)
			} else {
				rec.IncAlertsSent(p.alerter.Name())
			}
		}
	}()

	// -----------------------------------------------------------------------
	// Channel depth sampler — samples len() of all 4 stage channels once per
	// second and reports via rec.SetChannelDepth. Exits on ctx cancellation
	// or when the alert loop completes (samplerDone closed).
	// -----------------------------------------------------------------------
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-samplerDone:
				return
			case <-ticker.C:
				rec.SetChannelDepth("raw", len(rawC))
				rec.SetChannelDepth("parsed", len(evtC))
				rec.SetChannelDepth("counted", len(cntC))
				rec.SetChannelDepth("anomaly", len(anomC))
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
