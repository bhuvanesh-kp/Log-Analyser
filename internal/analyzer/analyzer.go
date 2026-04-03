package analyzer

import (
	"context"
	"fmt"
	"time"

	"log_analyser/internal/config"
	"log_analyser/internal/counter"
	"log_analyser/internal/window"
)

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// AnomalyKind identifies which detection rule fired.
type AnomalyKind string

const (
	KindRateSpike    AnomalyKind = "rate_spike"
	KindErrorSurge   AnomalyKind = "error_surge"
	KindLatencySpike AnomalyKind = "latency_spike"
	KindHostFlood    AnomalyKind = "host_flood"
	KindSilence      AnomalyKind = "silence"
)

// SeverityLevel classifies how serious an anomaly is.
type SeverityLevel string

const (
	SeverityWarning  SeverityLevel = "warning"
	SeverityCritical SeverityLevel = "critical"
)

// Anomaly is emitted by the Analyzer when a detection rule fires.
type Anomaly struct {
	DetectedAt    time.Time     `json:"detected_at"`
	Kind          AnomalyKind   `json:"kind"`
	Severity      SeverityLevel `json:"severity"`
	Message       string        `json:"message"`
	CurrentValue  float64       `json:"current_value"`  // observed metric value
	BaselineValue float64       `json:"baseline_value"` // window mean / baseline
	ThresholdUsed float64       `json:"threshold_used"` // configured threshold that was crossed
	SpikeRatio    float64       `json:"spike_ratio"`    // CurrentValue / BaselineValue (0 if baseline is 0)
	OffendingHost string        `json:"offending_host,omitempty"` // set only for host_flood
}

// ---------------------------------------------------------------------------
// Analyzer
// ---------------------------------------------------------------------------

// Analyzer receives EventCount buckets, maintains the sliding window, evaluates
// detection rules, and emits Anomaly values.
type Analyzer struct {
	cfg      config.Config
	window   *window.Window
	cooldown map[AnomalyKind]time.Time // last-fired timestamp per kind
}

// New returns an Analyzer configured with cfg and backed by win.
func New(cfg config.Config, win *window.Window) *Analyzer {
	return &Analyzer{
		cfg:      cfg,
		window:   win,
		cooldown: make(map[AnomalyKind]time.Time),
	}
}

// Run reads EventCount values from in, pushes each into the sliding window,
// evaluates all detection rules, and sends any fired Anomaly values to out.
// Blocks until in is closed or ctx is cancelled.
// Run does NOT close out — the caller owns the channel.
func (a *Analyzer) Run(ctx context.Context, in <-chan counter.EventCount, out chan<- Anomaly) {
	for {
		select {
		case <-ctx.Done():
			return
		case ec, ok := <-in:
			if !ok {
				return
			}
			a.evaluate(ec, out) // baseline from historical window, before current bucket
			a.window.Push(ec)
		}
	}
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

func (a *Analyzer) evaluate(current counter.EventCount, out chan<- Anomaly) {
	snap := a.window.Snapshot()
	if len(snap) < a.cfg.MinBaselineSamples {
		return // window not warm yet
	}

	mean := window.Mean(snap)
	var threshold float64

	if a.cfg.DetectionMethod == "sigma" {
		stddev := window.StdDev(snap)
		threshold = mean + a.cfg.SpikeMultiplier*stddev
	} else {
		threshold = mean * a.cfg.SpikeMultiplier
	}

	a.checkRate(current, mean, threshold, out)
	a.checkError(current, out)
	a.checkLatency(current, snap, out)
	a.checkHostFlood(current, out)
	a.checkSilence(snap, out)
}

// ---------------------------------------------------------------------------
// Detection rules
// ---------------------------------------------------------------------------

func (a *Analyzer) checkRate(current counter.EventCount, mean, threshold float64, out chan<- Anomaly) {
	cur := float64(current.Total)
	if cur <= threshold {
		return
	}

	ratio := float64(0)
	if mean > 0 {
		ratio = cur / mean
	}

	severity := SeverityWarning
	if ratio >= 10 {
		severity = SeverityCritical
	}

	a.emit(out, Anomaly{
		Kind:          KindRateSpike,
		Severity:      severity,
		CurrentValue:  cur,
		BaselineValue: mean,
		ThresholdUsed: threshold,
		SpikeRatio:    ratio,
		Message:       fmt.Sprintf("%.0f req/sec (%.1f× baseline of %.0f req/sec)", cur, ratio, mean),
	})
}

func (a *Analyzer) checkError(current counter.EventCount, out chan<- Anomaly) {
	if current.ErrorRate <= a.cfg.ErrorRateThreshold {
		return
	}
	a.emit(out, Anomaly{
		Kind:          KindErrorSurge,
		Severity:      SeverityWarning,
		CurrentValue:  current.ErrorRate,
		ThresholdUsed: a.cfg.ErrorRateThreshold,
		Message:       fmt.Sprintf("error rate %.1f%% exceeds threshold %.1f%%", current.ErrorRate*100, a.cfg.ErrorRateThreshold*100),
	})
}

func (a *Analyzer) checkLatency(current counter.EventCount, snap []counter.EventCount, out chan<- Anomaly) {
	if current.P99Latency == 0 {
		return
	}
	baselineP99 := window.P99Latency(snap)
	if baselineP99 == 0 {
		return
	}

	threshold := time.Duration(float64(baselineP99) * a.cfg.LatencyMultiplier)
	if current.P99Latency <= threshold {
		return
	}

	ratio := float64(current.P99Latency) / float64(baselineP99)
	severity := SeverityWarning
	if ratio >= 5 {
		severity = SeverityCritical
	}

	a.emit(out, Anomaly{
		Kind:          KindLatencySpike,
		Severity:      severity,
		CurrentValue:  float64(current.P99Latency.Milliseconds()),
		BaselineValue: float64(baselineP99.Milliseconds()),
		ThresholdUsed: float64(threshold.Milliseconds()),
		SpikeRatio:    ratio,
		Message:       fmt.Sprintf("p99 latency %s (%.1f× baseline of %s)", current.P99Latency, ratio, baselineP99),
	})
}

func (a *Analyzer) checkHostFlood(current counter.EventCount, out chan<- Anomaly) {
	if current.Total == 0 || len(current.ByHost) == 0 {
		return
	}

	floodThreshold := float64(current.Total) * a.cfg.HostFloodFraction
	var topHost string
	var topCount int64
	for host, count := range current.ByHost {
		if count > topCount {
			topCount = count
			topHost = host
		}
	}

	if float64(topCount) <= floodThreshold {
		return
	}

	fraction := float64(topCount) / float64(current.Total)
	a.emit(out, Anomaly{
		Kind:          KindHostFlood,
		Severity:      SeverityCritical,
		CurrentValue:  fraction,
		ThresholdUsed: a.cfg.HostFloodFraction,
		SpikeRatio:    fraction,
		OffendingHost: topHost,
		Message:       fmt.Sprintf("host %s accounts for %.1f%% of traffic (%d/%d req)", topHost, fraction*100, topCount, current.Total),
	})
}

func (a *Analyzer) checkSilence(snap []counter.EventCount, out chan<- Anomaly) {
	// count consecutive zero-total buckets from newest to oldest
	consecutive := 0
	for i := len(snap) - 1; i >= 0; i-- {
		if snap[i].Total == 0 {
			consecutive++
		} else {
			break
		}
	}

	if consecutive < a.cfg.SilenceThreshold {
		return
	}

	a.emit(out, Anomaly{
		Kind:          KindSilence,
		Severity:      SeverityCritical,
		CurrentValue:  float64(consecutive),
		ThresholdUsed: float64(a.cfg.SilenceThreshold),
		Message:       fmt.Sprintf("no log events for %d consecutive seconds", consecutive),
	})
}

// ---------------------------------------------------------------------------
// Emit with cooldown
// ---------------------------------------------------------------------------

func (a *Analyzer) emit(out chan<- Anomaly, anomaly Anomaly) {
	last := a.cooldown[anomaly.Kind]
	if a.cfg.AlertCooldown > 0 && !last.IsZero() && time.Since(last) < a.cfg.AlertCooldown {
		return // still within cooldown window
	}
	anomaly.DetectedAt = time.Now()
	a.cooldown[anomaly.Kind] = anomaly.DetectedAt
	out <- anomaly
}
