package window

import (
	"math"
	"sort"
	"time"

	"log_analyser/internal/counter"
)

// Mean returns the arithmetic mean of Total across all buckets.
// Returns 0 for an empty slice.
func Mean(buckets []counter.EventCount) float64 {
	if len(buckets) == 0 {
		return 0
	}
	var sum float64
	for _, b := range buckets {
		sum += float64(b.Total)
	}
	return sum / float64(len(buckets))
}

// StdDev returns the population standard deviation of Total across all buckets.
// Returns 0 for an empty slice.
func StdDev(buckets []counter.EventCount) float64 {
	if len(buckets) == 0 {
		return 0
	}
	m := Mean(buckets)
	var variance float64
	for _, b := range buckets {
		d := float64(b.Total) - m
		variance += d * d
	}
	return math.Sqrt(variance / float64(len(buckets)))
}

// ErrorRate returns the request-volume-weighted error rate across all buckets.
// Buckets with Total == 0 are excluded from the calculation.
// Returns 0 for an empty slice or when all buckets have zero total.
func ErrorRate(buckets []counter.EventCount) float64 {
	if len(buckets) == 0 {
		return 0
	}
	var totalErrors, totalRequests float64
	for _, b := range buckets {
		if b.Total == 0 {
			continue
		}
		totalErrors += float64(b.Total) * b.ErrorRate
		totalRequests += float64(b.Total)
	}
	if totalRequests == 0 {
		return 0
	}
	return totalErrors / totalRequests
}

// P99Latency returns the 99th-percentile P99Latency value across all buckets
// using the nearest-rank method: rank = ceil(0.99 * n), index = rank - 1.
// Returns 0 for an empty slice.
func P99Latency(buckets []counter.EventCount) time.Duration {
	if len(buckets) == 0 {
		return 0
	}
	vals := make([]time.Duration, len(buckets))
	for i, b := range buckets {
		vals[i] = b.P99Latency
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	rank := int(math.Ceil(0.99 * float64(len(vals))))
	return vals[rank-1]
}
