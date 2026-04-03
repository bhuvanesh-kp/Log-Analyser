package parser

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"log_analyser/internal/tailer"
)

// Nginx combined log format:
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" [$request_time]
//
// The trailing $request_time field is optional (not present in the default
// combined format without the add_header directive).
var nginxRe = regexp.MustCompile(
	`^(\S+)\s+-\s+\S+\s+` + // remote_addr - remote_user
		`\[([^\]]+)\]\s+` + // [time_local]
		`"(\w+)\s+(\S+)\s+\S+"\s+` + // "METHOD path proto"
		`(\d+)\s+` + // status
		`(\d+|-)\s+` + // body_bytes_sent (- allowed for 304 etc.)
		`"[^"]*"\s+` + // referer (ignored)
		`"[^"]*"` + // user_agent (ignored)
		`(?:\s+([\d.]+))?$`, // optional request_time (seconds)
)

// nginxTimeLayout matches nginx's default time_local format.
const nginxTimeLayout = "02/Jan/2006:15:04:05 -0700"

type nginxParser struct{}

func (p *nginxParser) Name() string { return "nginx" }

func (p *nginxParser) Parse(raw tailer.RawLine) (ParsedEvent, bool) {
	line := strings.TrimSpace(raw.Content)
	if line == "" {
		return ParsedEvent{}, false
	}

	m := nginxRe.FindStringSubmatch(line)
	if m == nil {
		return ParsedEvent{}, false
	}
	// m indices: 0=full, 1=host, 2=time, 3=method, 4=path, 5=status, 6=bytes, 7=req_time
	host := m[1]
	ts, _ := time.Parse(nginxTimeLayout, m[2])
	method := m[3]
	path := m[4]
	status, _ := strconv.Atoi(m[5])
	var bytesVal int64
	if m[6] != "-" {
		bytesVal, _ = strconv.ParseInt(m[6], 10, 64)
	}
	var latency time.Duration
	if m[7] != "" {
		secs, err := strconv.ParseFloat(m[7], 64)
		if err == nil {
			latency = time.Duration(secs * float64(time.Second))
		}
	}

	return ParsedEvent{
		Timestamp:  ts,
		Source:     raw.Source,
		Host:       host,
		Method:     method,
		Path:       path,
		StatusCode: status,
		Bytes:      bytesVal,
		Latency:    latency,
		Raw:        raw.Content,
	}, true
}
