package tailer

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"time"
)

// RawLine is a single log line emitted by the tailer.
type RawLine struct {
	Content string    // log line with CR stripped
	Source  string    // file path or "stdin"
	ReadAt  time.Time // wall time the line was read
	LineNum int64     // 1-based; resets to 1 on each file open/rotation
}

// Tailer reads a file (or stdin) and emits one RawLine per log line.
type Tailer struct {
	path         string
	follow       bool
	pollInterval time.Duration
}

// New returns a Tailer for the given file path.
// path == "" reads from stdin.
// follow=true seeks to EOF on open (tail -f behaviour).
// follow=false reads from the beginning and returns at EOF.
func New(path string, follow bool, pollInterval time.Duration) *Tailer {
	return &Tailer{
		path:         path,
		follow:       follow,
		pollInterval: pollInterval,
	}
}

// Run reads lines from the configured source and sends them to out until ctx
// is cancelled. Run never closes out — the caller owns the channel.
func (t *Tailer) Run(ctx context.Context, out chan<- RawLine) {
	if t.path == "" {
		t.runStdin(ctx, out)
		return
	}
	t.runFile(ctx, out)
}

// ---------------------------------------------------------------------------
// stdin path
// ---------------------------------------------------------------------------

func (t *Tailer) runStdin(ctx context.Context, out chan<- RawLine) {
	scanner := newScanner(os.Stdin)
	var lineNum int64
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		lineNum++
		out <- RawLine{
			Content: stripCR(scanner.Text()),
			Source:  "stdin",
			ReadAt:  time.Now(),
			LineNum: lineNum,
		}
	}
}

// ---------------------------------------------------------------------------
// file path
// ---------------------------------------------------------------------------

func (t *Tailer) runFile(ctx context.Context, out chan<- RawLine) {
	f, err := openFile(t.path)
	if err != nil {
		return
	}
	defer f.Close()

	// seek to the correct start position
	var offset int64
	if t.follow {
		offset, err = f.Seek(0, 2) // seek to EOF
		if err != nil {
			return
		}
	}

	stat, err := f.Stat()
	if err != nil {
		return
	}

	var lineNum int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// seek to our known offset and drain all available lines
		if _, err := f.Seek(offset, 0); err != nil {
			return
		}
		scanner := newScanner(f)
		for scanner.Scan() {
			lineNum++
			// offset tracks bytes consumed: token bytes + 1 newline byte.
			// splitLines returns the token without '\n'; for CRLF the token
			// includes '\r', so len(token)+1 == len(raw_bytes_in_file). ✓
			offset += int64(len(scanner.Bytes())) + 1
			out <- RawLine{
				Content: stripCR(scanner.Text()),
				Source:  t.path,
				ReadAt:  time.Now(),
				LineNum: lineNum,
			}
		}

		if !t.follow {
			return
		}

		// check whether the file was rotated or truncated
		newStat, err := os.Stat(t.path)
		if err != nil {
			// file disappeared; wait for it to reappear
			if !t.sleep(ctx) {
				return
			}
			continue
		}

		// rotation: the path now points to a different file
		if !os.SameFile(stat, newStat) {
			f.Close()
			f, err = openFile(t.path)
			if err != nil {
				if !t.sleep(ctx) {
					return
				}
				continue
			}
			stat, _ = f.Stat()
			offset = 0
			lineNum = 0
			continue // read new file immediately, no sleep
		}

		// truncation: file shrank below our read position, or was overwritten
		// in-place with same-size content (mtime changed while size == offset)
		truncated := newStat.Size() < offset ||
			(newStat.Size() == offset && newStat.ModTime().After(stat.ModTime()))
		if truncated {
			offset = 0
			lineNum = 0
			stat = newStat
			continue // re-read from beginning immediately, no sleep
		}

		if !t.sleep(ctx) {
			return
		}
	}
}

// sleep waits for pollInterval or ctx cancellation.
// Returns false if ctx was cancelled.
func (t *Tailer) sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(t.pollInterval):
		return true
	}
}

// ---------------------------------------------------------------------------
// Scanner helpers
// ---------------------------------------------------------------------------

// newScanner returns a bufio.Scanner with a 1 MB token buffer. The custom
// split function handles both LF and CRLF line endings correctly.
func newScanner(f interface{ Read([]byte) (int, error) }) *bufio.Scanner {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1*1024*1024), 1*1024*1024)
	scanner.Split(splitLines)
	return scanner
}

// splitLines is a bufio.SplitFunc that advances past '\n' and returns the
// token without '\n'. A trailing '\r' is left in the token so that stripCR
// can remove it, which also makes the advance count correct for CRLF files.
func splitLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// stripCR removes a trailing '\r' from a line (CRLF normalisation).
func stripCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
