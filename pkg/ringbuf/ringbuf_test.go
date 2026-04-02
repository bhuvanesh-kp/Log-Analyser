package ringbuf_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"log_analyser/pkg/ringbuf"
)

func TestNew(t *testing.T) {
	tests := []struct {
		desc     string
		capacity int
		check    func(t *testing.T, rb *ringbuf.RingBuf[int])
	}{
		{
			desc:     "should return 0 length when buffer is newly created",
			capacity: 5,
			check: func(t *testing.T, rb *ringbuf.RingBuf[int]) {
				assert.Equal(t, 0, rb.Len())
			},
		},
		{
			desc:     "should return configured capacity when buffer is newly created",
			capacity: 5,
			check: func(t *testing.T, rb *ringbuf.RingBuf[int]) {
				assert.Equal(t, 5, rb.Cap())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			rb := ringbuf.New[int](tc.capacity)
			tc.check(t, rb)
		})
	}
}

func TestPush(t *testing.T) {
	tests := []struct {
		desc    string
		cap     int
		items   []int
		wantLen int
	}{
		{
			desc:    "should return 1 length when one item is pushed to empty buffer",
			cap:     5,
			items:   []int{42},
			wantLen: 1,
		},
		{
			desc:    "should return capacity length when buffer is filled exactly",
			cap:     5,
			items:   []int{1, 2, 3, 4, 5},
			wantLen: 5,
		},
		{
			desc:    "should not exceed capacity when more items than capacity are pushed",
			cap:     5,
			items:   []int{1, 2, 3, 4, 5, 6, 7},
			wantLen: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			rb := ringbuf.New[int](tc.cap)
			for _, item := range tc.items {
				rb.Push(item)
			}
			assert.Equal(t, tc.wantLen, rb.Len())
		})
	}
}

func TestAt(t *testing.T) {
	tests := []struct {
		desc  string
		cap   int
		items []int
		index int
		want  int
	}{
		{
			desc:  "should return first pushed item when index is 0 and buffer is not full",
			cap:   5,
			items: []int{10, 20, 30},
			index: 0,
			want:  10,
		},
		{
			desc:  "should return last pushed item when index is Len-1 and buffer is not full",
			cap:   5,
			items: []int{10, 20, 30},
			index: 2,
			want:  30,
		},
		{
			// cap=5, push [1..7]: 1 and 2 are overwritten, oldest surviving = 3
			desc:  "should return oldest surviving item when index is 0 and buffer has wrapped",
			cap:   5,
			items: []int{1, 2, 3, 4, 5, 6, 7},
			index: 0,
			want:  3,
		},
		{
			desc:  "should return newest item when index is Len-1 and buffer has wrapped",
			cap:   5,
			items: []int{1, 2, 3, 4, 5, 6, 7},
			index: 4,
			want:  7,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			rb := ringbuf.New[int](tc.cap)
			for _, item := range tc.items {
				rb.Push(item)
			}
			assert.Equal(t, tc.want, rb.At(tc.index))
		})
	}
}

func TestSlice(t *testing.T) {
	tests := []struct {
		desc  string
		cap   int
		items []int
		want  []int
	}{
		{
			desc:  "should return empty non-nil slice when buffer is empty",
			cap:   5,
			items: []int{},
			want:  []int{},
		},
		{
			desc:  "should return items in insertion order when buffer is not full",
			cap:   5,
			items: []int{10, 20, 30},
			want:  []int{10, 20, 30},
		},
		{
			// cap=5, push [1..7]: oldest-first order is [3, 4, 5, 6, 7]
			desc:  "should return items oldest-first when buffer has wrapped",
			cap:   5,
			items: []int{1, 2, 3, 4, 5, 6, 7},
			want:  []int{3, 4, 5, 6, 7},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			rb := ringbuf.New[int](tc.cap)
			for _, item := range tc.items {
				rb.Push(item)
			}
			assert.Equal(t, tc.want, rb.Slice())
		})
	}
}

func TestSlice_ReturnsIndependentCopy(t *testing.T) {
	rb := ringbuf.New[int](5)
	rb.Push(1)
	rb.Push(2)
	rb.Push(3)
	s := rb.Slice()
	s[0] = 999
	assert.Equal(t, 1, rb.At(0),
		"should not modify ring buffer internal state when returned slice is mutated")
}

func TestAt_Panics(t *testing.T) {
	tests := []struct {
		desc  string
		items []int
		index int
	}{
		{
			desc:  "should panic when index is negative",
			items: []int{1, 2, 3},
			index: -1,
		},
		{
			desc:  "should panic when index equals length",
			items: []int{1, 2, 3},
			index: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			rb := ringbuf.New[int](5)
			for _, item := range tc.items {
				rb.Push(item)
			}
			assert.Panics(t, func() { rb.At(tc.index) })
		})
	}
}
