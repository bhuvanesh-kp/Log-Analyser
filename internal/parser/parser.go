package parser

import "time"

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
