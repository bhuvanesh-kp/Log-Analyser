package parser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"log_analyser/internal/tailer"
)

// ParsedEvent is the normalised representation of a single log line,
// shared across all log formats.
type ParsedEvent struct {
	Timestamp  time.Time
	Source     string        // file path or "stdin"
	Level      string        // "info", "warn", "error", "fatal" — syslog/json logs
	Host       string        // source IP or hostname
	Method     string        // HTTP method (empty for non-HTTP logs)
	Path       string        // URL path (empty for non-HTTP logs)
	StatusCode int           // HTTP status code (0 for non-HTTP logs)
	Bytes      int64         // response bytes (0 if not present)
	Latency    time.Duration // request latency (0 if not present)
	Raw        string        // original unparsed line
}

// Parser parses a single raw log line into a ParsedEvent.
// Parse returns (event, true) on success, or (_, false) if the line should be
// dropped (blank, comment, or format mismatch).
type Parser interface {
	Name() string
	Parse(raw tailer.RawLine) (ParsedEvent, bool)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

var registry = map[string]func() Parser{
	"nginx":  func() Parser { return &nginxParser{} },
	"apache": func() Parser { return &apacheParser{} },
	"json":   func() Parser { return &jsonParser{} },
	"syslog": func() Parser { return &syslogParser{} },
	"auto":   func() Parser { return newAutoParser() },
}

// NewParser returns a fresh Parser for the named format.
// Returns an error for unknown format names.
func NewParser(format string) (Parser, error) {
	fn, ok := registry[format]
	if !ok {
		return nil, fmt.Errorf("unknown log format %q; valid formats: nginx, apache, json, syslog, auto", format)
	}
	return fn(), nil
}

// ---------------------------------------------------------------------------
// Pool
// ---------------------------------------------------------------------------

// Pool fans one input channel of RawLines across N worker goroutines, each
// running its own Parser instance. Workers send successfully parsed events to
// out. Unparseable lines are silently dropped.
type Pool struct {
	format  string
	workers int
}

// NewPool returns a Pool that will use the named format and the given number
// of worker goroutines.
func NewPool(format string, workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{format: format, workers: workers}
}

// Run starts the worker goroutines and blocks until all workers have exited.
// Workers exit when in is closed or ctx is cancelled.
// Run does NOT close out — the caller owns the channel.
func (p *Pool) Run(ctx context.Context, in <-chan tailer.RawLine, out chan<- ParsedEvent) {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// each worker gets its own Parser instance — no shared mutable state
			parser, err := NewParser(p.format)
			if err != nil {
				return
			}
			for {
				select {
				case <-ctx.Done():
					return
				case raw, ok := <-in:
					if !ok {
						return
					}
					event, parsed := parser.Parse(raw)
					if parsed {
						out <- event
					}
				}
			}
		}()
	}
	wg.Wait()
}
