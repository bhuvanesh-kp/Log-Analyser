package parser_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
)

// Apache Common Log Format (CLF):
// $host - $user [$time] "$request" $status $bytes
//
// Example:
// 192.168.0.1 - frank [01/Apr/2026:12:00:00 +0000] "GET /index.html HTTP/1.1" 200 2326

func apacheParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := parser.NewParser("apache")
	require.NoError(t, err)
	return p
}

func TestApacheParser_Name(t *testing.T) {
	t.Run("should return 'apache' as the parser name", func(t *testing.T) {
		assert.Equal(t, "apache", apacheParser(t).Name())
	})
}

func TestApacheParser_Parse_ValidLine(t *testing.T) {
	tests := []struct {
		desc   string
		line   string
		check  func(t *testing.T, e parser.ParsedEvent)
	}{
		{
			desc: "should parse remote host into Host",
			line: `192.168.0.1 - frank [01/Apr/2026:12:00:00 +0000] "GET /index.html HTTP/1.1" 200 2326`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "192.168.0.1", e.Host)
			},
		},
		{
			desc: "should parse HTTP method",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "POST /submit HTTP/1.1" 302 0`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "POST", e.Method)
			},
		},
		{
			desc: "should parse URL path",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET /about HTTP/1.1" 200 1024`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "/about", e.Path)
			},
		},
		{
			desc: "should parse status code",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET /missing HTTP/1.1" 404 512`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, 404, e.StatusCode)
			},
		},
		{
			desc: "should parse response bytes",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 4096`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, int64(4096), e.Bytes)
			},
		},
		{
			desc: "should set Latency to zero since CLF has no request_time field",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, time.Duration(0), e.Latency)
			},
		},
		{
			desc: "should parse timestamp",
			line: `10.0.0.1 - - [01/Apr/2026:09:15:30 +0000] "GET / HTTP/1.1" 200 1`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.False(t, e.Timestamp.IsZero())
				assert.Equal(t, 9, e.Timestamp.Hour())
				assert.Equal(t, 15, e.Timestamp.Minute())
			},
		},
	}

	p := apacheParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			event, ok := p.Parse(rawLine(tc.line))
			require.True(t, ok, "expected line to be parseable")
			tc.check(t, event)
		})
	}
}

func TestApacheParser_Parse_InvalidLine(t *testing.T) {
	tests := []struct {
		desc string
		line string
	}{
		{
			desc: "should return false for a plain text line",
			line: "not a log line",
		},
		{
			desc: "should return false for an empty line",
			line: "",
		},
		{
			desc: "should return false for a JSON line",
			line: `{"level":"info","msg":"ok"}`,
		},
	}

	p := apacheParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, ok := p.Parse(rawLine(tc.line))
			assert.False(t, ok)
		})
	}
}
