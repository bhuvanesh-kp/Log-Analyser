package parser_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/parser"
	"log_analyser/internal/tailer"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func rawLine(content string) tailer.RawLine {
	return tailer.RawLine{
		Content: content,
		Source:  "test.log",
		ReadAt:  time.Now(),
		LineNum: 1,
	}
}

// runPool starts a Pool with the given format and workers, sends lines, closes
// the input, and collects all emitted ParsedEvents.
func runPool(t *testing.T, format string, lines []string, workers int) []parser.ParsedEvent {
	t.Helper()
	in := make(chan tailer.RawLine, len(lines))
	out := make(chan parser.ParsedEvent, len(lines)+1)

	for _, l := range lines {
		in <- rawLine(l)
	}
	close(in)

	pool := parser.NewPool(format, workers)
	ctx := context.Background()
	pool.Run(ctx, in, out)
	close(out)

	var events []parser.ParsedEvent
	for e := range out {
		events = append(events, e)
	}
	return events
}

// ---------------------------------------------------------------------------
// Registry — NewParser
// ---------------------------------------------------------------------------

func TestNewParser_KnownFormats(t *testing.T) {
	tests := []struct {
		name   string
		format string
	}{
		{"should return nginx parser for nginx format", "nginx"},
		{"should return apache parser for apache format", "apache"},
		{"should return json parser for json format", "json"},
		{"should return syslog parser for syslog format", "syslog"},
		{"should return auto parser for auto format", "auto"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parser.NewParser(tc.format)
			require.NoError(t, err)
			assert.NotNil(t, p)
		})
	}
}

func TestNewParser_UnknownFormat(t *testing.T) {
	t.Run("should return error for unknown format name", func(t *testing.T) {
		_, err := parser.NewParser("grpc")
		assert.Error(t, err)
	})
}

func TestNewParser_NameMatchesFormat(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{"nginx", "nginx"},
		{"apache", "apache"},
		{"json", "json"},
		{"syslog", "syslog"},
	}
	for _, tc := range tests {
		t.Run("should have Name() == "+tc.format, func(t *testing.T) {
			p, err := parser.NewParser(tc.format)
			require.NoError(t, err)
			assert.Equal(t, tc.want, p.Name())
		})
	}
}

// ---------------------------------------------------------------------------
// Pool — Run
// ---------------------------------------------------------------------------

func TestPool_EmitsOneEventPerParsableLine(t *testing.T) {
	t.Run("should emit one ParsedEvent for each parseable nginx line", func(t *testing.T) {
		lines := []string{
			`127.0.0.1 - - [01/Apr/2026:00:00:01 +0000] "GET /api HTTP/1.1" 200 512 "-" "curl/7.0" 0.042`,
			`127.0.0.1 - - [01/Apr/2026:00:00:02 +0000] "POST /login HTTP/1.1" 401 128 "-" "curl/7.0" 0.010`,
		}
		events := runPool(t, "nginx", lines, 1)
		assert.Len(t, events, 2)
	})
}

func TestPool_DropsUnparseableLines(t *testing.T) {
	t.Run("should not emit an event for a line that does not match the format", func(t *testing.T) {
		lines := []string{
			`this is not a valid log line`,
			`127.0.0.1 - - [01/Apr/2026:00:00:01 +0000] "GET / HTTP/1.1" 200 512 "-" "curl/7.0" 0.001`,
		}
		events := runPool(t, "nginx", lines, 1)
		// only the valid line should produce an event
		assert.Len(t, events, 1)
	})
}

func TestPool_GracefulShutdown(t *testing.T) {
	t.Run("should return when context is cancelled mid-stream", func(t *testing.T) {
		in := make(chan tailer.RawLine) // unbuffered — will block workers
		out := make(chan parser.ParsedEvent, 4)
		ctx, cancel := context.WithCancel(context.Background())

		pool := parser.NewPool("nginx", 1)
		done := make(chan struct{})
		go func() {
			pool.Run(ctx, in, out)
			close(done)
		}()

		cancel()
		select {
		case <-done:
			// Run returned — success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Pool.Run did not return after context cancellation")
		}
	})
}

func TestPool_MultipleWorkers(t *testing.T) {
	t.Run("should emit all events when using multiple workers", func(t *testing.T) {
		const n = 20
		lines := make([]string, n)
		for i := range lines {
			lines[i] = `10.0.0.1 - - [01/Apr/2026:00:00:01 +0000] "GET /path HTTP/1.1" 200 256 "-" "agent" 0.005`
		}
		events := runPool(t, "nginx", lines, 4)
		assert.Len(t, events, n)
	})
}

func TestPool_RawFieldPreserved(t *testing.T) {
	t.Run("should preserve the original line content in ParsedEvent.Raw", func(t *testing.T) {
		line := `192.168.1.1 - - [01/Apr/2026:00:00:01 +0000] "GET /health HTTP/1.1" 200 2 "-" "probe" 0.001`
		events := runPool(t, "nginx", []string{line}, 1)
		require.Len(t, events, 1)
		assert.Equal(t, line, events[0].Raw)
	})
}

func TestPool_SourceFieldPreserved(t *testing.T) {
	t.Run("should set Source from the RawLine source field", func(t *testing.T) {
		in := make(chan tailer.RawLine, 1)
		out := make(chan parser.ParsedEvent, 1)

		in <- tailer.RawLine{
			Content: `10.0.0.1 - - [01/Apr/2026:00:00:01 +0000] "GET / HTTP/1.1" 200 1 "-" "x" 0.001`,
			Source:  "/var/log/nginx/access.log",
			ReadAt:  time.Now(),
			LineNum: 7,
		}
		close(in)

		pool := parser.NewPool("nginx", 1)
		pool.Run(context.Background(), in, out)
		close(out)

		events := make([]parser.ParsedEvent, 0)
		for e := range out {
			events = append(events, e)
		}
		require.Len(t, events, 1)
		assert.Equal(t, "/var/log/nginx/access.log", events[0].Source)
	})
}
