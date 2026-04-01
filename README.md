# Real-Time Log Anomaly Detector

A Go tool that tails log files in real-time, builds a statistical baseline using a sliding window, and fires alerts the moment traffic spikes, error rates surge, latency climbs, or the log stream goes silent.

---

## Use Cases

| Scenario | What Fires |
|----------|-----------|
| DDoS attack — 50 req/sec jumps to 3000 req/sec | `rate_spike` (CRITICAL) |
| Brute-force login — single IP sends 80% of requests with 75% 401s | `host_flood` + `error_surge` |
| App crash — log rate drops to zero for 15 seconds | `silence` (CRITICAL) |
| Bad deploy — p99 DB latency spikes from 50 ms to 800 ms | `latency_spike` |
| Scraper bot — 5 rotating IPs flood traffic 15× above baseline | `rate_spike` |
| Hourly cron burst — known spike, suppress repeat alerts | cooldown suppression |

---

## High-Level Architecture

```
+-----------------------------------------------------------------------------+
|                     REAL-TIME LOG ANOMALY DETECTOR                          |
+-----------------------------------------------------------------------------+
|                                                                             |
|  +------------------+    +------------------+    +----------------------+   |
|  |   Log Sources    |    |   Config / CLI   |    |  Prometheus /metrics |   |
|  |                  |    |                  |    |     (optional)       |   |
|  |  - File (tail)   |    |  - YAML file     |    +----------+-----------+   |
|  |  - stdin (pipe)  |    |  - CLI flags     |               | scrape        |
|  |  - log rotation  |    |  - Env vars      |               |               |
|  +--------+---------+    +--------+---------+               |               |
|           |                       |                         |               |
|           v                       v                         |               |
|  +-------------------------------------------------------------------+      |
|  |                        INGESTION LAYER                            |<-----+
|  |                                                                   |      |
|  |   Tailer goroutine                                                |      |
|  |   - Poll-based, 100ms interval, no CGO, cross-platform            |      |
|  |   - Detects file rotation via inode comparison                    |      |
|  |   - bufio.Scanner with custom split (handles lines > 64 KB)       |      |
|  +------------------------------+------------------------------------+      |
|                                 |                                           |
|                    chan RawLine (buffered 1024)                             | 
|                                 |                                           |
|                                 v                                           |
|  +-------------------------------------------------------------------+      |
|  |                         PARSING LAYER                             |      | 
|  |                                                                   |      |
|  |   Parser Pool  (N goroutines, default = NumCPU)                   |      |
|  |                                                                   |      |
|  |   +----------+  +----------+  +----------+  +----------+          |      |
|  |   |  nginx   |  |  apache  |  |   JSON   |  |  syslog  |          |      |
|  |   |  parser  |  |  parser  |  |  parser  |  |  parser  |          |      |
|  |   +----------+  +----------+  +----------+  +----------+          |      |
|  |                                                                   |      |
|  |   Auto-detect: Probe() returns confidence score [0,1] per parser  |      |
|  +------------------------------+------------------------------------+      |
|                                 |                                           |
|                  chan ParsedEvent (buffered 512)                            |
|                                 |                                           |
|                                 v                                           |
|  +-------------------------------------------------------------------+      |
|  |                      AGGREGATION LAYER                            |      |
|  |                                                                   |      |
|  |   Counter goroutine  (emits one EventCount per second)            |      |
|  |   - Total events/sec                                              |      |
|  |   - Breakdown: by HTTP status / source IP / URL path              |      |
|  |   - Error rate (4xx + 5xx / total)                                |      |
|  |   - p99 latency (reservoir sample, capped at 10 000 pts)          |      |
|  +------------------------------+------------------------------------+      |
|                                 |                                           |
|                   chan EventCount (buffered 64)                             |
|                                 |                                           |
|                                 v                                           |
|  +-------------------------------------------------------------------+      |
|  |                        ANALYSIS LAYER                             |      |
|  |                                                                   |      |
|  |   Analyzer goroutine                                              |      |
|  |                                                                   |      |
|  |   +-----------------------------------------------------------+   |      |
|  |   |           Sliding Window  (circular buffer)               |   |      |
|  |   |   60 x 1s buckets  |  O(1) push  |  zero allocs at warm   |   |      |
|  |   |   Baseline = rolling mean over window                     |   |      |
|  |   |   Two modes: ratio (mean x N)  or  sigma (mean + k*std)   |   |      |
|  |   +-----------------------------------------------------------+   |      |
|  |                                                                   |      |
|  |   Detection Rules:                                                |      |
|  |   - rate_spike    : rate > mean x spike_multiplier                |      |
|  |   - error_surge   : error_rate > threshold (default 5%)           |      |
|  |   - latency_spike : p99 > baseline_p99 x latency_multiplier       |      |
|  |   - host_flood    : single IP > host_flood_fraction of traffic    |      |
|  |   - silence       : zero events for N consecutive seconds         |      |
|  |                                                                   |      |
|  |   Per-kind cooldown timer  (default 30s, independently tracked)   |      |
|  |   Min baseline guard: no alerts until N buckets collected         |      |
|  +------------------------------+------------------------------------+      |
|                                 |                                           |
|                     chan Anomaly (buffered 32)                              |
|                                 |                                           |
|                                 v                                           |
|  +-------------------------------------------------------------------+      |
|  |                        ALERTING LAYER                             |      |
|  |                                                                   |      |
|  |   MultiAlerter  (fan-out to all enabled alerters concurrently)    |      |
|  |                                                                   |      |
|  |   +--------------+  +-------------------+  +-----------------+    |      |
|  |   |   Console    |  |      Webhook      |  |      File       |    |      |
|  |   |   Alerter    |  |      Alerter      |  |     Alerter     |    |      |
|  |   |              |  |                   |  |                 |    |      |
|  |   | Colored TTY  |  | HTTP POST JSON    |  | Append .jsonl   |    |      |
|  |   | output with  |  | HMAC-SHA256 sign  |  | Size-based      |    |      |
|  |   | severity     |  | Retry queue with  |  | rotation        |    |      |
|  |   | labels       |  | exponential delay |  |                 |    |      |
|  |   +--------------+  +-------------------+  +-----------------+    |      |
|  +-------------------------------------------------------------------+      |
|                                                                             |
|  +-------------------------------------------------------------------+      |
|  |                    LIFECYCLE / SIGNAL HANDLER                     |      |
|  |                                                                   |      |
|  |   SIGINT / SIGTERM --> context.Cancel()                           |      |
|  |                                                                   |      |
|  |   Ordered drain:  Tailer  ->  Parsers  ->  Counter                |      |
|  |                   -> Analyzer  ->  Alerters                       |      |
|  |                                                                   |      |
|  |   Each stage closes its output channel only after draining input  |      |
|  |   Guarantees: no log events silently dropped on shutdown          |      |
|  +-------------------------------------------------------------------+      |
|                                                                             |
+-----------------------------------------------------------------------------+
```

### Data Flow

```
RawLine --> ParsedEvent --> EventCount --> Anomaly --> Alert
 (text)     (structured)   (aggregated)   (detected)  (delivered)
```

### Layer Responsibilities

| Layer | Component | Responsibility |
|-------|-----------|---------------|
| Ingestion | Tailer | Poll file, detect rotation, emit raw lines |
| Parsing | Parser Pool | Regex-based format detection, normalize to ParsedEvent |
| Aggregation | Counter | 1-second buckets, compute error rate + p99 latency |
| Analysis | Analyzer + Window | Maintain baseline, evaluate rules, suppress duplicates |
| Alerting | MultiAlerter | Fan out to console / webhook / file; retry on failure |
| Observability | Metrics | Prometheus counters and gauges for every stage |

---

## Architecture

```
[Log File / stdin]
       │  chan RawLine (buf 1024)
       ▼
  [Tailer goroutine]          poll-based, handles rotation (logrotate-safe)
       │  chan RawLine (buf 1024)
       ▼
  [Parser Pool — N goroutines]  regex per format, auto-detect
       │  chan ParsedEvent (buf 512)
       ▼
  [Counter goroutine]           1-second buckets, p99 latency, by-host/status/path
       │  chan EventCount (buf 64)
       ▼
  [Analyzer goroutine]          circular-buffer sliding window + rule evaluation
       │  chan Anomaly (buf 32)
       ▼
  [MultiAlerter fanout]
       ├── Console alerter      colored terminal output
       ├── Webhook alerter      HTTP POST JSON + HMAC-SHA256 signing + retry queue
       └── File alerter         append JSON to file, size-based rotation
  [Metrics goroutine]   ← Prometheus /metrics endpoint (optional)
  [Signal handler]      ← SIGINT/SIGTERM → graceful drain, no events lost
```

### Ordered Shutdown
On SIGINT/SIGTERM the tailer stops first, then each stage drains its input channel before exiting. **No events are dropped on shutdown.**

---

## Project Structure

```
log_analyser/
├── cmd/loganalyser/
│   └── main.go                   # CLI entry point, signal handling, pipeline wiring
├── internal/
│   ├── config/config.go          # Config struct, YAML loader, env-var + flag binding
│   ├── tailer/tailer.go          # Real-time file/stdin reader, rotation-safe
│   ├── parser/
│   │   ├── parser.go             # Parser interface, registry, auto-detect
│   │   ├── nginx.go              # Nginx combined log format
│   │   ├── apache.go             # Apache CLF format
│   │   ├── json.go               # Structured JSON logs
│   │   └── syslog.go             # RFC5424 syslog
│   ├── counter/counter.go        # 1-second event bucketing, p99 latency
│   ├── window/
│   │   ├── window.go             # Circular-buffer sliding window (O(1) push)
│   │   └── stats.go              # Mean, stddev, percentile
│   ├── analyzer/analyzer.go      # All detection rules + per-kind cooldown
│   ├── alerter/
│   │   ├── alerter.go            # Alerter interface + MultiAlerter fanout
│   │   ├── console.go            # Colored terminal
│   │   ├── webhook.go            # HTTP POST + HMAC signing + retry
│   │   └── file.go               # JSON file alerter
│   ├── pipeline/pipeline.go      # Channel wiring, goroutine lifecycle
│   └── metrics/metrics.go        # Prometheus metrics
├── pkg/ringbuf/ringbuf.go        # Generic circular buffer
├── testdata/                     # Sample log files for testing
├── configs/default.yaml          # Default configuration
├── go.mod
├── Makefile
└── README.md
```

---

## Detection Rules

| Rule | Trigger | Anomaly Kind |
|------|---------|-------------|
| Rate spike | `current_rate > mean × spike_multiplier` | `rate_spike` |
| Error surge | `error_rate > error_rate_threshold` | `error_surge` |
| Latency spike | `p99 > baseline_p99 × latency_multiplier` | `latency_spike` |
| Host flood | single IP > `host_flood_fraction` of traffic | `host_flood` |
| Silence | zero events for N consecutive seconds | `silence` |

**Baseline:** rolling mean of the last 60 one-second buckets (configurable).
**Guard:** no alerts fire until `min_baseline_samples` (default 10) buckets are collected.
**Cooldown:** each anomaly kind has an independent suppression timer (default 30s).

### Sliding Window Algorithm
- Circular buffer of `EventCount` structs — **zero heap allocations after warmup**
- O(1) push, O(N) iteration
- Two detection modes: `ratio` (`mean × multiplier`) and `sigma` (`mean + k × stddev`)

---

## Tech Stack

| Package | Purpose |
|---------|---------|
| Go 1.22 stdlib (`os`, `bufio`, `regexp`, `sync`, `context`, `net/http`, `log/slog`) | Core logic — no CGO, cross-platform |
| `github.com/spf13/cobra` | CLI flags and subcommands |
| `github.com/spf13/viper` | YAML config + environment variable override |
| `github.com/google/uuid` | Alert IDs for deduplication |
| `github.com/prometheus/client_golang` | Optional Prometheus metrics endpoint |
| `github.com/stretchr/testify` | Test assertions |

**5 external dependencies. No CGO. No message-queue SDKs.**

---

## CLI Usage

```bash
# Tail an nginx log file
loganalyser -f /var/log/nginx/access.log --format nginx

# Read from stdin (pipe)
tail -f /var/log/syslog | loganalyser --format syslog

# Tune detection thresholds
loganalyser -f access.log \
  --spike-multiplier 5.0 \
  --error-rate 0.1 \
  --window 30s

# Send alerts to a webhook + write to file
loganalyser -f access.log \
  --webhook https://hooks.slack.com/... \
  --webhook-secret my-signing-secret \
  --alert-file alerts.jsonl

# Enable Prometheus metrics
loganalyser -f access.log --metrics-addr :9090
# curl http://localhost:9090/metrics
```

### All Flags

```
Input:
  -f, --file string            Log file to tail (default: stdin)
  -F, --follow                 Keep tailing after EOF (default: true)
      --format string          nginx|apache|json|syslog|auto (default: auto)
      --poll-interval duration File poll interval (default: 100ms)

Detection:
      --window duration           Sliding window size (default: 60s)
      --spike-multiplier float    Alert when rate > N× baseline (default: 3.0)
      --error-rate float          Alert when error fraction > this (default: 0.05)
      --host-flood float          Alert when one IP > this fraction (default: 0.5)
      --latency-multiplier float  Alert when p99 > N× baseline (default: 3.0)
      --silence int               Alert after N silent seconds (default: 30)
      --cooldown duration         Duplicate alert suppression (default: 30s)
      --min-baseline int          Buckets before alerting starts (default: 10)

Alerting:
      --no-color               Disable colored console output
      --webhook string         Webhook URL for HTTP POST alerts
      --webhook-secret string  HMAC-SHA256 signing key
      --alert-file string      Append JSON alerts to this file

Config & Observability:
  -c, --config string          YAML config file
      --metrics-addr string    Prometheus /metrics address (e.g. :9090)
      --workers int            Parser goroutine pool size (default: NumCPU)
  -v, --verbose                Debug logging
      --version                Print version and exit
```

---

## Configuration (`configs/default.yaml`)

```yaml
log_file: ""
format: "auto"
follow: true
poll_interval: "100ms"

window_size: "60s"
bucket_duration: "1s"

spike_threshold_multiplier: 3.0
error_rate_threshold: 0.05
host_flood_fraction: 0.5
latency_multiplier: 3.0
silence_threshold_seconds: 30
min_baseline_samples: 10
detection_method: "ratio"   # "ratio" | "sigma"

alert_cooldown: "30s"

alerters:
  console:
    enabled: true
    use_color: true
  webhook:
    enabled: false
    url: ""
    secret: ""
    timeout: "5s"
    max_retries: 3
  file:
    enabled: false
    path: "alerts.jsonl"
    format: "json"

metrics_addr: ""
```

> CLI flags override config file values. Environment variables (`LOG_ANALYSER_SPIKE_MULTIPLIER`, etc.) also override config.

---

## Prometheus Metrics

```
log_analyser_lines_read_total           Lines read from source
log_analyser_lines_parsed_total         Lines successfully parsed
log_analyser_lines_unparseable_total    Lines matching no parser
log_analyser_events_per_second          Current rate (gauge)
log_analyser_baseline_rate              Window mean rate (gauge)
log_analyser_anomalies_total            Anomalies fired, label: kind
log_analyser_alerts_sent_total          Alerts delivered, label: alerter
log_analyser_alerts_failed_total        Alerts exhausted retries
log_analyser_pipeline_channel_depth     Channel fill depth, label: channel
```

---

## Extending the Tool

### Add a new log format parser
1. Create `internal/parser/myformat.go` and implement the `Parser` interface (`Name()`, `Parse()`, `Probe()`)
2. Register it in `parser.go`'s `init()` — no other files change

### Add a new alerter (e.g., Slack, PagerDuty)
1. Create `internal/alerter/slack.go` and implement `Alerter` (`Name()`, `Send()`, `Close()`)
2. Add its config to `config.AlerterConfig` and instantiate it in `pipeline.New()`

### Add a new detection rule
1. Add an `AnomalyKind` constant in `analyzer.go`
2. Write a `check*` method on `*Analyzer`
3. Call it from `evaluate()` — cooldown and fanout work automatically

---

## Build & Test

```bash
make build            # go build → bin/loganalyser
make test             # go test ./... -race
make test-coverage    # coverage HTML report
make lint             # golangci-lint
make docker           # docker build
```

---

## Webhook Alert Payload

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "timestamp": "2026-04-01T12:00:01Z",
  "kind": "rate_spike",
  "severity": "critical",
  "message": "3000 req/sec (60.0× baseline of 50 req/sec)",
  "spike_ratio": 60.0,
  "current_rate_per_sec": 3000,
  "baseline_rate_per_sec": 50,
  "log_source": "/var/log/nginx/access.log"
}
```

Authenticated with `X-Log-Analyser-Signature: sha256=<hmac-hex>` when a secret is configured.
