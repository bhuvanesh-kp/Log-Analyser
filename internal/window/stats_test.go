package window_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"log_analyser/internal/counter"
	"log_analyser/internal/window"
)

func TestMean(t *testing.T) {
	tests := []struct {
		desc    string
		buckets []counter.EventCount
		want    float64
	}{
		{
			desc:    "should return 0 when bucket list is empty",
			buckets: nil,
			want:    0,
		},
		{
			desc:    "should return the total when only one bucket exists",
			buckets: []counter.EventCount{{Total: 42}},
			want:    42.0,
		},
		{
			desc:    "should return average total across all buckets",
			buckets: []counter.EventCount{{Total: 10}, {Total: 20}, {Total: 30}},
			want:    20.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := window.Mean(tc.buckets)
			assert.InDelta(t, tc.want, got, 0.001)
		})
	}
}

func TestStdDev(t *testing.T) {
	tests := []struct {
		desc    string
		buckets []counter.EventCount
		want    float64
	}{
		{
			desc:    "should return 0 when bucket list is empty",
			buckets: nil,
			want:    0,
		},
		{
			desc:    "should return 0 when all buckets have the same total",
			buckets: []counter.EventCount{{Total: 5}, {Total: 5}, {Total: 5}},
			want:    0,
		},
		{
			// values: 2,4,4,4,5,5,7,9 → mean=5, population stddev=2
			desc: "should return correct population standard deviation for known values",
			buckets: []counter.EventCount{
				{Total: 2}, {Total: 4}, {Total: 4}, {Total: 4},
				{Total: 5}, {Total: 5}, {Total: 7}, {Total: 9},
			},
			want: 2.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := window.StdDev(tc.buckets)
			assert.InDelta(t, tc.want, got, 0.001)
		})
	}
}

func TestErrorRate(t *testing.T) {
	tests := []struct {
		desc    string
		buckets []counter.EventCount
		want    float64
	}{
		{
			desc:    "should return 0 when bucket list is empty",
			buckets: nil,
			want:    0,
		},
		{
			desc: "should return 0 when all buckets have zero error rate",
			buckets: []counter.EventCount{
				{Total: 100, ErrorRate: 0},
				{Total: 50, ErrorRate: 0},
			},
			want: 0,
		},
		{
			// 100 reqs × 10% = 10 errors; 200 reqs × 20% = 40 errors → 50/300 ≈ 0.1667
			desc: "should return weighted error rate when buckets have different request volumes",
			buckets: []counter.EventCount{
				{Total: 100, ErrorRate: 0.10},
				{Total: 200, ErrorRate: 0.20},
			},
			want: 0.1667,
		},
		{
			desc: "should ignore zero-total buckets when computing weighted error rate",
			buckets: []counter.EventCount{
				{Total: 0, ErrorRate: 0},
				{Total: 100, ErrorRate: 0.10},
			},
			want: 0.10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := window.ErrorRate(tc.buckets)
			assert.InDelta(t, tc.want, got, 0.001)
		})
	}
}

func TestP99Latency(t *testing.T) {
	tests := []struct {
		desc    string
		buckets []counter.EventCount
		want    time.Duration
	}{
		{
			desc:    "should return 0 when bucket list is empty",
			buckets: nil,
			want:    0,
		},
		{
			desc:    "should return the only value when one bucket exists",
			buckets: []counter.EventCount{{P99Latency: 50 * time.Millisecond}},
			want:    50 * time.Millisecond,
		},
		{
			// sorted [10ms, 50ms, 100ms]; nearest-rank p99: ceil(0.99×3)-1=2 → 100ms
			desc: "should return 99th percentile of bucket p99 latency values",
			buckets: []counter.EventCount{
				{P99Latency: 50 * time.Millisecond},
				{P99Latency: 10 * time.Millisecond},
				{P99Latency: 100 * time.Millisecond},
			},
			want: 100 * time.Millisecond,
		},
		{
			// 100 buckets 1ms..100ms; nearest-rank p99: ceil(0.99×100)-1=98 → 99ms
			desc: "should return 99ms when 100 buckets have p99 latencies from 1ms to 100ms",
			buckets: func() []counter.EventCount {
				var b []counter.EventCount
				for i := 1; i <= 100; i++ {
					b = append(b, counter.EventCount{P99Latency: time.Duration(i) * time.Millisecond})
				}
				return b
			}(),
			want: 99 * time.Millisecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			got := window.P99Latency(tc.buckets)
			assert.Equal(t, tc.want, got)
		})
	}
}
