package ringbuf

// RingBuf is a fixed-capacity circular buffer that overwrites the oldest entry
// when full. It is not safe for concurrent use; the caller must synchronize.
type RingBuf[T any] struct {
	buf   []T
	head  int // index of the next write slot
	count int // number of valid entries
}

// New returns a RingBuf with the given fixed capacity.
func New[T any](capacity int) *RingBuf[T] {
	return &RingBuf[T]{buf: make([]T, capacity)}
}

// Push adds item to the buffer. If the buffer is full the oldest entry is
// silently overwritten.
func (r *RingBuf[T]) Push(item T) {
	r.buf[r.head] = item
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// Len returns the number of items currently in the buffer.
func (r *RingBuf[T]) Len() int { return r.count }

// Cap returns the fixed maximum capacity of the buffer.
func (r *RingBuf[T]) Cap() int { return len(r.buf) }

// At returns the item at logical index i where 0 is the oldest entry and
// Len-1 is the newest. Panics if i is out of range.
func (r *RingBuf[T]) At(i int) T {
	if i < 0 || i >= r.count {
		panic("ringbuf: index out of range")
	}
	// oldest slot = head - count (mod cap) when full, or 0 when not yet full
	oldest := (r.head - r.count + len(r.buf)) % len(r.buf)
	return r.buf[(oldest+i)%len(r.buf)]
}

// Slice returns a snapshot copy of all items in oldest-first order. The
// returned slice is independent — mutations do not affect the buffer.
func (r *RingBuf[T]) Slice() []T {
	out := make([]T, r.count)
	for i := range r.count {
		out[i] = r.At(i)
	}
	return out
}
