package counter_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/counter"
	"log_analyser/internal/parser"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collect drains out until it is closed or timeout is reached, returning all
// received EventCounts.
func collect(t *testing.T, out <-chan counter.EventCount, timeout time.Duration) []counter.EventCount {
	t.Helper()
	var result []counter.EventCount
	deadline := time.After(timeout)
	for {
		select {
		case ec, ok := <-out:
			if !ok {
				return result
			}
			result = append(result, ec)
		case <-deadline:
			return result
		}
	}
}

// sendEvents pushes events onto in and then closes it.
func sendEvents(in chan<- parser.ParsedEvent, events []parser.ParsedEvent) {
	for _, e := range events {
		in <- e
	}
	close(in)
}

// httpEvent builds a minimal HTTP ParsedEvent.
func httpEvent(status int, latency time.Duration) parser.ParsedEvent {
	return parser.ParsedEvent{
		Timestamp:  time.Now(),
		Host:       "10.0.0.1",
		Method:     "GET",
		Path:       "/",
		StatusCode: status,
		Latency:    latency,
	}
}

// logEvent builds a minimal structured-log ParsedEvent (no HTTP fields).
func logEvent(level string) parser.ParsedEvent {
	return parser.ParsedEvent{
		Timestamp: time.Now(),
		Level:     level,
	}
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	tests := []struct {
		desc           string
		bucketDuration time.Duration
		check          func(t *testing.T, c *counter.Counter)
	}{
		{
			desc:           "should return non-nil counter when created with valid bucket duration",
			bucketDuration: time.Second,
			check: func(t *testing.T, c *counter.Counter) {
				assert.NotNil(t, c)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			c := counter.New(tc.bucketDuration)
			tc.check(t, c)
		})
	}
}

// ---------------------------------------------------------------------------
// Total counting
// ---------------------------------------------------------------------------

func TestRun_Total(t *testing.T) {
	tests := []struct {
		desc      string
		events    []parser.ParsedEvent
		wantTotal int64
	}{
		{
			desc:      "should return total of 3 when 3 events are sent in one bucket",
			events:    []parser.ParsedEvent{httpEvent(200, 0), httpEvent(200, 0), httpEvent(200, 0)},
			wantTotal: 3,
		},
		{
			desc:      "should return total of 0 when no events are sent before flush",
			events:    nil,
			wantTotal: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 16)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			// wait for at least one flush
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets, "should receive at least one flushed bucket")

			var total int64
			for _, b := range buckets {
				total += b.Total
			}
			assert.Equal(t, tc.wantTotal, total)
		})
	}
}

// ---------------------------------------------------------------------------
// ByStatus
// ---------------------------------------------------------------------------

func TestRun_ByStatus(t *testing.T) {
	tests := []struct {
		desc       string
		events     []parser.ParsedEvent
		wantCounts map[int]int64
	}{
		{
			desc: "should count each status code separately when events have different status codes",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(200, 0), httpEvent(404, 0), httpEvent(500, 0),
			},
			wantCounts: map[int]int64{200: 2, 404: 1, 500: 1},
		},
		{
			desc: "should count only 200s when all events have status 200",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(200, 0),
			},
			wantCounts: map[int]int64{200: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 16)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets)

			merged := make(map[int]int64)
			for _, b := range buckets {
				for k, v := range b.ByStatus {
					merged[k] += v
				}
			}
			assert.Equal(t, tc.wantCounts, merged)
		})
	}
}

// ---------------------------------------------------------------------------
// ByHost
// ---------------------------------------------------------------------------

func TestRun_ByHost(t *testing.T) {
	tests := []struct {
		desc       string
		events     []parser.ParsedEvent
		wantCounts map[string]int64
	}{
		{
			desc: "should count each host separately when events come from different hosts",
			events: []parser.ParsedEvent{
				{Timestamp: time.Now(), Host: "10.0.0.1", StatusCode: 200},
				{Timestamp: time.Now(), Host: "10.0.0.1", StatusCode: 200},
				{Timestamp: time.Now(), Host: "10.0.0.2", StatusCode: 200},
			},
			wantCounts: map[string]int64{"10.0.0.1": 2, "10.0.0.2": 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 16)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets)

			merged := make(map[string]int64)
			for _, b := range buckets {
				for k, v := range b.ByHost {
					merged[k] += v
				}
			}
			assert.Equal(t, tc.wantCounts, merged)
		})
	}
}

// ---------------------------------------------------------------------------
// ByLevel
// ---------------------------------------------------------------------------

func TestRun_ByLevel(t *testing.T) {
	tests := []struct {
		desc       string
		events     []parser.ParsedEvent
		wantCounts map[string]int64
	}{
		{
			desc: "should count each log level separately when events have different levels",
			events: []parser.ParsedEvent{
				logEvent("info"), logEvent("info"), logEvent("error"), logEvent("fatal"),
			},
			wantCounts: map[string]int64{"info": 2, "error": 1, "fatal": 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 16)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets)

			merged := make(map[string]int64)
			for _, b := range buckets {
				for k, v := range b.ByLevel {
					merged[k] += v
				}
			}
			assert.Equal(t, tc.wantCounts, merged)
		})
	}
}

// ---------------------------------------------------------------------------
// ErrorRate
// ---------------------------------------------------------------------------

func TestRun_ErrorRate(t *testing.T) {
	tests := []struct {
		desc          string
		events        []parser.ParsedEvent
		wantErrorRate float64
		delta         float64
	}{
		{
			desc: "should return 0.5 error rate when half of HTTP events are 5xx",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(200, 0), httpEvent(500, 0), httpEvent(503, 0),
			},
			wantErrorRate: 0.5,
			delta:         0.01,
		},
		{
			desc: "should return 0.25 error rate when one of four HTTP events is 4xx",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(200, 0), httpEvent(200, 0), httpEvent(404, 0),
			},
			wantErrorRate: 0.25,
			delta:         0.01,
		},
		{
			desc: "should return 0 error rate when all HTTP events are 2xx",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(201, 0),
			},
			wantErrorRate: 0.0,
			delta:         0.01,
		},
		{
			desc: "should return 1.0 error rate when all log-level events are errors or fatals",
			events: []parser.ParsedEvent{
				logEvent("error"), logEvent("fatal"),
			},
			wantErrorRate: 1.0,
			delta:         0.01,
		},
		{
			desc: "should return 0.5 error rate when half of log-level events are errors",
			events: []parser.ParsedEvent{
				logEvent("info"), logEvent("info"), logEvent("error"), logEvent("error"),
			},
			wantErrorRate: 0.5,
			delta:         0.01,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 16)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets)

			// find the bucket that actually has events
			var got float64
			for _, b := range buckets {
				if b.Total > 0 {
					got = b.ErrorRate
					break
				}
			}
			assert.InDelta(t, tc.wantErrorRate, got, tc.delta)
		})
	}
}

// ---------------------------------------------------------------------------
// P99Latency
// ---------------------------------------------------------------------------

func TestRun_P99Latency(t *testing.T) {
	tests := []struct {
		desc    string
		events  []parser.ParsedEvent
		wantP99 time.Duration
	}{
		{
			desc: "should return 0 p99 when all events have zero latency",
			events: []parser.ParsedEvent{
				httpEvent(200, 0), httpEvent(200, 0),
			},
			wantP99: 0,
		},
		{
			desc: "should return correct p99 when events have varying latencies",
			// 10 events 10ms..100ms; nearest-rank p99: ceil(0.99×10)-1=9 → 100ms
			events: func() []parser.ParsedEvent {
				var evs []parser.ParsedEvent
				for i := 1; i <= 10; i++ {
					evs = append(evs, httpEvent(200, time.Duration(i)*10*time.Millisecond))
				}
				return evs
			}(),
			wantP99: 100 * time.Millisecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			in := make(chan parser.ParsedEvent, 32)
			out := make(chan counter.EventCount, 16)
			c := counter.New(50 * time.Millisecond)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.Run(ctx, in, out)
			}()

			sendEvents(in, tc.events)
			time.Sleep(120 * time.Millisecond)
			cancel()
			<-done
			close(out)

			buckets := collect(t, out, time.Second)
			require.NotEmpty(t, buckets)

			var got time.Duration
			for _, b := range buckets {
				if b.Total > 0 {
					got = b.P99Latency
					break
				}
			}
			assert.Equal(t, tc.wantP99, got)
		})
	}
}

// ---------------------------------------------------------------------------
// Zero-event flush (silence detection support)
// ---------------------------------------------------------------------------

func TestRun_FlushesZeroBucketOnSilence(t *testing.T) {
	t.Run("should flush a zero-total bucket when no events arrive before tick", func(t *testing.T) {
		in := make(chan parser.ParsedEvent, 8)
		out := make(chan counter.EventCount, 8)
		c := counter.New(50 * time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, in, out)
		}()

		// send nothing — just wait for two ticks
		time.Sleep(130 * time.Millisecond)
		cancel()
		close(in)
		<-done
		close(out)

		buckets := collect(t, out, time.Second)
		require.NotEmpty(t, buckets, "should receive flushed buckets even with no events")
		for _, b := range buckets {
			assert.Equal(t, int64(0), b.Total)
		}
	})
}

// ---------------------------------------------------------------------------
// WindowStart / WindowEnd timestamps
// ---------------------------------------------------------------------------

func TestRun_BucketTimestamps(t *testing.T) {
	t.Run("should set WindowEnd after WindowStart in every flushed bucket", func(t *testing.T) {
		in := make(chan parser.ParsedEvent, 8)
		out := make(chan counter.EventCount, 8)
		c := counter.New(50 * time.Millisecond)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, in, out)
		}()

		time.Sleep(130 * time.Millisecond)
		cancel()
		close(in)
		<-done
		close(out)

		buckets := collect(t, out, time.Second)
		require.NotEmpty(t, buckets)
		for _, b := range buckets {
			assert.True(t, b.WindowEnd.After(b.WindowStart),
				"WindowEnd should be after WindowStart")
		}
	})
}

// ---------------------------------------------------------------------------
// Graceful shutdown: final partial bucket is flushed
// ---------------------------------------------------------------------------

func TestRun_FlushesOnShutdown(t *testing.T) {
	t.Run("should flush partial bucket on context cancellation before next tick", func(t *testing.T) {
		in := make(chan parser.ParsedEvent, 16)
		out := make(chan counter.EventCount, 16)
		c := counter.New(10 * time.Second) // very long bucket — won't tick naturally

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, in, out)
		}()

		// send 3 events then cancel before the 10s tick fires
		in <- httpEvent(200, 0)
		in <- httpEvent(200, 0)
		in <- httpEvent(200, 0)
		time.Sleep(20 * time.Millisecond)
		cancel()
		close(in)
		<-done
		close(out)

		buckets := collect(t, out, time.Second)
		var total int64
		for _, b := range buckets {
			total += b.Total
		}
		assert.Equal(t, int64(3), total,
			"should flush the 3 in-flight events in the final partial bucket")
	})
}
