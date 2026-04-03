package parser_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
)

func autoParser(t *testing.T) parser.Parser {
	t.Helper()
	p, err := parser.NewParser("auto")
	require.NoError(t, err)
	return p
}

func TestAutoParser_Name(t *testing.T) {
	t.Run("should return 'auto' as the parser name", func(t *testing.T) {
		assert.Equal(t, "auto", autoParser(t).Name())
	})
}

func TestAutoParser_DetectsNginx(t *testing.T) {
	t.Run("should detect and parse an nginx combined log line", func(t *testing.T) {
		p := autoParser(t)
		line := `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET /path HTTP/1.1" 200 512 "-" "agent" 0.005`
		event, ok := p.Parse(rawLine(line))
		require.True(t, ok)
		assert.Equal(t, 200, event.StatusCode)
		assert.Equal(t, "10.0.0.1", event.Host)
	})
}

func TestAutoParser_DetectsJSON(t *testing.T) {
	t.Run("should detect and parse a JSON log line", func(t *testing.T) {
		p := autoParser(t)
		line := `{"time":"2026-04-01T12:00:00Z","level":"error","msg":"crash"}`
		event, ok := p.Parse(rawLine(line))
		require.True(t, ok)
		assert.Equal(t, "error", event.Level)
	})
}

func TestAutoParser_DetectsSyslog(t *testing.T) {
	t.Run("should detect and parse an RFC5424 syslog line", func(t *testing.T) {
		p := autoParser(t)
		line := `<134>1 2026-04-01T12:00:00Z webhost myapp 1234 - - hello`
		event, ok := p.Parse(rawLine(line))
		require.True(t, ok)
		assert.Equal(t, "webhost", event.Host)
	})
}

func TestAutoParser_DetectsApache(t *testing.T) {
	t.Run("should detect and parse an Apache CLF line", func(t *testing.T) {
		p := autoParser(t)
		line := `192.168.0.1 - frank [01/Apr/2026:12:00:00 +0000] "GET /index.html HTTP/1.1" 200 2326`
		event, ok := p.Parse(rawLine(line))
		require.True(t, ok)
		assert.Equal(t, "192.168.0.1", event.Host)
	})
}

func TestAutoParser_LocksAfterFirstMatch(t *testing.T) {
	t.Run("should lock to nginx after first successful parse and skip syslog lines", func(t *testing.T) {
		p := autoParser(t)

		nginxLine := `10.0.0.1 - - [01/Apr/2026:00:00:00 +0000] "GET / HTTP/1.1" 200 1 "-" "x" 0.001`
		_, ok := p.Parse(rawLine(nginxLine))
		require.True(t, ok, "first nginx line must be accepted to lock the parser")

		// after locking to nginx, a syslog line should not parse
		syslogLine := `<134>1 2026-04-01T12:00:00Z host app - - - message`
		_, ok = p.Parse(rawLine(syslogLine))
		assert.False(t, ok, "should reject syslog line once locked to nginx")
	})
}

func TestAutoParser_ReturnsFalseForUnrecognizedLine(t *testing.T) {
	t.Run("should return false when no parser matches the line", func(t *testing.T) {
		p := autoParser(t)
		_, ok := p.Parse(rawLine("this matches nothing at all"))
		assert.False(t, ok)
	})
}
