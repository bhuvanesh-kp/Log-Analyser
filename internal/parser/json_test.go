package parser_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
)

func jsonParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := parser.NewParser("json")
	require.NoError(t, err)
	return p
}

// ---------------------------------------------------------------------------
// Basic field extraction
// ---------------------------------------------------------------------------

func TestJSONParser_Name(t *testing.T) {
	t.Run("should return 'json' as the parser name", func(t *testing.T) {
		assert.Equal(t, "json", jsonParser(t).Name())
	})
}

func TestJSONParser_Parse_ValidLine(t *testing.T) {
	tests := []struct {
		desc  string
		line  string
		check func(t *testing.T, e parser.ParsedEvent)
	}{
		// ----- timestamp aliases -----
		{
			desc:  "should parse timestamp from 'timestamp' key",
			line:  `{"timestamp":"2026-04-01T12:00:00Z","level":"info","msg":"ok"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.False(t, e.Timestamp.IsZero()) },
		},
		{
			desc:  "should parse timestamp from 'time' key",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","msg":"ok"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.False(t, e.Timestamp.IsZero()) },
		},
		{
			desc:  "should parse timestamp from 'ts' key (unix epoch float)",
			line:  `{"ts":1743508800.0,"level":"info","msg":"ok"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.False(t, e.Timestamp.IsZero()) },
		},
		{
			desc:  "should parse timestamp from '@timestamp' key (ECS format)",
			line:  `{"@timestamp":"2026-04-01T12:00:00Z","level":"info","msg":"ok"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.False(t, e.Timestamp.IsZero()) },
		},

		// ----- level aliases -----
		{
			desc:  "should parse level from 'level' key",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"error","msg":"boom"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "error", e.Level) },
		},
		{
			desc:  "should parse level from 'severity' key",
			line:  `{"time":"2026-04-01T12:00:00Z","severity":"warn","msg":"watch out"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "warn", e.Level) },
		},
		{
			desc:  "should parse level from 'lvl' key",
			line:  `{"time":"2026-04-01T12:00:00Z","lvl":"debug","msg":"verbose"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "debug", e.Level) },
		},
		{
			desc:  "should lowercase the level value",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"ERROR","msg":"boom"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "error", e.Level) },
		},

		// ----- latency aliases -----
		{
			desc:  "should parse latency from 'latency_ms' key as milliseconds",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","latency_ms":42}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, 42*time.Millisecond, e.Latency) },
		},
		{
			desc:  "should parse latency from 'duration_ms' key as milliseconds",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","duration_ms":100}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, 100*time.Millisecond, e.Latency) },
		},
		{
			desc:  "should parse latency from 'elapsed_ms' key as milliseconds",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","elapsed_ms":7}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, 7*time.Millisecond, e.Latency) },
		},
		{
			desc:  "should set Latency to zero when no latency key is present",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","msg":"ok"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, time.Duration(0), e.Latency) },
		},

		// ----- optional HTTP fields -----
		{
			desc:  "should parse status_code into StatusCode",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","status_code":404}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, 404, e.StatusCode) },
		},
		{
			desc:  "should parse host field into Host",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","host":"10.0.0.1"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "10.0.0.1", e.Host) },
		},
		{
			desc:  "should parse method field into Method",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","method":"PUT"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "PUT", e.Method) },
		},
		{
			desc:  "should parse path field into Path",
			line:  `{"time":"2026-04-01T12:00:00Z","level":"info","path":"/api/v2"}`,
			check: func(t *testing.T, e parser.ParsedEvent) { assert.Equal(t, "/api/v2", e.Path) },
		},
	}

	p := jsonParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			event, ok := p.Parse(rawLine(tc.line))
			require.True(t, ok, "expected line to be parseable")
			tc.check(t, event)
		})
	}
}

// ---------------------------------------------------------------------------
// Invalid / non-JSON lines
// ---------------------------------------------------------------------------

func TestJSONParser_Parse_InvalidLine(t *testing.T) {
	tests := []struct {
		desc string
		line string
	}{
		{
			desc: "should return false for a plain text line",
			line: "just a plain string",
		},
		{
			desc: "should return false for an empty line",
			line: "",
		},
		{
			desc: "should return false for malformed JSON",
			line: `{"level":"info","msg":}`,
		},
		{
			desc: "should return false for an nginx log line",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1 "-" "x" 0.001`,
		},
	}

	p := jsonParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, ok := p.Parse(rawLine(tc.line))
			assert.False(t, ok)
		})
	}
}
