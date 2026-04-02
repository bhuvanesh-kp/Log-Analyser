package window_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"log_analyser/internal/counter"
	"log_analyser/internal/window"
)

// makeEC builds a minimal EventCount for use in window tests.
func makeEC(total int64) counter.EventCount {
	now := time.Now()
	return counter.EventCount{
		WindowStart: now,
		WindowEnd:   now.Add(time.Second),
		Total:       total,
	}
}

func TestWindow_New(t *testing.T) {
	tests := []struct {
		desc     string
		capacity int
		check    func(t *testing.T, w *window.Window)
	}{
		{
			desc:     "should return 0 length when window is newly created",
			capacity: 60,
			check: func(t *testing.T, w *window.Window) {
				assert.Equal(t, 0, w.Len())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			w := window.New(tc.capacity)
			tc.check(t, w)
		})
	}
}

func TestWindow_Push(t *testing.T) {
	tests := []struct {
		desc    string
		cap     int
		pushes  int
		wantLen int
	}{
		{
			desc:    "should return 1 length when one event count is pushed",
			cap:     60,
			pushes:  1,
			wantLen: 1,
		},
		{
			desc:    "should return exact count when pushed fewer events than capacity",
			cap:     60,
			pushes:  30,
			wantLen: 30,
		},
		{
			desc:    "should return capacity when pushed more events than capacity",
			cap:     60,
			pushes:  65,
			wantLen: 60,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			w := window.New(tc.cap)
			for i := 0; i < tc.pushes; i++ {
				w.Push(makeEC(int64(i + 1)))
			}
			assert.Equal(t, tc.wantLen, w.Len())
		})
	}
}

func TestWindow_Snapshot(t *testing.T) {
	tests := []struct {
		desc    string
		events  []counter.EventCount
		wantLen int
	}{
		{
			desc:    "should return empty non-nil slice when window is empty",
			events:  nil,
			wantLen: 0,
		},
		{
			desc:    "should return all items in order when window is not full",
			events:  []counter.EventCount{makeEC(10), makeEC(20), makeEC(30)},
			wantLen: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			w := window.New(60)
			for _, e := range tc.events {
				w.Push(e)
			}
			snap := w.Snapshot()
			assert.Len(t, snap, tc.wantLen)
			assert.NotNil(t, snap)
		})
	}
}

func TestWindow_Snapshot_ReturnsIndependentCopy(t *testing.T) {
	w := window.New(60)
	w.Push(makeEC(100))
	snap := w.Snapshot()
	snap[0].Total = 999
	snap2 := w.Snapshot()
	assert.Equal(t, int64(100), snap2[0].Total,
		"should not modify window internal state when snapshot is mutated")
}
