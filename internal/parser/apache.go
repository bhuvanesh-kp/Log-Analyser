package parser

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"log_analyser/internal/tailer"
)

// Apache Common Log Format (CLF):
//
//	$host - $user [$time] "$request" $status $bytes
//
// No referer, no user-agent, no request_time — this is the strict CLF subset.
// The regex is deliberately tighter than the nginx pattern (no trailing groups)
// so it does not accidentally match nginx combined lines.
var apacheRe = regexp.MustCompile(
	`^(\S+)\s+-\s+\S+\s+` + // host - user
		`\[([^\]]+)\]\s+` + // [time]
		`"(\w+)\s+(\S+)\s+\S+"\s+` + // "METHOD path proto"
		`(\d+)\s+` + // status
		`(\d+|-)$`, // bytes (- allowed)
)

type apacheParser struct{}

func (p *apacheParser) Name() string { return "apache" }

func (p *apacheParser) Parse(raw tailer.RawLine) (ParsedEvent, bool) {
	line := strings.TrimSpace(raw.Content)
	if line == "" {
		return ParsedEvent{}, false
	}

	m := apacheRe.FindStringSubmatch(line)
	if m == nil {
		return ParsedEvent{}, false
	}
	// m indices: 0=full, 1=host, 2=time, 3=method, 4=path, 5=status, 6=bytes
	host := m[1]
	ts, _ := time.Parse(nginxTimeLayout, m[2]) // same timestamp format as nginx
	method := m[3]
	path := m[4]
	status, _ := strconv.Atoi(m[5])
	var bytesVal int64
	if m[6] != "-" {
		bytesVal, _ = strconv.ParseInt(m[6], 10, 64)
	}

	return ParsedEvent{
		Timestamp:  ts,
		Source:     raw.Source,
		Host:       host,
		Method:     method,
		Path:       path,
		StatusCode: status,
		Bytes:      bytesVal,
		Raw:        raw.Content,
	}, true
}
