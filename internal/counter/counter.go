package counter

import (
	"context"
	"math"
	"sort"
	"time"

	"log_analyser/internal/parser"
)

// EventCount is emitted by the counter goroutine once per bucket interval.
type EventCount struct {
	WindowStart time.Time
	WindowEnd   time.Time
	Total       int64
	ByStatus    map[int]int64
	ByHost      map[string]int64
	ByLevel     map[string]int64
	ErrorRate   float64
	P99Latency  time.Duration
}

// Counter aggregates ParsedEvents into fixed-duration EventCount buckets.
type Counter struct {
	bucketDuration time.Duration
}

// New returns a Counter that flushes one EventCount per bucketDuration.
func New(bucketDuration time.Duration) *Counter {
	return &Counter{bucketDuration: bucketDuration}
}

// ---------------------------------------------------------------------------
// Internal bucket
// ---------------------------------------------------------------------------

type bucket struct {
	start     time.Time
	total     int64
	errors    int64
	byStatus  map[int]int64
	byHost    map[string]int64
	byLevel   map[string]int64
	latencies []time.Duration
}

func newBucket(start time.Time) *bucket {
	return &bucket{
		start:    start,
		byStatus: make(map[int]int64),
		byHost:   make(map[string]int64),
		byLevel:  make(map[string]int64),
	}
}

func (b *bucket) add(e parser.ParsedEvent) {
	b.total++

	if e.Host != "" {
		b.byHost[e.Host]++
	}

	if e.StatusCode != 0 {
		// HTTP event — classify errors by status code
		b.byStatus[e.StatusCode]++
		if e.StatusCode >= 400 {
			b.errors++
		}
	} else if e.Level != "" {
		// structured-log event — classify errors by level
		b.byLevel[e.Level]++
		if e.Level == "error" || e.Level == "fatal" {
			b.errors++
		}
	}

	if e.Latency > 0 {
		b.latencies = append(b.latencies, e.Latency)
	}
}

func (b *bucket) flush(end time.Time) EventCount {
	var errorRate float64
	if b.total > 0 {
		errorRate = float64(b.errors) / float64(b.total)
	}
	return EventCount{
		WindowStart: b.start,
		WindowEnd:   end,
		Total:       b.total,
		ByStatus:    b.byStatus,
		ByHost:      b.byHost,
		ByLevel:     b.byLevel,
		ErrorRate:   errorRate,
		P99Latency:  p99(b.latencies),
	}
}

func p99(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(0.99 * float64(len(sorted))))
	return sorted[rank-1]
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// Run reads ParsedEvents from in, aggregates them into BucketDuration windows,
// and sends each completed EventCount to out. It blocks until ctx is cancelled.
// On shutdown it drains any buffered events still in in and flushes the final
// partial bucket before returning. out is never closed by Run — the caller owns it.
func (c *Counter) Run(ctx context.Context, in <-chan parser.ParsedEvent, out chan<- EventCount) {
	ticker := time.NewTicker(c.bucketDuration)
	defer ticker.Stop()

	cur := newBucket(time.Now())

	for {
		select {
		case e, ok := <-in:
			if !ok {
				in = nil // closed; prevent tight-looping on zero-value reads
			} else {
				cur.add(e)
			}

		case t := <-ticker.C:
			out <- cur.flush(t)
			cur = newBucket(t)

		case <-ctx.Done():
			// drain events already buffered in the channel
		drain:
			for in != nil {
				select {
				case e, ok := <-in:
					if !ok {
						break drain
					}
					cur.add(e)
				default:
					break drain
				}
			}
			out <- cur.flush(time.Now())
			return
		}
	}
}
