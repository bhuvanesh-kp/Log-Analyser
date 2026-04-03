package alerter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"log_analyser/internal/analyzer"
)

// FileAlerter appends one JSON line per anomaly to a file (JSONL format).
// It implements io.Closer — call Close() to release the file handle.
type FileAlerter struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewFileAlerter opens (or creates) the file at path in append mode.
// Returns an error if the parent directory does not exist.
func NewFileAlerter(path string) (*FileAlerter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open alert file %q: %w", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &FileAlerter{f: f, enc: enc}, nil
}

func (fa *FileAlerter) Name() string { return "file" }

// Send encodes the anomaly as a single JSON line and flushes it to disk.
// Thread-safe: multiple goroutines may call Send concurrently.
func (fa *FileAlerter) Send(_ context.Context, a analyzer.Anomaly) error {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.enc.Encode(a)
}

// Close releases the underlying file handle.
func (fa *FileAlerter) Close() error {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.f.Close()
}
