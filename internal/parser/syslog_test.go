package parser_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
)

// RFC5424 syslog format:
// <PRI>VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA MSG
//
// Example:
// <134>1 2026-04-01T12:00:00Z webhost myapp 1234 - - User login successful

func syslogParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := parser.NewParser("syslog")
	require.NoError(t, err)
	return p
}

func TestSyslogParser_Name(t *testing.T) {
	t.Run("should return 'syslog' as the parser name", func(t *testing.T) {
		assert.Equal(t, "syslog", syslogParser(t).Name())
	})
}

func TestSyslogParser_Parse_ValidLine(t *testing.T) {
	tests := []struct {
		desc  string
		line  string
		check func(t *testing.T, e parser.ParsedEvent)
	}{
		{
			desc: "should parse hostname into Host",
			line: `<134>1 2026-04-01T12:00:00Z webhost myapp 1234 - - User login successful`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "webhost", e.Host)
			},
		},
		{
			desc: "should parse timestamp",
			line: `<134>1 2026-04-01T12:30:45Z webhost myapp 1234 - - started`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.False(t, e.Timestamp.IsZero())
				assert.Equal(t, 12, e.Timestamp.UTC().Hour())
				assert.Equal(t, 30, e.Timestamp.UTC().Minute())
			},
		},
		{
			desc: "should map severity 134 (local0.info) to level 'info'",
			line: `<134>1 2026-04-01T12:00:00Z host app - - - info message`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "info", e.Level)
			},
		},
		{
			desc: "should map severity 131 (local0.err) to level 'error'",
			line: `<131>1 2026-04-01T12:00:00Z host app - - - error message`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "error", e.Level)
			},
		},
		{
			desc: "should map severity 132 (local0.warning) to level 'warn'",
			line: `<132>1 2026-04-01T12:00:00Z host app - - - warning message`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, "warn", e.Level)
			},
		},
		{
			desc: "should set StatusCode to zero for syslog lines",
			line: `<134>1 2026-04-01T12:00:00Z host app - - - message`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.Equal(t, 0, e.StatusCode)
			},
		},
		{
			desc: "should handle nil structured-data ('-')",
			line: `<30>1 2026-04-01T12:00:00Z host app 99 - - message text`,
			check: func(t *testing.T, e parser.ParsedEvent) {
				assert.NotEmpty(t, e.Host)
			},
		},
	}

	p := syslogParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			event, ok := p.Parse(rawLine(tc.line))
			require.True(t, ok, "expected line to be parseable")
			tc.check(t, event)
		})
	}
}

func TestSyslogParser_Parse_SeverityMapping(t *testing.T) {
	// PRI = facility*8 + severity. Use facility=16 (local0, offset 128)
	// so PRI = 128 + severity.
	tests := []struct {
		desc     string
		pri      int
		expected string
	}{
		{"severity 0 (emergency) maps to fatal", 128, "fatal"},
		{"severity 1 (alert) maps to fatal", 129, "fatal"},
		{"severity 2 (critical) maps to fatal", 130, "fatal"},
		{"severity 3 (error) maps to error", 131, "error"},
		{"severity 4 (warning) maps to warn", 132, "warn"},
		{"severity 5 (notice) maps to info", 133, "info"},
		{"severity 6 (informational) maps to info", 134, "info"},
		{"severity 7 (debug) maps to debug", 135, "debug"},
	}
	p := syslogParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			line := fmt.Sprintf("<%d>1 2026-04-01T12:00:00Z host app - - - msg", tc.pri)
			event, ok := p.Parse(rawLine(line))
			require.True(t, ok)
			assert.Equal(t, tc.expected, event.Level)
		})
	}
}

func TestSyslogParser_Parse_InvalidLine(t *testing.T) {
	tests := []struct {
		desc string
		line string
	}{
		{
			desc: "should return false for a plain text line",
			line: "hello world",
		},
		{
			desc: "should return false for an empty line",
			line: "",
		},
		{
			desc: "should return false for an nginx line",
			line: `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1 "-" "x" 0.001`,
		},
		{
			desc: "should return false for a JSON line",
			line: `{"level":"info","msg":"started"}`,
		},
	}

	p := syslogParser(t)
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, ok := p.Parse(rawLine(tc.line))
			assert.False(t, ok)
		})
	}
}
