package parser

import (
	"sync"

	"log_analyser/internal/tailer"
)

// autoParser tries each format in priority order on the first line it sees.
// Once any parser succeeds, it locks to that parser for all subsequent lines.
//
// Detection order: nginx → syslog → json → apache
//   - nginx before apache: apache regex is a strict subset of nginx
//   - syslog before json: a syslog message body may be valid JSON; the more
//     specific format must win
type autoParser struct {
	mu       sync.Mutex
	locked   Parser   // non-nil once a format has been detected
	probes   []Parser // candidates tried in order until one matches
}

func newAutoParser() *autoParser {
	return &autoParser{
		probes: []Parser{
			&nginxParser{},
			&syslogParser{},
			&jsonParser{},
			&apacheParser{},
		},
	}
}

func (p *autoParser) Name() string { return "auto" }

func (p *autoParser) Parse(raw tailer.RawLine) (ParsedEvent, bool) {
	p.mu.Lock()
	locked := p.locked
	p.mu.Unlock()

	// fast path — already locked to a format
	if locked != nil {
		return locked.Parse(raw)
	}

	// slow path — probe all candidates
	for _, candidate := range p.probes {
		event, ok := candidate.Parse(raw)
		if ok {
			p.mu.Lock()
			if p.locked == nil { // double-checked: another goroutine may have locked first
				p.locked = candidate
			}
			locked = p.locked
			p.mu.Unlock()
			return locked.Parse(raw) // re-parse with the winner (in case another goroutine locked to a different candidate first)
		}
	}

	return ParsedEvent{}, false
}
