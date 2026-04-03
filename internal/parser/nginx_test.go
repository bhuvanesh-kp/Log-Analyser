package parser_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
)

// Nginx combined log format:
// $remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" $request_time
//
// Example:
// 203.0.113.7 - bob [01/Apr/2026:12:00:00 +0000] "GET /api/v1/users HTTP/1.1" 200 1024 "https://example.com" "Mozilla/5.0" 0.042

func nginxParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := parser.NewParser("nginx")
	require.NoError(t, err)
	return p
}

// ---------------------------------------------------------------------------
// Basic field extraction
// ---------------------------------------------------------------------------

func TestNginxParser_Name(t *testing.T) {
	t.Run("should return 'nginx' as the parser name", func(t *testing.T) {
		assert.Equal(t, "nginx", nginxParser(t).Name())
	})
}

func TestNginxParser_Parse_ValidLine(t *testing.T) {
	tests := []struct {
		desc    string
		line    string
		wantOK  bool
		check   func(t *testing.T, e parser.ParsedEvent)
	}{
		{
			desc:   "should parse remote addr into Host",
			line:   `203.0.113.7 - bob [01/Apr/2026:12:00:00 +0000] "GET /api HTTP/1.1" 200 1024 "-" "curl/7" 0.042`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "203.0.113.7", e.Host)
			},
		},
		{
			desc:   "should parse HTTP method",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "POST /login HTTP/1.1" 401 256 "-" "agent" 0.010`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "POST", e.Method)
			},
		},
		{
			desc:   "should parse URL path",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET /metrics HTTP/1.1" 200 512 "-" "prom" 0.001`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "/metrics", e.Path)
			},
		},
		{
			desc:   "should parse HTTP status code",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "DELETE /res/1 HTTP/1.1" 404 0 "-" "x" 0.005`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, 404, e.StatusCode)
			},
		},
		{
			desc:   "should parse response bytes",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 8192 "-" "x" 0.001`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, int64(8192), e.Bytes)
			},
		},
		{
			desc:   "should convert request_time seconds to Latency duration",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 100 "-" "x" 0.042`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, 42*time.Millisecond, e.Latency)
			},
		},
		{
			desc:   "should parse timestamp into Timestamp field",
			line:   `10.0.0.1 - - [01/Apr/2026:12:30:00 +0000] "GET / HTTP/1.1" 200 1 "-" "x" 0.001`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.False(t, e.Timestamp.IsZero())
				assert.Equal(t, 12, e.Timestamp.Hour())
				assert.Equal(t, 30, e.Timestamp.Minute())
			},
		},
		{
			desc:   "should set Latency to zero when request_time field is absent",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1 "-" "x"`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, time.Duration(0), e.Latency)
			},
		},
		{
			desc:   "should handle dash as response bytes (treated as 0)",
			line:   `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 304 - "-" "x" 0.001`,
			wantOK: true,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, int64(0), e.Bytes)
			},
		},
	}

	p := nginxParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			event, ok := p.Parse(rawLine(tc.line))
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK && ok {
				tc.check(t, event)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Invalid / non-nginx lines
// ---------------------------------------------------------------------------

func TestNginxParser_Parse_InvalidLine(t *testing.T) {
	tests := []struct {
		desc string
		line string
	}{
		{
			desc: "should return false for a plain text line",
			line: "this is not a log line",
		},
		{
			desc: "should return false for a JSON log line",
			line: `{"level":"info","msg":"started"}`,
		},
		{
			desc: "should return false for an empty line",
			line: "",
		},
		{
			desc: "should return false for a syslog line",
			line: `<134>1 2026-04-01T12:00:00Z host app - - - hello`,
		},
	}

	p := nginxParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, ok := p.Parse(rawLine(tc.line))
			assert.False(t, ok)
		})
	}
}
