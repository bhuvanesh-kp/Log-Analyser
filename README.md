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

## Design Decisions

Decisions, trade-offs, and rejected alternatives recorded as components are built.

---

### `internal/config` — Configuration Loading

**Proposal:** Viper-backed loader with three entry points (`LoadFile`, `LoadEnv`, `Load`), typed secret rejection, and a separate `Validate()` step.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Config library | `github.com/spf13/viper` | Handles YAML parsing, env-var binding, and precedence merging out of the box; avoids hand-rolling flag→file→default resolution |
| Three public loaders | `LoadFile` / `LoadEnv` / `Load` | Each load path is independently testable; CLI wires `Load`, tests can use `LoadEnv` without touching the filesystem |
| Secret rejection | `checkForFileSecrets()` runs **before** `setupEnv()` in `Load` | Any non-empty `alerters.webhook.secret` value at that point must have come from the file — env vars haven't been bound yet; avoids false positives |
| Typed error for secret-in-file | `*SecretInFileError` with `IsSecretInFileError()` helper | Callers can distinguish this specific error from generic I/O errors without string matching; wraps cleanly with `errors.As` |
| Nested env-var binding | Explicit `v.BindEnv("alerters.webhook.secret", "LOG_ANALYSER_WEBHOOK_SECRET")` per nested key | Viper's `AutomaticEnv` resolves `LOG_ANALYSER_FORMAT` → `format` but cannot map `LOG_ANALYSER_WEBHOOK_SECRET` → `alerters.webhook.secret` without explicit binding |
| `Default()` is viper-free | Returns a plain `*Config` literal with hardcoded values | Tests that only need a valid base config do not spin up a viper instance; no file I/O, no env reads |
| `Validate()` is a separate method | Called explicitly after loading, not inside the loaders | Lets callers mutate the config between load and validate (e.g., CLI flag overrides); loaders stay pure data mappers |
| Validation style | Early-return per rule, plain `fmt.Errorf` strings | One error at a time is enough for a CLI tool; accumulating all errors adds complexity with minimal UX gain |

**Rejected:** Merging `Validate()` into the loaders — forces callers to suppress validation when they only need partial config (e.g., loading defaults for help text display).

**Rejected:** `mapstructure` auto-decode from viper — `extract()` maps keys explicitly, which makes the key names visible in one place and avoids silent field-name mismatches when struct tags differ from viper keys.

---

### `pkg/ringbuf` — Generic Circular Buffer

**Proposal:** A fixed-capacity, generic `RingBuf[T any]` that overwrites the oldest entry when full.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Generic type parameter | `RingBuf[T any]` | Reusable across `EventCount`, future types; minor added complexity vs a hand-typed struct |
| Synchronization | None — caller owns the lock | Keeps the buffer reusable and free of hidden contention; `Window` wraps it with `sync.RWMutex` |
| `Slice()` return | Always a full copy | Callers (stats functions) iterate without holding a lock; mutations to the slice never corrupt internal state |
| `At(i)` out-of-bounds | **Panic** | Consistent with Go slice semantics; the analyzer always guards with `Len()` before calling `At()` — a checked no-op return `(T, bool)` would hide logic bugs |
| Overwrite on full | Oldest entry silently overwritten | Matches sliding-window semantics: old data expires naturally; no blocking, no error return |

**Rejected:** A typed `EventCountRingBuf` — works but would need to be duplicated for any future ring-buffer user.

---

### `internal/window` — Thread-Safe Sliding Window

**Proposal:** A thin `sync.RWMutex` wrapper around `RingBuf[counter.EventCount]`.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Locking strategy | `RWMutex`: write lock on `Push`, read lock on `Snapshot` | Multiple readers (analyzer, metrics) can snapshot concurrently; only the counter goroutine writes |
| `Snapshot()` return | Full copy of the slice | Decouples stats computation from the lock; stats functions are pure and can run without holding any mutex |
| Package boundary | `window` is separate from `counter` | `counter` defines `EventCount`; `window` imports `counter` but not vice versa — avoids circular import |
| Capacity unit | Number of buckets (not time) | Caller controls resolution; `60 buckets × 1 s/bucket = 60 s window` is explicit in config |

---

### `internal/window/stats` — Pure Statistical Functions

**Proposal:** Free functions over `[]counter.EventCount` — no receivers, no state, no locks.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Computation timing | Eagerly recomputed from a fresh snapshot on every analyzer tick | Avoids staleness bugs; window size ≤ 3600 entries, recompute takes < 1 ms — not worth caching |
| No cached stats | No rolling accumulators | Simplicity wins; incremental mean/variance (Welford) would save ~microseconds but adds mutable state and edge-case bugs on wrap |
| `StdDev` variant | **Population** stddev (divide by N) | Window buckets represent the complete observed population, not a sample drawn from a larger one |
| `P99Latency` algorithm | Sort-based, nearest-rank formula | Exact result; O(n log n) over ≤ 3600 elements is negligible; reservoir sampling or T-digest would add complexity for no measurable gain here |
| `ErrorRate` aggregation | **Weighted** by `Total` across buckets | A bucket with 1000 requests should outweigh one with 10; simple average would skew results during traffic ramps |
| Zero-total bucket handling | Excluded from weighted error-rate denominator | Prevents division-by-zero and avoids ghost 0% buckets diluting the rate during silence periods |

**Rejected:** Pre-computing stats inside `Window.Push()` — creates coupling between the window and stats logic, and means stats are re-derived from partial data during warmup.

---

### `internal/counter` — Bucket Aggregation

**Proposal:** A `Counter` struct with a `Run(ctx, in, out)` blocking method. A `time.Ticker` fires every `BucketDuration`; on each tick the current bucket is flushed as an `EventCount` and a fresh empty bucket starts.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Flush trigger | `time.Ticker` every `BucketDuration` | Decoupled from event rate; fires even during silence — emits a zero-total bucket which the analyzer needs for silence detection |
| P99 within bucket | Collect all `Latency` values in a `[]time.Duration`, sort at flush | Exact result; a 1s bucket at 10k req/s = 10k values, sort ≈ 1ms — acceptable. Reservoir sampling saves memory but adds complexity for marginal gain |
| Error classification | HTTP: 4xx+5xx; non-HTTP: `Level == "error"` or `"fatal"` | Uniform coverage across log formats without separate config per format |
| Zero-event flush | Flush `Total=0` bucket on every tick regardless of event count | Silence detection in the analyzer requires zero-total buckets in the window — skipping empty ticks would mask silence |
| Counter config | Takes only `BucketDuration` from the main `Config` | Minimal coupling; counter is unaware of window size, thresholds, or alerting config |
| `Run` method signature | `Run(ctx, in <-chan ParsedEvent, out chan<- EventCount)` — blocking | Consistent with the pipeline goroutine pattern; caller controls scheduling; context cancellation triggers drain-then-close |
| Shutdown behaviour | On `ctx.Done()`: drain remaining in-flight events, then flush the final partial bucket | Guarantees no events are silently dropped on SIGTERM; final partial bucket gives the analyzer the most recent data |
| `time.Ticker` on Windows | No special handling — bucket duration is 1s, ticker resolution is ~15ms | 15ms jitter on a 1s boundary is negligible for anomaly detection; no OS-specific code needed |

**Rejected:** Flushing only when `in` is empty — creates variable-length buckets that break the sliding window's time assumptions and make baseline calculations incorrect.

**Rejected:** Computing error rate and p99 incrementally inside the event loop — avoids an extra allocation but makes the flush path stateful and harder to test in isolation.

---

### `internal/tailer` — File Tailing and Rotation

**Proposal:** A poll-based file tailer with a `Run(ctx, out)` blocking method. Seeks to EOF on startup (`follow=true`), reads all available lines on each poll, detects rotation via `os.SameFile` + size shrink, and supports stdin as a special case.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| File watching | Poll-based (`time.Sleep(pollInterval)`) | No CGO, no OS-specific APIs; `inotify` is Linux-only, `ReadDirectoryChangesW` needs CGO — polling works identically on Windows, Linux, and macOS |
| Rotation detection | `os.SameFile(oldStat, newStat)` | Uses volume serial number + file index on Windows — detects rename-based rotation without CGO; standard library only |
| Truncation detection | `currentStat.Size() < lastOffset` | Handles `copytruncate` mode where logrotate truncates the file in-place rather than renaming |
| Line reading | `bufio.Scanner` with custom split func | Default 64KB scanner limit is too small for some log lines; custom split also strips `\r` so CRLF Windows logs are handled transparently |
| Seek on open | Seek to EOF when `follow=true`, beginning when `false` | `follow=true` mimics `tail -f` — avoids reprocessing MB of existing history on startup |
| Rotation re-seek | Seek to beginning of new file | After rotation the new file starts fresh — seeking to end would skip lines written between rotation detection and reopen |
| Stdin support | `path == ""` → `os.Stdin` | No rotation check, no seek, no polling — read until EOF or ctx cancel |
| Line numbering | `lineNum int64` incremented per line, reset on rotation | Aids debugging; allows correlating alerts back to specific source lines |
| `out` ownership | Caller owns, `Run` never closes | Consistent with counter and pipeline channel ownership pattern |
| Poll interval on Windows | Default 100ms from config | Windows ticker resolution is ~15ms; 100ms poll is well above that — no jitter concern |

**Rejected:** `fsnotify` library — adds a dependency, uses OS-specific backends internally, still requires polling fallback on network filesystems. Pure polling is simpler and sufficient for 100ms latency requirements.

**Rejected:** `bufio.Reader.ReadString('\n')` — does not handle lines exceeding the internal buffer; Scanner with a custom split is more robust.

---

### `internal/parser` — Log Format Parsing

**Proposal:** A `Parser` interface with a registry and auto-detect strategy. A goroutine pool (`Pool`) fans one input channel across N workers. Four concrete parsers (nginx, apache, JSON, syslog) plus an auto-detect wrapper that locks to the first matching format.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| `Parse` return signature | `(ParsedEvent, bool)` — no `error` | Unparseable lines are a normal condition (comments, blank lines, partial writes); returning an error on every miss would force callers to pattern-match errors rather than just checking the bool |
| Drop vs forward unparseable | **Drop silently**, increment a metric counter | Forwarding a half-parsed event would corrupt counter aggregations (wrong error rate, zero latency skewing p99); the raw line is always logged at `--verbose` |
| Auto-detect locking | Lock to first format that matches **any** line | Log files are homogeneous — line 1 format == all subsequent lines; locking eliminates per-line overhead of trying all parsers after warmup |
| Auto-detect order | `nginx → syslog → json → apache` | More-specific formats win: a syslog message body may be valid JSON, so syslog must be tried before the generic JSON parser; apache is last because its regex is a strict subset of nginx |
| Apache vs nginx regex | Apache shares the **nginx regex** with optional referer/user-agent groups absent | Avoids maintaining two nearly-identical regexes; the only difference is the trailing `"$referer" "$ua"` groups which are optional in the shared pattern |
| JSON field aliases | Hardcoded alias list: `timestamp`/`time`/`ts`/`@timestamp`, `level`/`severity`/`lvl`, `latency_ms`/`duration_ms`/`elapsed_ms` | Covers the 95% case (Logrus, Zap, structlog, ECS) without a configurable field map; YAGNI — adding config later is straightforward |
| Latency unit convention | JSON numeric latency fields assumed **milliseconds**; nginx `$request_time` is **seconds** (3 decimal places) | Milliseconds is the most common unit in structured logging libraries; nginx's floating-point seconds is documented and unambiguous |
| Pool worker count | Default `runtime.NumCPU()`, configurable via `Config.Workers` | Regex matching is CPU-bound; parallelism helps on multi-core; single-CPU machines get 1 worker with no contention overhead |
| Pool shutdown | Workers drain `in` until closed, `sync.WaitGroup` closes `out` after last worker exits | Consistent with counter and tailer ownership model; caller detects completion via closed `out` channel |
| Registry | `map[string]func() Parser` populated in `init()` | Each call to `NewParser` gets a fresh instance — avoids shared mutable state between pool workers calling the same parser |

**Rejected:** Per-line auto-detect (try all parsers on every line) — 4× regex overhead on every line indefinitely; unnecessary after the first match locks the format.

**Rejected:** Configurable JSON field map — adds config complexity and a validation surface for a feature that hardcoded aliases cover adequately in v1.

---

### `internal/analyzer` — Anomaly Detection

**Proposal:** A single-goroutine `Analyzer` that receives `EventCount` buckets, pushes them into the sliding window, evaluates all five detection rules, enforces per-kind cooldown, and emits `Anomaly` values.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Evaluation trigger | **One eval per `EventCount`** — no separate ticker | The counter already fires every 1s; the analyzer simply evaluates on each received bucket; eliminates an extra goroutine and avoids a second timer that could drift relative to the counter |
| Silence detection | **Tail scan** — count consecutive zero-total buckets from newest to oldest | Correctly captures "the last N seconds were silent" rather than "window mean is low"; a traffic burst an hour ago must not mask a current outage |
| Cooldown state | `map[AnomalyKind]time.Time` (last-fired timestamp per kind) | Zero value of `time.Time` means "never fired"; `time.Since(zero)` is always > cooldown — no special init code needed |
| Baseline guard | Skip all rules when `window.Len() < MinBaselineSamples` | Prevents false positives at startup when the window contains only 1–2 buckets and mean is artificially low or high |
| Severity assignment | Hardcoded ratio thresholds inside `checkRate`/`checkLatency` | Operators tune *trigger* thresholds via config; severity is a derived signal; making severity thresholds configurable adds a UI surface with negligible operator value in v1 |
| Baseline=0 guard | `SpikeRatio = 0` when `BaselineValue == 0` | Avoids division-by-zero and misleading infinity values during early warmup or after extended silence |
| Host flood check | Iterate `current.ByHost`, find max count | O(hosts per bucket) — typically O(10–100) entries; no sort needed, just one linear scan |
| Detection mode | `"ratio"` (mean × N) or `"sigma"` (mean + k × stddev), selected by `Config.DetectionMethod` | Ratio mode is intuitive and operator-tuneable; sigma mode is statistically principled for bursty traffic; both are implemented, operator picks via config |

**Severity thresholds (hardcoded):**
- `rate_spike`: Warning if `SpikeRatio < 10`, Critical if `≥ 10`
- `latency_spike`: Warning if `SpikeRatio < 5`, Critical if `≥ 5`
- `error_surge`: always Warning
- `host_flood`: always Critical
- `silence`: always Critical

**Rejected:** A separate evaluation ticker — would require synchronising with the counter ticker to avoid double-evaluating the same window state; one-eval-per-bucket is simpler and correct.

**Rejected:** Per-kind cooldown channels or timers — a simple `map[AnomalyKind]time.Time` is sufficient; the overhead of real timers is not justified.

---

### `internal/alerter` — Alert Delivery

**Proposal:** An `Alerter` interface with four implementations (console, webhook, file, multi-fanout). `MultiAlerter` fans out to all enabled alerters concurrently via goroutines. Cleanup is opt-in via `io.Closer` type assertion rather than mandated by the interface.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Interface signature | `Send(ctx context.Context, a analyzer.Anomaly) error` | Returns error so `MultiAlerter` and pipeline can log failures; context propagates cancellation into retry loops and HTTP calls |
| `Close()` in interface | **No** — opt-in via `io.Closer` type assertion | Only `FileAlerter` holds a resource (`*os.File`); forcing `Close()` on `ConsoleAlerter` and `WebhookAlerter` is meaningless boilerplate; pipeline type-asserts to `io.Closer` and calls `Close()` only if the alerter implements it |
| JSON serialization | Add json struct tags directly to `analyzer.Anomaly` | No wrapper struct needed; `time.Time` marshals as RFC3339 natively; `AnomalyKind` and `SeverityLevel` are string types and marshal naturally; both webhook and file share the same representation |
| ConsoleAlerter output target | Injected `io.Writer` (defaults to `os.Stderr`) | Testable without capturing stdout/stderr; can be redirected by the caller without subprocess tricks |
| ConsoleAlerter color detection | Check `NO_COLOR` env var first; then check if writer is `*os.File` with `ModeCharDevice` bit via `Stat()` | Respects the `NO_COLOR` convention; auto-disables ANSI codes when piped to a file or another process; no extra dependency |
| ConsoleAlerter ANSI scheme | Warning = yellow (`\033[33m`), Critical = red (`\033[31m`), reset after each line | Two severity levels map cleanly to two universally supported ANSI colors |
| WebhookAlerter signing | `X-Signature-SHA256: sha256=<hex>` (HMAC-SHA256 of body) | GitHub-compatible convention; widely understood by webhook receivers; optional — skipped when no secret is configured |
| WebhookAlerter deduplication | `X-Alert-ID` request header + `"alert_id"` field in JSON body (UUID) | Allows idempotent retry handling at the receiver without tracking request state on the sender side |
| WebhookAlerter retry policy | 3 attempts, fixed backoff 1 s / 2 s / 4 s; abort on 4xx or `ctx.Done()` | Transient 5xx and network errors are retried; 4xx indicates a config problem (wrong URL/secret) — retrying wastes time and fills receiver logs |
| WebhookAlerter `http.Client` | Constructor-injected | Tests swap in a `httptest.Server`-backed client without starting a real server; no monkey-patching of global state |
| FileAlerter open mode | `O_APPEND \| O_CREATE \| O_WRONLY` | Append mode is atomic on POSIX; file is created if absent; no read permission needed; one JSON object per line (JSONL) |
| FileAlerter thread safety | `sync.Mutex` around `json.Encoder.Encode` + flush | Multiple goroutines in `MultiAlerter` could call `Send` concurrently; mutex ensures no interleaved JSON lines |
| MultiAlerter concurrency | One goroutine per alerter, `sync.WaitGroup` to collect results | Webhook retry can take up to ~7 s; running alerters sequentially would block console and file output for the duration; goroutine-per-alerter lets fast alerters complete immediately |
| MultiAlerter error handling | Collect all errors, log via `slog`, return `errors.Join(errs...)` | Errors from one alerter (e.g. webhook timeout) must not suppress delivery to others; caller sees a joined error if any alerter failed |
| Cooldown / deduplication | **Not in alerter** — handled by `analyzer.emit()` | Centralising cooldown in the analyzer means all three alerters see the same filtered stream; no risk of per-alerter dedup diverging |

**Rejected:** Sequential fan-out in `MultiAlerter` — a slow webhook retry would delay console and file alerts by up to 7 s, making the console output misleading during incidents.

**Rejected:** A persistent retry queue (channel + background goroutine) for webhook — adds significant complexity (drain on shutdown, bounded queue size, backpressure) for a failure mode that is better addressed by lowering `max_retries` or using a more reliable transport at the infrastructure level.

---

### `internal/pipeline` — Goroutine Wiring and Lifecycle

**Proposal:** A `Pipeline` struct that owns all inter-stage channels, constructs every component from `config.Config`, and exposes a single `Run(ctx) error` method. Ordered shutdown is implemented via wrapper goroutines that close each output channel after the upstream stage exits, propagating a drain wave downstream.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Channel ownership | Pipeline creates and closes all channels | No stage creates or closes a channel; eliminates double-close races and makes channel lifetimes visible in one place |
| Constructor vs Run split | `New(cfg, alerter)` is pure setup; `Run(ctx)` starts goroutines | `New` is safe to call in tests without starting goroutines; `Run` is the only place goroutines are spawned — easy to reason about lifetime |
| Ordered shutdown | Wrapper goroutines call `defer close(outputChan)` after each stage's `Run` returns | Each stage's `Run` already drains its input before returning; wrapping with `defer close` chains the drain signal downstream without modifying any stage's code |
| Stage channel close responsibility | Pipeline wrapper goroutines, not the stages themselves | Existing stages (`counter`, `analyzer`) have "caller owns the channel" semantics; changing them would break their standalone tests; wrapper pattern is additive with no changes to existing code |
| Alert loop | `for a := range anomC` in its own goroutine | `range` blocks until the channel is closed — guarantees every anomaly queued before shutdown is delivered before `Run` returns |
| `io.Closer` for FileAlerter | Type-assert alerter to `io.Closer` after alert loop exits; call `Close()` if implemented | Keeps `io.Closer` out of the `Alerter` interface while still releasing file handles cleanly at shutdown |
| Error handling in `Run` | Hard errors (e.g. tailer cannot open file) returned from `Run`; soft errors (alert delivery failures) logged via `slog` | A tailer open failure is unrecoverable — the pipeline cannot produce any output; alert delivery failures are transient and already retried by `WebhookAlerter` |
| Signal handling | Not in pipeline — `main.go` owns `signal.NotifyContext` | Pipeline only reacts to ctx cancellation; decoupled from OS signal mechanics; easier to test (just cancel a context) |
| Channel buffer sizes | `rawLines=1024`, `parsed=512`, `counts=64`, `anomalies=32` | Sized for realistic throughput ratios: parser pool is N× faster than counter (aggregation); counter emits 1 bucket/s so a small buffer is sufficient; anomalies are rare so 32 is ample |
| Parser pool context | `pool.Run` receives `ctx` but drains until `rawLines` closes | Ensures lines buffered before ctx cancel are not dropped; the tailer stops producing, the pool drains what remains |

**Shutdown cascade (no events dropped):**
```
ctx.Cancel()
  → tailer.Run returns       → close(rawLines)
  → pool drains rawLines     → close(parsed)
  → counter drains parsed, flushes final bucket → close(counts)
  → analyzer drains counts   → close(anomalies)
  → alertLoop drains anomalies, delivers all pending alerts → done
```

**Rejected:** Having each stage close its own output channel — would require changing the existing stage contracts, breaking their standalone tests, and creating ambiguity about who owns the channel in non-pipeline contexts (e.g. unit tests that wire channels manually).

**Rejected:** A single `sync.WaitGroup` over all goroutines with a shared context for shutdown — does not enforce ordering; the analyzer could exit before the counter flushes its final bucket, losing the last EventCount.

---

### `internal/metrics` — Prometheus Metrics Exposition

**Proposal:** A `Recorder` interface abstraction over Prometheus counters/gauges. Pipeline stages call `Recorder` methods (`IncLinesRead()`, `ObserveAnomaly(kind)`, etc.) for zero-cost instrumentation. A `PrometheusRecorder` implementation registers metrics on a custom (non-global) `prometheus.Registry`. A `NoopRecorder` is used when `--metrics-addr` is not set. An HTTP `Server` exposes `/metrics` with graceful shutdown.

| Decision | Choice | Trade-off / Rationale |
|----------|--------|-----------------------|
| Instrumentation abstraction | `Recorder` interface with `PrometheusRecorder` and `NoopRecorder` implementations | Pipeline stages call `rec.IncLinesRead()` — no direct prometheus imports outside the metrics package; `NoopRecorder` is zero-cost when metrics are disabled; no nil checks needed |
| Registry | Custom `prometheus.Registry` per `PrometheusRecorder`, not `prometheus.DefaultRegisterer` | Makes tests hermetic — no "already registered" panics across parallel test runs or multiple pipeline instances; each recorder owns its own collectors |
| Pipeline integration | `Pipeline.New(cfg, alerter, recorder)` — recorder passed as a constructor argument | Pipeline calls recorder methods inside `Run`; if metrics are disabled, `NoopRecorder{}` is passed — no conditional logic in pipeline code |
| Channel depth gauges | Pipeline calls `rec.SetChannelDepth(name, len(ch))` periodically | Metrics package holds no channel references — stays decoupled from pipeline internals; pipeline already owns the channels |
| HTTP server lifecycle | `Server` wraps `http.Server`; `ListenAndServe()` blocks in a goroutine; `Shutdown(ctx)` drains in-flight scrapes | Standard `http.Server` shutdown semantics; `main.go` starts the server and calls `Shutdown` in the deferred cleanup path |
| No server when disabled | If `--metrics-addr` is empty, `main.go` skips server creation entirely | Zero overhead; no listener, no goroutine, no port binding when metrics are not requested |
| Metric naming | `log_analyser_` prefix on all metrics | Standard Prometheus namespace convention; avoids collisions with other exporters on the same process |
| Label cardinality | `kind` on anomalies (5 values), `alerter` on alerts sent (3 values), `channel` on depth (4 values) | Low, bounded cardinality — no risk of label explosion; all label values are enum-like constants defined in the codebase |

**Metrics exposed:**

| Prometheus Name | Type | Labels | Source |
|-----------------|------|--------|--------|
| `log_analyser_lines_read_total` | Counter | — | Tailer (via pipeline) |
| `log_analyser_lines_parsed_total` | Counter | — | Parser pool (via pipeline) |
| `log_analyser_events_per_second` | Gauge | — | Counter stage (via pipeline) |
| `log_analyser_baseline_rate` | Gauge | — | Analyzer (via pipeline) |
| `log_analyser_anomalies_total` | Counter | `kind` | Analyzer (via pipeline) |
| `log_analyser_alerts_sent_total` | Counter | `alerter` | Alert loop (via pipeline) |
| `log_analyser_pipeline_channel_depth` | Gauge | `channel` | Pipeline (periodic) |

**Rejected:** Global `prometheus.DefaultRegisterer` — causes "already registered" panics in parallel tests; custom registry is strictly better for testability and multi-instance safety.

**Rejected:** Metrics middleware wrapping channels — would require metrics to own or wrap pipeline channels, creating tight coupling; explicit `Recorder` method calls are simpler and keep ownership clear.

**Rejected:** Push-based metrics via Pushgateway — adds an external dependency and an additional failure mode; pull-based `/metrics` endpoint is the Prometheus standard for long-running services.

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
