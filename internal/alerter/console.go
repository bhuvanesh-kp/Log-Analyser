package alerter

import (
	"context"
	"fmt"
	"io"
	"os"

	"log_analyser/internal/analyzer"
)

// ConsoleAlerter writes formatted anomaly lines to an io.Writer.
type ConsoleAlerter struct {
	w     io.Writer
	color bool
}

// NewConsoleAlerter returns a ConsoleAlerter that writes to w.
// Set color=true to emit ANSI color codes; false for plain text.
func NewConsoleAlerter(w io.Writer, color bool) *ConsoleAlerter {
	return &ConsoleAlerter{w: w, color: color}
}

// NewConsoleAlerterAuto returns a ConsoleAlerter that auto-detects color
// support: disabled if NO_COLOR env var is set or if w is not a TTY.
func NewConsoleAlerterAuto(w io.Writer) *ConsoleAlerter {
	color := false
	if os.Getenv("NO_COLOR") == "" {
		if f, ok := w.(*os.File); ok {
			if fi, err := f.Stat(); err == nil {
				color = (fi.Mode() & os.ModeCharDevice) != 0
			}
		}
	}
	return &ConsoleAlerter{w: w, color: color}
}

func (c *ConsoleAlerter) Name() string { return "console" }

// Send formats and writes the anomaly. Always returns nil unless the writer fails.
func (c *ConsoleAlerter) Send(_ context.Context, a analyzer.Anomaly) error {
	label := "WARNING"
	if a.Severity == analyzer.SeverityCritical {
		label = "CRITICAL"
	}

	ts := a.DetectedAt.Format("15:04:05")

	if !c.color {
		_, err := fmt.Fprintf(c.w, "[%s] %s %-14s %s\n", label, ts, a.Kind, a.Message)
		return err
	}

	// ANSI: warning=yellow, critical=red, reset at end
	colorCode := "\033[33m" // yellow
	if a.Severity == analyzer.SeverityCritical {
		colorCode = "\033[31m" // red
	}
	_, err := fmt.Fprintf(c.w, "%s[%s] %s %-14s %s\033[0m\n", colorCode, label, ts, a.Kind, a.Message)
	return err
}
