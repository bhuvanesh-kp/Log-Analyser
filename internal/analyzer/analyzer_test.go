package analyzer_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/analyzer"
	"log_analyser/internal/config"
	"log_analyser/internal/counter"
	"log_analyser/internal/window"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// defaultCfg returns a config tuned for deterministic tests:
// small window, short cooldown, low baseline requirement.
func defaultCfg() config.Config {
	cfg := *config.Default()
	cfg.SpikeMultiplier = 3.0
	cfg.ErrorRateThreshold = 0.05
	cfg.HostFloodFraction = 0.5
	cfg.LatencyMultiplier = 3.0
	cfg.SilenceThreshold = 3
	cfg.AlertCooldown = 0 // no cooldown — every firing should emit
	cfg.MinBaselineSamples = 3
	cfg.DetectionMethod = "ratio"
	return cfg
}

// bucket constructs a counter.EventCount for test use.
func bucket(total int64, errorRate float64, p99 time.Duration, byHost map[string]int64) counter.EventCount {
	return counter.EventCount{
		WindowStart: time.Now(),
		WindowEnd:   time.Now().Add(time.Second),
		Total:       total,
		ErrorRate:   errorRate,
		P99Latency:  p99,
		ByHost:      byHost,
		ByStatus:    map[int]int64{},
		ByLevel:     map[string]int64{},
	}
}

// runAnalyzer sends buckets into the analyzer and collects all emitted anomalies.
// It closes the input channel after all buckets are sent and waits for Run to return.
func runAnalyzer(t *testing.T, cfg config.Config, win *window.Window, buckets []counter.EventCount) []analyzer.Anomaly {
	t.Helper()
	in := make(chan counter.EventCount, len(buckets))
	out := make(chan analyzer.Anomaly, 64)

	for _, b := range buckets {
		in <- b
	}
	close(in)

	a := analyzer.New(cfg, win)
	a.Run(context.Background(), in, out)
	close(out)

	var anomalies []analyzer.Anomaly
	for anomaly := range out {
		anomalies = append(anomalies, anomaly)
	}
	return anomalies
}

// warmWindow pre-populates the window with n buckets of steady traffic.
func warmWindow(win *window.Window, n int, total int64) {
	for i := 0; i < n; i++ {
		win.Push(bucket(total, 0, 0, nil))
	}
}

// ---------------------------------------------------------------------------
// Baseline guard
// ---------------------------------------------------------------------------

func TestAnalyzer_NoAlertBeforeBaseline(t *testing.T) {
	t.Run("should not emit anomaly when window has fewer than MinBaselineSamples buckets", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MinBaselineSamples = 5
		win := window.New(60)
		// only 2 buckets in window — below threshold
		warmWindow(win, 2, 10)

		// send a massive spike
		buckets := []counter.EventCount{bucket(10000, 0, 0, nil)}
		anomalies := runAnalyzer(t, cfg, win, buckets)
		assert.Empty(t, anomalies, "should not alert before baseline is established")
	})
}

// ---------------------------------------------------------------------------
// rate_spike
// ---------------------------------------------------------------------------

func TestAnalyzer_RateSpike(t *testing.T) {
	tests := []struct {
		desc      string
		baseline  int64
		current   int64
		wantFired bool
		wantKind  analyzer.AnomalyKind
	}{
		{
			desc:      "should fire rate_spike when current rate exceeds mean * multiplier",
			baseline:  100,
			current:   400, // 4× baseline, multiplier=3 → fires
			wantFired: true,
			wantKind:  analyzer.KindRateSpike,
		},
		{
			desc:      "should not fire rate_spike when current rate is below threshold",
			baseline:  100,
			current:   250, // 2.5× baseline, multiplier=3 → no fire
			wantFired: false,
		},
		{
			desc:      "should not fire rate_spike when current equals exactly the mean",
			baseline:  100,
			current:   100,
			wantFired: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			warmWindow(win, 10, tc.baseline)

			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(tc.current, 0, 0, nil),
			})

			if tc.wantFired {
				require.NotEmpty(t, anomalies)
				assert.Equal(t, tc.wantKind, anomalies[0].Kind)
			} else {
				assert.Empty(t, anomalies)
			}
		})
	}
}

func TestAnalyzer_RateSpike_Severity(t *testing.T) {
	tests := []struct {
		desc         string
		current      int64
		wantSeverity analyzer.SeverityLevel
	}{
		{
			desc:         "should set severity warning when spike ratio is below 10x",
			current:      400, // 4× baseline of 100
			wantSeverity: analyzer.SeverityWarning,
		},
		{
			desc:         "should set severity critical when spike ratio is 10x or more",
			current:      1100, // 11× baseline of 100
			wantSeverity: analyzer.SeverityCritical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			warmWindow(win, 10, 100)

			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(tc.current, 0, 0, nil),
			})
			require.NotEmpty(t, anomalies)
			assert.Equal(t, tc.wantSeverity, anomalies[0].Severity)
		})
	}
}

func TestAnalyzer_RateSpike_Fields(t *testing.T) {
	t.Run("should populate CurrentValue, BaselineValue, and SpikeRatio on rate_spike anomaly", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100) // mean = 100

		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
			bucket(500, 0, 0, nil),
		})
		require.NotEmpty(t, anomalies)
		a := anomalies[0]
		assert.Equal(t, float64(500), a.CurrentValue)
		assert.InDelta(t, float64(100), a.BaselineValue, 1.0)
		assert.InDelta(t, 5.0, a.SpikeRatio, 0.1)
	})
}

// ---------------------------------------------------------------------------
// error_surge
// ---------------------------------------------------------------------------

func TestAnalyzer_ErrorSurge(t *testing.T) {
	tests := []struct {
		desc      string
		errorRate float64
		wantFired bool
	}{
		{
			desc:      "should fire error_surge when error rate exceeds threshold",
			errorRate: 0.10, // 10% > 5% threshold
			wantFired: true,
		},
		{
			desc:      "should not fire error_surge when error rate is below threshold",
			errorRate: 0.03, // 3% < 5% threshold
			wantFired: false,
		},
		{
			desc:      "should not fire error_surge when error rate equals threshold exactly",
			errorRate: 0.05, // equal to threshold — not exceeded
			wantFired: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			warmWindow(win, 10, 100)

			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(100, tc.errorRate, 0, nil),
			})

			fired := false
			for _, a := range anomalies {
				if a.Kind == analyzer.KindErrorSurge {
					fired = true
				}
			}
			assert.Equal(t, tc.wantFired, fired)
		})
	}
}

func TestAnalyzer_ErrorSurge_IsAlwaysWarning(t *testing.T) {
	t.Run("should always set severity to warning for error_surge", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100)

		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
			bucket(100, 0.99, 0, nil), // extreme error rate
		})
		var found bool
		for _, a := range anomalies {
			if a.Kind == analyzer.KindErrorSurge {
				assert.Equal(t, analyzer.SeverityWarning, a.Severity)
				found = true
			}
		}
		assert.True(t, found, "error_surge anomaly should have been emitted")
	})
}

// ---------------------------------------------------------------------------
// latency_spike
// ---------------------------------------------------------------------------

func TestAnalyzer_LatencySpike(t *testing.T) {
	tests := []struct {
		desc      string
		baseP99   time.Duration
		currentP99 time.Duration
		wantFired bool
	}{
		{
			desc:       "should fire latency_spike when p99 exceeds baseline * multiplier",
			baseP99:    50 * time.Millisecond,
			currentP99: 200 * time.Millisecond, // 4× > 3× multiplier
			wantFired:  true,
		},
		{
			desc:       "should not fire latency_spike when p99 is below threshold",
			baseP99:    50 * time.Millisecond,
			currentP99: 100 * time.Millisecond, // 2× < 3× multiplier
			wantFired:  false,
		},
		{
			desc:       "should not fire latency_spike when baseline p99 is zero",
			baseP99:    0,
			currentP99: 999 * time.Millisecond,
			wantFired:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			for i := 0; i < 10; i++ {
				win.Push(bucket(100, 0, tc.baseP99, nil))
			}

			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(100, 0, tc.currentP99, nil),
			})

			fired := false
			for _, a := range anomalies {
				if a.Kind == analyzer.KindLatencySpike {
					fired = true
				}
			}
			assert.Equal(t, tc.wantFired, fired)
		})
	}
}

func TestAnalyzer_LatencySpike_Severity(t *testing.T) {
	tests := []struct {
		desc         string
		currentP99   time.Duration
		wantSeverity analyzer.SeverityLevel
	}{
		{
			desc:         "should set severity warning when latency ratio is below 5x",
			currentP99:   160 * time.Millisecond, // 3.2× of 50ms baseline
			wantSeverity: analyzer.SeverityWarning,
		},
		{
			desc:         "should set severity critical when latency ratio is 5x or more",
			currentP99:   300 * time.Millisecond, // 6× of 50ms baseline
			wantSeverity: analyzer.SeverityCritical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			for i := 0; i < 10; i++ {
				win.Push(bucket(100, 0, 50*time.Millisecond, nil))
			}

			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(100, 0, tc.currentP99, nil),
			})
			var found bool
			for _, a := range anomalies {
				if a.Kind == analyzer.KindLatencySpike {
					assert.Equal(t, tc.wantSeverity, a.Severity)
					found = true
				}
			}
			assert.True(t, found, "latency_spike should have been emitted")
		})
	}
}

// ---------------------------------------------------------------------------
// host_flood
// ---------------------------------------------------------------------------

func TestAnalyzer_HostFlood(t *testing.T) {
	tests := []struct {
		desc      string
		byHost    map[string]int64
		total     int64
		wantFired bool
		wantHost  string
	}{
		{
			desc:      "should fire host_flood when single host exceeds flood fraction",
			byHost:    map[string]int64{"1.2.3.4": 80, "5.6.7.8": 20},
			total:     100,
			wantFired: true,
			wantHost:  "1.2.3.4",
		},
		{
			desc:      "should not fire host_flood when no single host exceeds threshold",
			byHost:    map[string]int64{"1.2.3.4": 40, "5.6.7.8": 35, "9.10.11.12": 25},
			total:     100,
			wantFired: false,
		},
		{
			desc:      "should not fire host_flood when total is zero",
			byHost:    map[string]int64{},
			total:     0,
			wantFired: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			warmWindow(win, 10, 100)

			b := bucket(tc.total, 0, 0, tc.byHost)
			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{b})

			fired := false
			for _, a := range anomalies {
				if a.Kind == analyzer.KindHostFlood {
					fired = true
					if tc.wantHost != "" {
						assert.Equal(t, tc.wantHost, a.OffendingHost)
					}
				}
			}
			assert.Equal(t, tc.wantFired, fired)
		})
	}
}

func TestAnalyzer_HostFlood_IsAlwaysCritical(t *testing.T) {
	t.Run("should always set severity critical for host_flood", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100)

		b := bucket(100, 0, 0, map[string]int64{"attacker": 90, "other": 10})
		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{b})
		var found bool
		for _, a := range anomalies {
			if a.Kind == analyzer.KindHostFlood {
				assert.Equal(t, analyzer.SeverityCritical, a.Severity)
				found = true
			}
		}
		assert.True(t, found)
	})
}

// ---------------------------------------------------------------------------
// silence
// ---------------------------------------------------------------------------

func TestAnalyzer_Silence(t *testing.T) {
	tests := []struct {
		desc      string
		silent    int // consecutive zero buckets to push before the test bucket
		wantFired bool
	}{
		{
			desc:      "should fire silence when consecutive zero-total buckets reach threshold",
			silent:    3, // SilenceThreshold=3
			wantFired: true,
		},
		{
			desc:      "should not fire silence when zero-total streak is below threshold",
			silent:    2,
			wantFired: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			win := window.New(60)
			warmWindow(win, 10, 100) // establish baseline
			for i := 0; i < tc.silent; i++ {
				win.Push(bucket(0, 0, 0, nil))
			}

			// the incoming bucket is also zero (silence continues)
			anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
				bucket(0, 0, 0, nil),
			})

			fired := false
			for _, a := range anomalies {
				if a.Kind == analyzer.KindSilence {
					fired = true
				}
			}
			assert.Equal(t, tc.wantFired, fired)
		})
	}
}

func TestAnalyzer_Silence_IsAlwaysCritical(t *testing.T) {
	t.Run("should always set severity critical for silence", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100)
		for i := 0; i < 3; i++ {
			win.Push(bucket(0, 0, 0, nil))
		}

		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
			bucket(0, 0, 0, nil),
		})
		var found bool
		for _, a := range anomalies {
			if a.Kind == analyzer.KindSilence {
				assert.Equal(t, analyzer.SeverityCritical, a.Severity)
				found = true
			}
		}
		assert.True(t, found)
	})
}

// ---------------------------------------------------------------------------
// Cooldown
// ---------------------------------------------------------------------------

func TestAnalyzer_Cooldown_SuppressDuplicates(t *testing.T) {
	t.Run("should not re-emit same anomaly kind within cooldown window", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.AlertCooldown = 10 * time.Minute // long cooldown

		win := window.New(60)
		warmWindow(win, 10, 100)

		// two consecutive spikes — only first should emit
		in := make(chan counter.EventCount, 2)
		in <- bucket(500, 0, 0, nil)
		in <- bucket(500, 0, 0, nil)
		close(in)

		out := make(chan analyzer.Anomaly, 16)
		a := analyzer.New(cfg, win)
		a.Run(context.Background(), in, out)
		close(out)

		var rateSpikeCount int
		for anomaly := range out {
			if anomaly.Kind == analyzer.KindRateSpike {
				rateSpikeCount++
			}
		}
		assert.Equal(t, 1, rateSpikeCount, "cooldown should suppress the second rate_spike")
	})
}

func TestAnalyzer_Cooldown_IndependentPerKind(t *testing.T) {
	t.Run("should allow different anomaly kinds to fire independently within their cooldowns", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.AlertCooldown = 10 * time.Minute

		win := window.New(60)
		warmWindow(win, 10, 100)

		// one bucket that triggers both rate_spike and error_surge
		in := make(chan counter.EventCount, 1)
		in <- bucket(500, 0.20, 0, nil)
		close(in)

		out := make(chan analyzer.Anomaly, 16)
		a := analyzer.New(cfg, win)
		a.Run(context.Background(), in, out)
		close(out)

		kinds := map[analyzer.AnomalyKind]bool{}
		for anomaly := range out {
			kinds[anomaly.Kind] = true
		}
		assert.True(t, kinds[analyzer.KindRateSpike], "rate_spike should fire")
		assert.True(t, kinds[analyzer.KindErrorSurge], "error_surge should fire independently")
	})
}

// ---------------------------------------------------------------------------
// Sigma detection mode
// ---------------------------------------------------------------------------

func TestAnalyzer_SigmaMode_RateSpike(t *testing.T) {
	t.Run("should fire rate_spike in sigma mode when current exceeds mean + k*stddev", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.DetectionMethod = "sigma"
		cfg.SpikeMultiplier = 3.0

		win := window.New(60)
		// uniform baseline: mean=100, stddev=0 → threshold = 100 + 3×0 = 100
		// any value > 100 should fire
		warmWindow(win, 10, 100)

		anomalies := runAnalyzer(t, cfg, win, []counter.EventCount{
			bucket(105, 0, 0, nil),
		})
		fired := false
		for _, a := range anomalies {
			if a.Kind == analyzer.KindRateSpike {
				fired = true
			}
		}
		assert.True(t, fired)
	})
}

// ---------------------------------------------------------------------------
// Graceful shutdown
// ---------------------------------------------------------------------------

func TestAnalyzer_GracefulShutdown(t *testing.T) {
	t.Run("should return when context is cancelled", func(t *testing.T) {
		win := window.New(60)
		in := make(chan counter.EventCount) // unbuffered — blocks
		out := make(chan analyzer.Anomaly, 4)

		ctx, cancel := context.WithCancel(context.Background())
		a := analyzer.New(defaultCfg(), win)

		done := make(chan struct{})
		go func() {
			a.Run(ctx, in, out)
			close(done)
		}()

		cancel()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Run did not return after context cancellation")
		}
	})
}

// ---------------------------------------------------------------------------
// Anomaly fields
// ---------------------------------------------------------------------------

func TestAnalyzer_AnomalyFields(t *testing.T) {
	t.Run("should populate DetectedAt with a non-zero time", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100)

		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
			bucket(500, 0, 0, nil),
		})
		require.NotEmpty(t, anomalies)
		assert.False(t, anomalies[0].DetectedAt.IsZero())
	})

	t.Run("should populate Message with a non-empty string", func(t *testing.T) {
		win := window.New(60)
		warmWindow(win, 10, 100)

		anomalies := runAnalyzer(t, defaultCfg(), win, []counter.EventCount{
			bucket(500, 0, 0, nil),
		})
		require.NotEmpty(t, anomalies)
		assert.NotEmpty(t, anomalies[0].Message)
	})
}
