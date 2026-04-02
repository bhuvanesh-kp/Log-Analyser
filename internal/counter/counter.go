package counter

import "time"

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
