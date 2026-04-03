package parser

import (
	"encoding/json"
	"strings"
	"time"

	"log_analyser/internal/tailer"
)

// Timestamp key aliases — tried in order, first non-zero value wins.
var tsKeys = []string{"timestamp", "time", "ts", "@timestamp"}

// Level key aliases.
var levelKeys = []string{"level", "severity", "lvl"}

// Latency key aliases — all assumed to be numeric milliseconds.
var latencyKeys = []string{"latency_ms", "duration_ms", "elapsed_ms"}

type jsonParser struct{}

func (p *jsonParser) Name() string { return "json" }

func (p *jsonParser) Parse(raw tailer.RawLine) (ParsedEvent, bool) {
	line := strings.TrimSpace(raw.Content)
	if line == "" || line[0] != '{' {
		return ParsedEvent{}, false
	}

	// Unmarshal into a generic map — field names vary across libraries.
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return ParsedEvent{}, false
	}

	var e ParsedEvent
	e.Raw = raw.Content
	e.Source = raw.Source

	// --- timestamp ---
	for _, k := range tsKeys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			ts, err := time.Parse(time.RFC3339, val)
			if err == nil {
				e.Timestamp = ts
			}
		case float64:
			// Unix epoch seconds (e.g. Zap's ts field)
			e.Timestamp = time.Unix(int64(val), int64((val-float64(int64(val)))*1e9))
		}
		if !e.Timestamp.IsZero() {
			break
		}
	}

	// --- level ---
	for _, k := range levelKeys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				e.Level = strings.ToLower(s)
				break
			}
		}
	}

	// --- latency (numeric ms) ---
	for _, k := range latencyKeys {
		if v, ok := m[k]; ok {
			if ms, ok := v.(float64); ok {
				e.Latency = time.Duration(ms * float64(time.Millisecond))
				break
			}
		}
	}

	// --- optional HTTP fields ---
	if v, ok := m["host"]; ok {
		if s, ok := v.(string); ok {
			e.Host = s
		}
	}
	if v, ok := m["method"]; ok {
		if s, ok := v.(string); ok {
			e.Method = s
		}
	}
	if v, ok := m["path"]; ok {
		if s, ok := v.(string); ok {
			e.Path = s
		}
	}
	if v, ok := m["status_code"]; ok {
		if f, ok := v.(float64); ok {
			e.StatusCode = int(f)
		}
	}

	return e, true
}
