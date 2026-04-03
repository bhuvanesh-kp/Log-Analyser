package parser

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"log_analyser/internal/tailer"
)

// RFC5424 syslog format:
//
//	<PRI>VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA MSG
//
// Example:
//
//	<134>1 2026-04-01T12:00:00Z webhost myapp 1234 - - User login successful
var syslogRe = regexp.MustCompile(
	`^<(\d+)>\d+\s+` + // <PRI>VERSION
		`(\S+)\s+` + // TIMESTAMP
		`(\S+)\s+` + // HOSTNAME
		`\S+\s+` + // APP-NAME (ignored)
		`\S+\s+` + // PROCID (ignored)
		`\S+\s+` + // MSGID (ignored)
		`\S+`, // STRUCTURED-DATA (ignored; MSG not captured)
)

// priToLevel maps a syslog PRI severity (PRI % 8) to a log level string.
// RFC5424 severities: 0=emerg,1=alert,2=crit,3=err,4=warn,5=notice,6=info,7=debug
func priToLevel(pri int) string {
	switch pri % 8 {
	case 0, 1, 2: // emergency, alert, critical
		return "fatal"
	case 3: // error
		return "error"
	case 4: // warning
		return "warn"
	case 5, 6: // notice, informational
		return "info"
	case 7: // debug
		return "debug"
	default:
		return "info"
	}
}

type syslogParser struct{}

func (p *syslogParser) Name() string { return "syslog" }

func (p *syslogParser) Parse(raw tailer.RawLine) (ParsedEvent, bool) {
	line := strings.TrimSpace(raw.Content)
	if line == "" || line[0] != '<' {
		return ParsedEvent{}, false
	}

	m := syslogRe.FindStringSubmatch(line)
	if m == nil {
		return ParsedEvent{}, false
	}
	// m indices: 0=full, 1=pri, 2=timestamp, 3=hostname
	pri, _ := strconv.Atoi(m[1])
	ts, _ := time.Parse(time.RFC3339, m[2])

	return ParsedEvent{
		Timestamp: ts,
		Source:    raw.Source,
		Host:      m[3],
		Level:     priToLevel(pri),
		Raw:       raw.Content,
	}, true
}
