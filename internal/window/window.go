package window

import (
	"sync"

	"log_analyser/internal/counter"
	"log_analyser/pkg/ringbuf"
)

// Window is a thread-safe sliding window over counter.EventCount buckets.
// Push is safe for concurrent use with Snapshot and Len.
type Window struct {
	mu  sync.RWMutex
	buf *ringbuf.RingBuf[counter.EventCount]
}

// New returns a Window that holds at most capacity buckets.
func New(capacity int) *Window {
	return &Window{buf: ringbuf.New[counter.EventCount](capacity)}
}

// Push appends a new EventCount, evicting the oldest when the window is full.
func (w *Window) Push(e counter.EventCount) {
	w.mu.Lock()
	w.buf.Push(e)
	w.mu.Unlock()
}

// Len returns the number of buckets currently in the window.
func (w *Window) Len() int {
	w.mu.RLock()
	n := w.buf.Len()
	w.mu.RUnlock()
	return n
}

// Snapshot returns an independent copy of all buckets in oldest-first order.
// The caller may freely read or mutate the returned slice.
func (w *Window) Snapshot() []counter.EventCount {
	w.mu.RLock()
	s := w.buf.Slice()
	w.mu.RUnlock()
	return s
}
