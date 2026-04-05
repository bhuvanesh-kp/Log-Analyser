package tailer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/tailer"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeFile creates a file with the given content under t.TempDir().
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

// appendFile appends content to an existing file.
func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(content)
	require.NoError(t, err)
}

// collect reads from out until the channel is closed or timeout expires.
func collect(t *testing.T, out <-chan tailer.RawLine, timeout time.Duration) []tailer.RawLine {
	t.Helper()
	var lines []tailer.RawLine
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-out:
			if !ok {
				return lines
			}
			lines = append(lines, line)
		case <-deadline:
			return lines
		}
	}
}

// runTailer starts the tailer in a goroutine and returns the output channel
// and a cancel func. The caller must call cancel() to stop the tailer.
func runTailer(t *testing.T, path string, follow bool, pollInterval time.Duration) (<-chan tailer.RawLine, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan tailer.RawLine, 64)
	tl := tailer.New(path, follow, pollInterval)
	go func() {
		tl.Run(ctx, out)
		close(out)
	}()
	return out, cancel
}

// ---------------------------------------------------------------------------
// RawLine fields
// ---------------------------------------------------------------------------

func TestRawLine_Fields(t *testing.T) {
	tests := []struct {
		desc  string
		check func(t *testing.T, line tailer.RawLine)
	}{
		{
			desc: "should have non-empty Content when a line is read",
			check: func(t *testing.T, line tailer.RawLine) {
				assert.NotEmpty(t, line.Content)
			},
		},
		{
			desc: "should have non-empty Source when reading from a file",
			check: func(t *testing.T, line tailer.RawLine) {
				assert.NotEmpty(t, line.Source)
			},
		},
		{
			desc: "should have non-zero ReadAt when a line is read",
			check: func(t *testing.T, line tailer.RawLine) {
				assert.False(t, line.ReadAt.IsZero())
			},
		},
		{
			desc: "should have LineNum of 1 for the first line read",
			check: func(t *testing.T, line tailer.RawLine) {
				assert.Equal(t, int64(1), line.LineNum)
			},
		},
	}

	path := writeFile(t, "access.log", "first line\n")
	out, cancel := runTailer(t, path, false, 10*time.Millisecond)
	defer cancel()

	lines := collect(t, out, time.Second)
	require.NotEmpty(t, lines)

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tc.check(t, lines[0])
		})
	}
}

// ---------------------------------------------------------------------------
// follow=false: read existing content and stop
// ---------------------------------------------------------------------------

func TestRun_FollowFalse(t *testing.T) {
	tests := []struct {
		desc    string
		content string
		want    []string
	}{
		{
			desc:    "should return all lines when file has multiple lines and follow is false",
			content: "line1\nline2\nline3\n",
			want:    []string{"line1", "line2", "line3"},
		},
		{
			desc:    "should return empty result when file is empty and follow is false",
			content: "",
			want:    nil,
		},
		{
			desc:    "should return single line when file has one line without trailing newline",
			content: "only line",
			want:    []string{"only line"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			path := writeFile(t, "test.log", tc.content)
			out, cancel := runTailer(t, path, false, 10*time.Millisecond)
			defer cancel()

			lines := collect(t, out, time.Second)
			var got []string
			for _, l := range lines {
				got = append(got, l.Content)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// CRLF stripping
// ---------------------------------------------------------------------------

func TestRun_CRLFStripping(t *testing.T) {
	tests := []struct {
		desc    string
		content string
		want    string
	}{
		{
			desc:    "should strip carriage return when line ends with CRLF",
			content: "windows line\r\n",
			want:    "windows line",
		},
		{
			desc:    "should not alter line when line ends with LF only",
			content: "unix line\n",
			want:    "unix line",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			path := writeFile(t, "test.log", tc.content)
			out, cancel := runTailer(t, path, false, 10*time.Millisecond)
			defer cancel()

			lines := collect(t, out, time.Second)
			require.NotEmpty(t, lines)
			assert.Equal(t, tc.want, lines[0].Content)
		})
	}
}

// ---------------------------------------------------------------------------
// Source field matches file path
// ---------------------------------------------------------------------------

func TestRun_SourceField(t *testing.T) {
	t.Run("should set Source to the file path when reading from a file", func(t *testing.T) {
		path := writeFile(t, "named.log", "a line\n")
		out, cancel := runTailer(t, path, false, 10*time.Millisecond)
		defer cancel()

		lines := collect(t, out, time.Second)
		require.NotEmpty(t, lines)
		assert.Equal(t, path, lines[0].Source)
	})
}

// ---------------------------------------------------------------------------
// Line numbering
// ---------------------------------------------------------------------------

func TestRun_LineNumbering(t *testing.T) {
	tests := []struct {
		desc     string
		content  string
		wantNums []int64
	}{
		{
			desc:     "should number lines sequentially starting at 1",
			content:  "a\nb\nc\n",
			wantNums: []int64{1, 2, 3},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			path := writeFile(t, "test.log", tc.content)
			out, cancel := runTailer(t, path, false, 10*time.Millisecond)
			defer cancel()

			lines := collect(t, out, time.Second)
			require.Len(t, lines, len(tc.wantNums))
			for i, wantNum := range tc.wantNums {
				assert.Equal(t, wantNum, lines[i].LineNum)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// follow=true: picks up newly appended lines
// ---------------------------------------------------------------------------

func TestRun_FollowTrue_PicksUpNewLines(t *testing.T) {
	t.Run("should emit newly appended lines when follow is true", func(t *testing.T) {
		path := writeFile(t, "live.log", "existing\n")
		out, cancel := runTailer(t, path, true, 20*time.Millisecond)
		defer cancel()

		// tailer starts at EOF; give it time to settle
		time.Sleep(60 * time.Millisecond)
		appendFile(t, path, "new line\n")

		lines := collect(t, out, 500*time.Millisecond)
		var contents []string
		for _, l := range lines {
			contents = append(contents, l.Content)
		}
		assert.Contains(t, contents, "new line",
			"should receive the appended line")
	})
}

func TestRun_FollowTrue_SkipsExistingContent(t *testing.T) {
	t.Run("should not emit pre-existing lines when follow is true", func(t *testing.T) {
		path := writeFile(t, "live.log", "old line\n")
		out, cancel := runTailer(t, path, true, 20*time.Millisecond)
		defer cancel()

		lines := collect(t, out, 150*time.Millisecond)
		for _, l := range lines {
			assert.NotEqual(t, "old line", l.Content,
				"should not emit pre-existing content when follow=true")
		}
	})
}

// ---------------------------------------------------------------------------
// Rotation detection
// ---------------------------------------------------------------------------

func TestRun_Rotation(t *testing.T) {
	t.Run("should emit lines from the new file after log rotation is detected", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log")

		// write initial file
		require.NoError(t, os.WriteFile(path, []byte("before rotation\n"), 0600))

		out, cancel := runTailer(t, path, true, 20*time.Millisecond)
		defer cancel()

		// let tailer settle at EOF
		time.Sleep(60 * time.Millisecond)

		// simulate logrotate: rename old file, create new one
		require.NoError(t, os.Rename(path, filepath.Join(dir, "app.log.1")))
		require.NoError(t, os.WriteFile(path, []byte("after rotation\n"), 0600))

		lines := collect(t, out, 500*time.Millisecond)
		var contents []string
		for _, l := range lines {
			contents = append(contents, l.Content)
		}
		assert.Contains(t, contents, "after rotation",
			"should read from the new file after rotation")
	})
}

// ---------------------------------------------------------------------------
// Truncation detection
// ---------------------------------------------------------------------------

func TestRun_Truncation(t *testing.T) {
	t.Run("should emit lines from beginning of file when file is truncated", func(t *testing.T) {
		path := writeFile(t, "app.log", "original content\n")

		out, cancel := runTailer(t, path, true, 20*time.Millisecond)
		defer cancel()

		time.Sleep(60 * time.Millisecond)

		// truncate in-place (copytruncate style)
		require.NoError(t, os.WriteFile(path, []byte("after truncation\n"), 0600))

		lines := collect(t, out, 500*time.Millisecond)
		var contents []string
		for _, l := range lines {
			contents = append(contents, l.Content)
		}
		assert.Contains(t, contents, "after truncation",
			"should read from the beginning of the truncated file")
	})
}

// ---------------------------------------------------------------------------
// Graceful shutdown
// ---------------------------------------------------------------------------

func TestRun_GracefulShutdown(t *testing.T) {
	t.Run("should stop emitting lines when context is cancelled", func(t *testing.T) {
		path := writeFile(t, "app.log", "")
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan tailer.RawLine, 64)
		tl := tailer.New(path, true, 20*time.Millisecond)

		done := make(chan struct{})
		go func() {
			tl.Run(ctx, out)
			close(out)
			close(done)
		}()

		cancel()
		select {
		case <-done:
			// Run returned — success
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Run did not return after context cancellation")
		}
	})
}

// ---------------------------------------------------------------------------
// Large line support
// ---------------------------------------------------------------------------

func TestRun_LargeLines(t *testing.T) {
	t.Run("should return full content when line exceeds default 64KB scanner limit", func(t *testing.T) {
		// build a line larger than bufio.Scanner's default 64KB token limit
		bigLine := strings.Repeat("x", 128*1024) // 128 KB
		path := writeFile(t, "big.log", bigLine+"\n")

		out, cancel := runTailer(t, path, false, 10*time.Millisecond)
		defer cancel()

		lines := collect(t, out, time.Second)
		require.NotEmpty(t, lines)
		assert.Equal(t, len(bigLine), len(lines[0].Content))
	})
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestRun_NonexistentFile_ReturnsImmediately(t *testing.T) {
	t.Run("should return without emitting when the file does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does_not_exist.log")
		out, cancel := runTailer(t, path, false, 10*time.Millisecond)
		defer cancel()

		// Tailer should exit quickly and close the output channel; collect will
		// return an empty slice once the close propagates.
		lines := collect(t, out, 500*time.Millisecond)
		assert.Empty(t, lines, "no lines should be emitted for a missing file")
	})
}

func TestRun_Stdin_ReadsLinesFromOSPipe(t *testing.T) {
	t.Run("should read lines from stdin when path is empty", func(t *testing.T) {
		// Replace os.Stdin with the read end of a pipe so runStdin can consume it.
		r, w, err := os.Pipe()
		require.NoError(t, err)
		origStdin := os.Stdin
		os.Stdin = r
		t.Cleanup(func() {
			os.Stdin = origStdin
			r.Close()
		})

		go func() {
			defer w.Close()
			_, _ = w.WriteString("alpha\nbeta\ngamma\n")
		}()

		out, cancel := runTailer(t, "", false, 10*time.Millisecond)
		defer cancel()

		lines := collect(t, out, 500*time.Millisecond)
		require.Len(t, lines, 3)
		assert.Equal(t, "alpha", lines[0].Content)
		assert.Equal(t, "stdin", lines[0].Source)
		assert.Equal(t, int64(3), lines[2].LineNum)
	})
}
