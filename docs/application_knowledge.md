# Application Knowledge

Implementation reference for all components built in the log_analyser project.
Each section covers: purpose, data structures, every function, and key implementation notes.

---

## Table of Contents

1. [pkg/ringbuf — Generic Circular Buffer](#pkgringbuf--generic-circular-buffer)
2. [internal/parser — Log Format Parsing](#internalparser--log-format-parsing)
3. [internal/counter — Bucket Aggregation](#internalcounter--bucket-aggregation)
4. [internal/window — Sliding Window](#internalwindow--sliding-window)
5. [internal/window/stats — Statistical Functions](#internalwindowstats--statistical-functions)
6. [internal/analyzer — Anomaly Detection](#internalanalyzer--anomaly-detection)
7. [internal/config — Configuration Loading](#internalconfig--configuration-loading)
8. [internal/tailer — File Tailing and Rotation](#internaltailer--file-tailing-and-rotation)

---

## pkg/ringbuf — Generic Circular Buffer

**File:** `pkg/ringbuf/ringbuf.go`

**Purpose:** A fixed-capacity, zero-allocation-after-warmup circular buffer. Forms the foundation of the sliding window. Not concurrency-safe — the caller controls synchronization.

### Data Structure

```
RingBuf[T any]
  buf   []T   — backing array, pre-allocated at construction, never resized
  head  int   — index of the NEXT write slot (advances with each Push)
  count int   — number of valid entries (capped at len(buf))
```

The physical layout after wrapping looks like:

```
buf:  [ 6 | 7 | 3 | 4 | 5 ]   (cap=5, pushed 1..7)
              ^
              head=2  (next write goes here)
oldest = (head - count + cap) % cap = (2 - 5 + 5) % 5 = 2
logical index 0 maps to buf[2] = 3  (correct: oldest surviving item)
```

### Functions

#### `New[T any](capacity int) *RingBuf[T]`
Allocates the backing array once. `head` and `count` start at zero. All subsequent `Push` calls write into this pre-allocated slice — no further heap allocations for the buffer itself.

#### `Push(item T)`
Writes `item` at `buf[head]`, then advances `head = (head + 1) % cap`. If `count < cap`, increments `count`. If the buffer is already full, `count` stays at `cap` and the oldest entry is silently overwritten — this is the intended sliding-window behaviour.

#### `Len() int`
Returns `count`. O(1).

#### `Cap() int`
Returns `len(buf)`. O(1).

#### `At(i int) T`
Translates a logical index (0 = oldest, Len-1 = newest) to a physical slot:
```
oldest = (head - count + cap) % cap
physical = (oldest + i) % cap
```
The `+ cap` before the modulo handles the case where `head - count` is negative (i.e. the buffer has not yet wrapped). **Panics** on out-of-range `i` — consistent with Go slice semantics; callers guard with `Len()`.

#### `Slice() []T`
Allocates a fresh `[]T` of length `count`, fills it by calling `At(i)` for each index in order, and returns it. The returned slice is independent — mutating it does not affect the ring buffer's internal state.

---

## internal/parser — Log Format Parsing

**Files:**
- `internal/parser/parser.go` — `ParsedEvent`, `Parser` interface, registry, `Pool`
- `internal/parser/nginx.go` — nginx combined log parser
- `internal/parser/apache.go` — Apache CLF parser
- `internal/parser/json.go` — structured JSON log parser
- `internal/parser/syslog.go` — RFC5424 syslog parser
- `internal/parser/auto.go` — format auto-detection with locking

**Purpose:** Converts raw log lines (`tailer.RawLine`) into normalised `ParsedEvent` values. A goroutine pool fans one input channel across N worker goroutines. Unparseable lines are silently dropped — forwarding a half-parsed event would corrupt counter aggregations.

---

### Data Structures

#### `ParsedEvent` (public — shared with `internal/counter`)
```
ParsedEvent
  Timestamp  time.Time      — when the log event occurred
  Source     string         — file path or "stdin"
  Level      string         — "info" | "warn" | "error" | "fatal" (non-HTTP logs)
  Host       string         — source IP address or hostname
  Method     string         — HTTP verb (empty for non-HTTP)
  Path       string         — URL path (empty for non-HTTP)
  StatusCode int            — HTTP status code; 0 means non-HTTP event
  Bytes      int64          — response size in bytes; 0 if absent
  Latency    time.Duration  — request duration; 0 if absent
  Raw        string         — original unparsed log line
```

**Field conventions:**
- `StatusCode == 0` signals a non-HTTP log line (syslog, JSON structured log). The counter uses this to switch between HTTP error classification (4xx/5xx) and level-based classification (`error`/`fatal`).
- `Latency == 0` means latency was not present in the log line and is excluded from p99 calculations.
- `Level` is always lower-cased before storage so comparisons are simple string equality.

#### `Parser` interface (public)
```go
type Parser interface {
    Name()  string
    Parse(raw tailer.RawLine) (ParsedEvent, bool)
}
```
`Parse` returns `(event, true)` on success. `(_, false)` means the line should be dropped — blank line, comment, or format mismatch. No error return: unparseable lines are a normal condition, not an exception.

#### `Pool` (public)
```
Pool
  format   string  — format name passed to NewParser for each worker
  workers  int     — number of goroutines to spawn (minimum 1)
```

---

### Functions — `parser.go`

#### `NewParser(format string) (Parser, error)`
Looks up `format` in the package-level registry map and calls the registered factory function to return a fresh `Parser` instance. Returns an error for unknown format names. Each call returns a **new** instance — workers in the pool never share a `Parser`.

**Registry (populated at package init):**
| Key        | Factory           |
|------------|-------------------|
| `"nginx"`  | `&nginxParser{}`  |
| `"apache"` | `&apacheParser{}` |
| `"json"`   | `&jsonParser{}`   |
| `"syslog"` | `&syslogParser{}` |
| `"auto"`   | `newAutoParser()` |

#### `NewPool(format string, workers int) *Pool`
Returns a `Pool`. If `workers < 1`, clamped to 1.

#### `(*Pool).Run(ctx, in <-chan tailer.RawLine, out chan<- ParsedEvent)`
Spawns `workers` goroutines. Each worker:
1. Creates its own `Parser` via `NewParser(format)` — no shared mutable state between workers.
2. Loops on a two-way select:
   - `case raw, ok := <-in` — parses the line; sends to `out` if `ok==true`; returns if channel is closed.
   - `case <-ctx.Done()` — returns immediately.
3. `sync.WaitGroup` ensures `Run` blocks until **all** workers have exited.

`Run` does **not** close `out` — the caller (pipeline) owns the output channel lifetime. The caller detects completion by waiting for `Run` to return.

---

### Functions — `nginx.go`

#### `(*nginxParser).Name() string`
Returns `"nginx"`.

#### `(*nginxParser).Parse(raw tailer.RawLine) (ParsedEvent, bool)`
Applies a single pre-compiled `regexp.MustCompile` against the trimmed line content.

**Regex captures (in order):**
1. `remote_addr` → `Host`
2. `time_local` → `Timestamp` (parsed with layout `"02/Jan/2006:15:04:05 -0700"`)
3. `method` → `Method`
4. `path` → `Path`
5. `status` → `StatusCode`
6. `body_bytes_sent` → `Bytes` (`"-"` is treated as 0, for 304 responses)
7. `request_time` (optional, last group) → `Latency` — float seconds converted via `time.Duration(secs * float64(time.Second))`

The trailing `request_time` group is optional (`(?:\s+([\d.]+))?$`) — absent when nginx is configured without `$request_time` in the log format. `Latency` is left as zero in that case.

Returns `(_, false)` for empty lines or lines that do not match the regex.

---

### Functions — `apache.go`

#### `(*apacheParser).Name() string`
Returns `"apache"`.

#### `(*apacheParser).Parse(raw tailer.RawLine) (ParsedEvent, bool)`
Applies a stricter regex than nginx — the Apache CLF regex has **no** trailing referer, user-agent, or request-time groups. This ensures apache lines do not accidentally match the nginx pattern and vice versa.

Captures: `host`, `time` (same layout as nginx), `method`, `path`, `status`, `bytes`.

`Latency` is always zero — CLF has no latency field. `Level` is always empty.

Reuses `nginxTimeLayout` constant from `nginx.go` — both formats share the same `dd/Mon/yyyy:HH:MM:SS tz` timestamp format.

---

### Functions — `json.go`

#### `(*jsonParser).Name() string`
Returns `"json"`.

#### `(*jsonParser).Parse(raw tailer.RawLine) (ParsedEvent, bool)`
Fast-fail: returns `(_, false)` immediately if the trimmed line is empty or does not start with `{`.

Otherwise calls `json.Unmarshal` into a `map[string]any`. If unmarshal fails, returns `(_, false)`.

**Field resolution uses alias lists (first match wins):**

| `ParsedEvent` field | Aliases tried in order                    |
|---------------------|-------------------------------------------|
| `Timestamp`         | `timestamp`, `time`, `ts`, `@timestamp`   |
| `Level`             | `level`, `severity`, `lvl`                |
| `Latency`           | `latency_ms`, `duration_ms`, `elapsed_ms` |

Timestamp parsing:
- String values → `time.Parse(time.RFC3339, val)`
- Float64 values → interpreted as Unix epoch seconds (Zap's `ts` field convention)

Level is lowercased via `strings.ToLower`.

Latency numeric values are assumed **milliseconds** — the most common unit in structured logging libraries (Logrus, Zap, structlog, ECS).

Optional HTTP fields read directly by key: `host`, `method`, `path`, `status_code`.

---

### Functions — `syslog.go`

#### `(*syslogParser).Name() string`
Returns `"syslog"`.

#### `(*syslogParser).Parse(raw tailer.RawLine) (ParsedEvent, bool)`
Fast-fail: returns `(_, false)` if the line is empty or does not start with `<`.

Applies a regex matching the RFC5424 header:
```
<PRI>VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA
```
Captures: `PRI` (integer), `TIMESTAMP` (RFC3339), `HOSTNAME`.

**PRI → Level mapping** via `priToLevel(pri int)`:
```
severity = pri % 8     (RFC5424: PRI = facility*8 + severity)
0,1,2 (emerg/alert/crit) → "fatal"
3 (error)                → "error"
4 (warning)              → "warn"
5,6 (notice/info)        → "info"
7 (debug)                → "debug"
```

`StatusCode`, `Latency`, `Method`, `Path`, `Bytes` are always zero/empty — syslog is a non-HTTP format.

---

### Functions — `auto.go`

#### `newAutoParser() *autoParser`
Returns an `autoParser` with a `probes` list in detection-priority order:
```
nginx → syslog → json → apache
```
**Order rationale:**
- `nginx` before `apache`: the apache regex is a strict subset of the nginx regex (fewer trailing groups); trying nginx first correctly classifies combined-format lines.
- `syslog` before `json`: a syslog message body may itself be valid JSON; the more specific format must win.
- `apache` last: least likely in practice and most likely to be a subset match.

#### `(*autoParser).Name() string`
Returns `"auto"`.

#### `(*autoParser).Parse(raw tailer.RawLine) (ParsedEvent, bool)`
**Fast path (locked):** If `p.locked` is non-nil (format already detected), delegates directly to `p.locked.Parse(raw)`. No mutex contention after warm-up — read under lock, then call outside lock.

**Slow path (probe):** Tries each candidate in `probes` order. On first success:
1. Acquires the mutex.
2. Double-checked lock: sets `p.locked = candidate` only if `p.locked == nil` (prevents two workers racing to lock to different formats).
3. Re-calls `p.locked.Parse(raw)` — ensures the locked winner is used even if another goroutine raced and locked first.

Returns `(_, false)` if no candidate matches.

**Concurrency safety:** `p.locked` is read and written under `sync.Mutex`. The double-checked pattern ensures exactly one format wins the race across all pool workers without requiring a full lock on every call after warm-up.

---

## internal/counter — Bucket Aggregation

**File:** `internal/counter/counter.go`

**Purpose:** Reads `ParsedEvent` values from a channel, groups them into fixed-duration time buckets, and emits one `EventCount` per bucket interval. Sits between the parser pool and the sliding window in the pipeline.

### Data Structures

#### `EventCount` (public — shared with `internal/window`)
```
EventCount
  WindowStart  time.Time          — start of the bucket interval
  WindowEnd    time.Time          — end of the bucket interval (ticker fire time or shutdown time)
  Total        int64              — total events in this bucket
  ByStatus     map[int]int64      — HTTP status code -> event count
  ByHost       map[string]int64   — source IP/host -> event count
  ByLevel      map[string]int64   — log level -> event count
  ErrorRate    float64            — errors / Total for this bucket (0 if Total==0)
  P99Latency   time.Duration      — 99th-percentile latency of events in this bucket
```

#### `Counter` (public)
```
Counter
  bucketDuration  time.Duration   — how long each bucket spans (typically 1s)
```

#### `bucket` (internal — not exported)
```
bucket
  start      time.Time
  total      int64
  errors     int64               — count of error-classified events
  byStatus   map[int]int64
  byHost     map[string]int64
  byLevel    map[string]int64
  latencies  []time.Duration     — raw latency values collected for p99 calculation
```

### Functions

#### `New(bucketDuration time.Duration) *Counter`
Returns a `Counter` configured with the given bucket duration. No goroutines are started here — `Run` starts the processing loop.

#### `newBucket(start time.Time) *bucket` (internal)
Allocates a fresh bucket with empty maps. Called once at `Run` startup and again after each flush. The maps are freshly allocated so each `EventCount` returned from `flush` owns its own map data and is safe to read after the bucket is replaced.

#### `(*bucket).add(e parser.ParsedEvent)` (internal)
Classifies and accumulates a single event into the current bucket:

1. Increments `total`.
2. If `e.Host != ""`, increments `byHost[e.Host]`.
3. **HTTP path** (`StatusCode != 0`): increments `byStatus[e.StatusCode]`; if `>= 400`, increments `errors`.
4. **Log-level path** (`StatusCode == 0` and `Level != ""`): increments `byLevel[e.Level]`; if level is `"error"` or `"fatal"`, increments `errors`.
5. If `e.Latency > 0`, appends to `latencies`.

#### `(*bucket).flush(end time.Time) EventCount` (internal)
Computes derived fields and returns an `EventCount`:
- `ErrorRate = errors / total` (0 if `total == 0`)
- `P99Latency` = result of `p99(latencies)`
- Maps are transferred directly (not copied) — the bucket is discarded after flush so there is no aliasing risk.
- `WindowStart` = `bucket.start`, `WindowEnd` = the `end` argument (ticker timestamp or shutdown time).

#### `p99(latencies []time.Duration) time.Duration` (internal)
Computes the 99th percentile using the nearest-rank method:
1. Copies the slice to avoid mutating the bucket's data.
2. Sorts ascending.
3. `rank = ceil(0.99 × n)`, returns `sorted[rank-1]`.
Returns 0 for an empty slice.

#### `(*Counter).Run(ctx, in <-chan ParsedEvent, out chan<- EventCount)`
The main blocking loop. Starts a `time.Ticker` for `bucketDuration`. Runs a three-way `select`:

| Branch                 | Action |
|------------------------|--------|
| `case e, ok := <-in`   | If `ok`, calls `cur.add(e)`. If channel is closed (`!ok`), sets `in = nil` to prevent the select from spinning on zero-value reads from a closed channel. |
| `case t := <-ticker.C` | Calls `cur.flush(t)`, sends result to `out`, starts a fresh `newBucket(t)`. Fires even if no events arrived — emits a zero-total bucket which the analyzer needs for silence detection. |
| `case <-ctx.Done()`    | Performs a non-blocking drain of any events still buffered in `in`, then calls `cur.flush(time.Now())` and sends the final partial bucket to `out` before returning. Guarantees no events are lost on SIGTERM. |

**Ownership note:** `Run` never closes `out` — the caller (pipeline) owns the output channel lifetime.

---

## internal/window — Sliding Window

**File:** `internal/window/window.go`

**Purpose:** A thread-safe wrapper around `RingBuf[counter.EventCount]`. Provides a concurrent-safe store of the most recent N buckets, used by the analyzer to compute baselines.

### Data Structure

```
Window
  mu   sync.RWMutex                        — guards all access to buf
  buf  *ringbuf.RingBuf[counter.EventCount] — the underlying circular buffer
```

### Functions

#### `New(capacity int) *Window`
Allocates the underlying `RingBuf` with the given capacity. For a 60-second window with 1-second buckets, capacity is 60.

#### `(*Window).Push(e counter.EventCount)`
Acquires a **write lock**, calls `buf.Push(e)`, releases the lock. When the buffer is full the oldest bucket is silently evicted — identical to `RingBuf.Push` semantics.

#### `(*Window).Len() int`
Acquires a **read lock**, reads `buf.Len()`, releases the lock. Multiple goroutines may call `Len` concurrently without blocking each other.

#### `(*Window).Snapshot() []counter.EventCount`
Acquires a **read lock**, calls `buf.Slice()` (which returns an independent copy), releases the lock. The returned slice is safe to read, sort, or mutate after the lock is released — it does not share memory with the internal buffer. The analyzer calls this once per evaluation tick.

**Locking rationale:** Write lock on `Push` only (one writer: the counter goroutine). Read lock on `Snapshot` and `Len` (multiple potential readers: analyzer, metrics). This maximises read concurrency without blocking reads for reads.

---

## internal/window/stats — Statistical Functions

**File:** `internal/window/stats.go`

**Purpose:** Pure functions that compute statistics over a `[]counter.EventCount` snapshot. No state, no locks, no receivers. Called by the analyzer after it takes a `Window.Snapshot()`.

### Functions

#### `Mean(buckets []counter.EventCount) float64`
Arithmetic mean of `Total` across all buckets.
```
sum(b.Total for b in buckets) / len(buckets)
```
Returns 0 for an empty slice. Used as the baseline rate for spike detection.

#### `StdDev(buckets []counter.EventCount) float64`
**Population** standard deviation of `Total`:
```
sqrt( sum((b.Total - mean)^2) / n )
```
Divides by N (not N-1) because the window represents the complete observed population, not a statistical sample. Returns 0 for an empty or single-element slice. Used in `sigma` detection mode (`mean + k × stddev`).

#### `ErrorRate(buckets []counter.EventCount) float64`
Request-volume-weighted error rate across all buckets:
```
sum(b.Total * b.ErrorRate) / sum(b.Total)
```
Buckets where `Total == 0` are excluded from both numerator and denominator. This prevents zero-traffic buckets (silence periods) from diluting the error signal and avoids division-by-zero. Returns 0 for an empty slice or when all totals are zero.

#### `P99Latency(buckets []counter.EventCount) time.Duration`
99th percentile of `P99Latency` across all bucket snapshots, using the nearest-rank method:
```
sort buckets by P99Latency ascending
rank = ceil(0.99 × n)
return sorted[rank-1]
```
Takes the p99 of p99s — each bucket already represents the 99th percentile of raw latencies within its 1-second window. Returns 0 for an empty slice. Used in the `latency_spike` detection rule.

---

## internal/analyzer — Anomaly Detection

**File:** `internal/analyzer/analyzer.go`

**Purpose:** Receives `counter.EventCount` buckets from the counter, pushes them into the sliding window, evaluates five detection rules against the historical baseline, enforces per-kind cooldown, and emits `Anomaly` values. This is the intelligence layer — everything upstream produces data; the analyzer decides what is anomalous.

---

### Data Structures

#### `AnomalyKind` (public)
```
KindRateSpike    = "rate_spike"
KindErrorSurge   = "error_surge"
KindLatencySpike = "latency_spike"
KindHostFlood    = "host_flood"
KindSilence      = "silence"
```

#### `SeverityLevel` (public)
```
SeverityWarning  = "warning"
SeverityCritical = "critical"
```

#### `Anomaly` (public — passed to alerters)
```
Anomaly
  DetectedAt    time.Time     — wall time the anomaly was detected
  Kind          AnomalyKind   — which rule fired
  Severity      SeverityLevel — warning | critical
  Message       string        — human-readable description
  CurrentValue  float64       — observed metric (rate, error fraction, latency ms, host fraction)
  BaselineValue float64       — window mean used as the comparison baseline
  ThresholdUsed float64       — the computed threshold that was crossed
  SpikeRatio    float64       — CurrentValue / BaselineValue (0 if baseline is 0)
  OffendingHost string        — populated only for host_flood; empty otherwise
```

#### `Analyzer` (public)
```
Analyzer
  cfg      config.Config              — detection thresholds and settings
  window   *window.Window             — sliding window of EventCount buckets
  cooldown map[AnomalyKind]time.Time  — last-fired timestamp per kind
```

---

### Functions

#### `New(cfg config.Config, win *window.Window) *Analyzer`
Returns a configured `Analyzer`. Initialises the `cooldown` map (empty). No goroutines started here — `Run` contains the blocking loop.

**Why value-type `config.Config` (not pointer):** The analyzer captures the config at construction time; the caller may change the original struct without affecting detection thresholds mid-run.

#### `(*Analyzer).Run(ctx, in <-chan counter.EventCount, out chan<- Anomaly)`
The main blocking loop. Two-way select:

| Branch | Action |
|--------|--------|
| `case <-ctx.Done()` | Return immediately |
| `case ec, ok := <-in` | If closed (`!ok`), return. Otherwise: call `evaluate(ec, out)` **first**, then `window.Push(ec)` |

**Critical ordering — evaluate before push:** The current bucket is evaluated against the historical window *before* being added to it. If the spike bucket were pushed first, it would inflate the window mean and reduce its own spike ratio, potentially suppressing the alert. The historical baseline must be uncontaminated by the bucket being judged.

`Run` does **not** close `out` — the caller (pipeline) owns the output channel.

#### `(*Analyzer).evaluate(current counter.EventCount, out chan<- Anomaly)` (internal)
Takes a window snapshot and checks the baseline guard first:

```
snap = window.Snapshot()
if len(snap) < MinBaselineSamples → return  (window not warm yet)
```

Computes the rate threshold based on `DetectionMethod`:
- **ratio:** `threshold = mean × SpikeMultiplier`
- **sigma:** `threshold = mean + SpikeMultiplier × stddev`

Then calls all five check functions in sequence. Each check is independent — multiple rules can fire on the same bucket.

---

### Detection Rules

#### `checkRate` — `rate_spike`
```
cur = float64(current.Total)
if cur > threshold → emit rate_spike
```
`SpikeRatio = cur / mean` (0 if mean is 0).

**Severity:** Warning if `SpikeRatio < 10`, Critical if `≥ 10`.

#### `checkError` — `error_surge`
```
if current.ErrorRate > ErrorRateThreshold → emit error_surge
```
Strict greater-than — equal to threshold does not fire. `CurrentValue = current.ErrorRate`.

**Severity:** always Warning.

#### `checkLatency` — `latency_spike`
```
baselineP99 = window.P99Latency(snap)
threshold   = baselineP99 × LatencyMultiplier
if current.P99Latency > threshold → emit latency_spike
```
Two early exits: if `current.P99Latency == 0` (no latency data in bucket) or `baselineP99 == 0` (no latency in window history). These guards prevent false positives when a format switch or log silence removes latency data from the stream.

`CurrentValue`, `BaselineValue`, `ThresholdUsed` are all reported in **milliseconds** for consistent alerter formatting.

**Severity:** Warning if `SpikeRatio < 5`, Critical if `≥ 5`.

#### `checkHostFlood` — `host_flood`
```
floodThreshold = float64(current.Total) × HostFloodFraction
topHost, topCount = argmax(current.ByHost)
if float64(topCount) > floodThreshold → emit host_flood
```
Linear scan over `current.ByHost` — typically O(10–100) entries per bucket, negligible cost. `OffendingHost` is set to the name of the top host. Early exit if `Total == 0` or `ByHost` is empty.

**Severity:** always Critical.

#### `checkSilence` — `silence`
```
consecutive = 0
for i = len(snap)-1 downto 0:
    if snap[i].Total == 0: consecutive++
    else: break
if consecutive ≥ SilenceThreshold → emit silence
```
Tail scan from newest to oldest — stops at the first non-zero bucket. This correctly measures "the last N seconds were silent" without being distorted by older active traffic. A burst an hour ago must not mask a current outage.

`CurrentValue = float64(consecutive)`.

**Severity:** always Critical.

---

### Cooldown — `emit` (internal)

```go
func (a *Analyzer) emit(out chan<- Anomaly, anomaly Anomaly) {
    last := a.cooldown[anomaly.Kind]
    if AlertCooldown > 0 && !last.IsZero() && time.Since(last) < AlertCooldown → return
    anomaly.DetectedAt = time.Now()
    a.cooldown[anomaly.Kind] = anomaly.DetectedAt
    out <- anomaly
}
```

**Zero-value map semantics:** `a.cooldown[kind]` returns `time.Time{}` (zero) for a kind that has never fired. `last.IsZero()` is true → the `time.Since` check is skipped → first firing always goes through. No special initialisation of the map is needed.

**Per-kind independence:** Each `AnomalyKind` has its own timestamp. A `rate_spike` cooldown does not block `error_surge` from firing on the same bucket. Multiple kinds can fire simultaneously.

**`AlertCooldown == 0` bypass:** If cooldown is set to zero (disabled), the `AlertCooldown > 0` guard short-circuits the entire cooldown check. Used in tests to ensure every qualifying bucket fires an alert without waiting.

---

### Severity Summary

| Kind | Severity |
|------|---------|
| `rate_spike` | Warning if ratio < 10×; Critical if ≥ 10× |
| `error_surge` | always Warning |
| `latency_spike` | Warning if ratio < 5×; Critical if ≥ 5× |
| `host_flood` | always Critical |
| `silence` | always Critical |

---

## internal/config — Configuration Loading

**File:** `internal/config/config.go`

**Purpose:** Defines the full runtime configuration for log_analyser, provides three load paths (file, env, merged), validates all fields, and enforces the security rule that secrets must never appear in config files.

### Data Structures

```
Config
  LogFile             string         — path to the log file to tail
  Format              string         — "auto"|"nginx"|"apache"|"json"|"syslog"
  Follow              bool           — keep tailing past EOF
  PollInterval        time.Duration  — file poll interval (default 100ms)
  WindowSize          time.Duration  — sliding window duration (default 60s)
  BucketDuration      time.Duration  — aggregation bucket size (default 1s)
  SpikeMultiplier     float64        — rate > mean × N triggers rate_spike (default 3.0)
  ErrorRateThreshold  float64        — error fraction threshold (default 0.05)
  HostFloodFraction   float64        — single-IP fraction threshold (default 0.5)
  LatencyMultiplier   float64        — p99 > baseline × N triggers latency_spike (default 3.0)
  SilenceThreshold    int            — silent seconds before silence alert (default 30)
  AlertCooldown       time.Duration  — per-kind duplicate suppression (default 30s)
  MinBaselineSamples  int            — buckets needed before alerting starts (default 10)
  DetectionMethod     string         — "ratio" | "sigma"
  ParserWorkers       int            — goroutine pool size for parser
  MetricsAddr         string         — Prometheus /metrics listen address
  Alerters            AlertersConfig — sub-config for all alerters

AlertersConfig
  Console  ConsoleConfig
  Webhook  WebhookConfig
  File     FileConfig

ConsoleConfig   { Enabled bool; UseColor bool; TimeFormat string }
WebhookConfig   { Enabled bool; URL string; Secret string; Timeout time.Duration; MaxRetries int; RetryDelay time.Duration }
FileConfig      { Enabled bool; Path string; Format string; MaxSizeMB int }
```

#### `SecretInFileError`
```
SecretInFileError
  Field  string  — the config key path that contained the secret (e.g. "alerters.webhook.secret")
```
Returned when a secret field is found populated inside a config file. `Error()` returns a message that names the correct `LOG_ANALYSER_*` environment variable to use instead. `IsSecretInFileError(err)` uses `errors.As` for unwrap-safe detection.

### Functions

#### `Default() *Config`
Returns a `*Config` with all production-safe defaults hardcoded as Go literals. Does not use Viper or read any files/env vars. Used in tests that need a valid base config without I/O, and as the canonical reference for what defaults are.

#### `newViper() *viper.Viper` (internal)
Creates a new Viper instance and calls `v.SetDefault(key, value)` for every config field. These defaults are the fallback layer — they apply when neither a file value nor an env var is set. Kept in sync with `Default()`.

#### `setupEnv(v *viper.Viper)` (internal)
Configures environment variable resolution on a Viper instance:
- `SetEnvPrefix("LOG_ANALYSER")` — all env vars must start with `LOG_ANALYSER_`
- `SetEnvKeyReplacer(".", "_")` — dots in key names become underscores in env var names
- `AutomaticEnv()` — handles top-level keys automatically (e.g. `LOG_ANALYSER_FORMAT` → `format`)
- Explicit `BindEnv` calls for nested alerter keys (e.g. `alerters.webhook.secret` → `LOG_ANALYSER_WEBHOOK_SECRET`) because `AutomaticEnv` cannot bridge the `alerters.*` path prefix automatically.

#### `extract(v *viper.Viper) *Config` (internal)
Maps every Viper key to its corresponding `Config` field using typed getter methods (`GetString`, `GetBool`, `GetDuration`, `GetFloat64`, `GetInt`). Explicit mapping (instead of `mapstructure` auto-decode) keeps all key names visible in one place and avoids silent field mismatches.

#### `checkForFileSecrets(v *viper.Viper) error` (internal)
Called **before** `setupEnv` in the `Load` path. At that point only file values are loaded into Viper — env vars are not yet bound. Any non-empty value for `alerters.webhook.secret` must therefore have come from the file. Returns `*SecretInFileError` if found. Called in both `LoadFile` and `Load`.

#### `LoadFile(path string) (*Config, error)`
Loads configuration from a YAML file only — no env var merging:
1. `newViper()` — sets built-in defaults
2. `v.ReadInConfig()` — reads the YAML file
3. `checkForFileSecrets(v)` — rejects hardcoded secrets
4. `extract(v)` — maps to `*Config`

#### `LoadEnv() (*Config, error)`
Loads configuration from environment variables only, falling back to built-in defaults:
1. `newViper()` — sets built-in defaults
2. `setupEnv(v)` — binds env vars
3. `extract(v)` — maps to `*Config`

Does not read any files. Used in deployments where configuration is injected entirely via environment.

#### `Load(path string) (*Config, error)`
Merges file and environment variables with correct precedence (env > file > default):
1. `newViper()` — sets built-in defaults
2. `v.ReadInConfig()` — loads the file (file values now override defaults in Viper's layers)
3. `checkForFileSecrets(v)` — runs **before** env binding so any secret value is provably from the file
4. `setupEnv(v)` — binds env vars (env values now override file values in Viper's layers)
5. `extract(v)` — maps to `*Config`

#### `(*Config).Validate() error`
Validates all fields independently of how the config was loaded. Called explicitly after loading — not inside the loaders — so callers can apply flag overrides between load and validate. Rules checked:

| Rule               | Condition                                                   |
|--------------------|-------------------------------------------------------------|
| Format             | Must be one of: `auto`, `nginx`, `apache`, `json`, `syslog` |
| DetectionMethod    | Must be `ratio` or `sigma`                                  |
| WindowSize         | Must be >= BucketDuration                                   |
| SpikeMultiplier    | Must be > 0                                                 |
| ErrorRateThreshold | Must be in (0, 1]                                           |
| HostFloodFraction  | Must be in (0, 1]                                           |
| LatencyMultiplier  | Must be > 0                                                 |
| MinBaselineSamples | Must be > 0                                                 |
| AlertCooldown      | Must be >= 0                                                |
| Webhook enabled    | URL must not be empty                                       |
| File enabled       | Path must not be empty                                      |

Returns the first error encountered (early-return style). Returns `nil` if all rules pass.

---

## internal/tailer — File Tailing and Rotation

**Files:**
- `internal/tailer/tailer.go` — core logic (all platforms)
- `internal/tailer/open_windows.go` — Windows-specific file open with `FILE_SHARE_DELETE`
- `internal/tailer/open_other.go` — non-Windows stub wrapping `os.Open`

**Purpose:** Reads a log file (or stdin) in real-time and emits one `RawLine` per log line. Handles log rotation (rename-based and truncation-based), CRLF line endings, and lines larger than `bufio.Scanner`'s default 64KB limit. This is the pipeline entry point — every `ParsedEvent` originates from a `RawLine` emitted here.

### Data Structure

```
RawLine
  Content  string     — log line text with trailing '\r' stripped
  Source   string     — absolute file path, or "stdin"
  ReadAt   time.Time  — wall clock time the line was read from disk
  LineNum  int64      — 1-based counter; resets to 1 on each file open or rotation
```

### Build-Tag Split: `open_windows.go` / `open_other.go`

The `openFile(path string) (*os.File, error)` function is the only platform-specific code in the tailer.

**`open_windows.go`** (`//go:build windows`):
Uses `syscall.CreateFile` with sharing flags:
```
FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE
```
The critical flag is `FILE_SHARE_DELETE`. On Windows, `os.Open` does not set this flag, which means `os.Rename` on an open file returns "The process cannot access the file because it is being used by another process." Adding `FILE_SHARE_DELETE` allows logrotate-style rename rotation while the tailer holds the file open.

**`open_other.go`** (`//go:build !windows`):
Simply wraps `os.Open`. On Linux and macOS, file renaming is permitted regardless of open handles — no special flags needed.

### Functions

#### `New(path string, follow bool, pollInterval time.Duration) *Tailer`
Returns a configured `Tailer`. No I/O happens here. `path == ""` means stdin. `follow=true` seeks to EOF on startup (mimics `tail -f`); `follow=false` reads from byte 0 and returns at EOF.

#### `(*Tailer).Run(ctx context.Context, out chan<- RawLine)`
The public entry point. Dispatches to `runStdin` or `runFile` based on whether `path` is empty. Blocks until `ctx` is cancelled. Never closes `out` — the caller (pipeline) owns the channel.

#### `(*Tailer).runStdin(ctx, out)` (internal)
Creates one `Scanner` over `os.Stdin` and reads until EOF or ctx cancellation. No seek, no rotation check, no poll loop — stdin is a one-shot stream. Emits each line with `Source = "stdin"`.

#### `(*Tailer).runFile(ctx, out)` (internal)
The main file-follow loop. On entry:
1. Opens the file with `openFile(path)` (platform-specific, see above).
2. Seeks to EOF if `follow=true`, otherwise stays at byte 0.
3. Captures the initial `os.FileInfo` as the rotation baseline.

Then runs the poll loop:

```
loop:
  seek to current offset
  create fresh Scanner
  drain all available lines -> emit to out, advance offset
  if follow=false: return

  stat the path again
  if path missing          -> sleep, retry
  if !os.SameFile(old,new) -> rotation detected: reopen, reset offset+lineNum
  if size < offset OR
     (size == offset AND mtime changed) -> truncation: seek to 0, reset offset+lineNum
  else                     -> no new data: sleep(pollInterval)
```

**Why a fresh Scanner each poll cycle:** `bufio.Scanner` permanently sets an internal `done` flag on first EOF and never calls `Read` again. Re-using one scanner across polls would permanently stop reading after the first quiet period. Creating a new scanner after `f.Seek(offset, 0)` is the correct approach — it costs one small allocation per poll tick.

**Rotation detection (`os.SameFile`):**
`os.SameFile(oldStat, newStat)` compares the underlying OS file identity. On Windows this uses `VolumeSerialNumber + FileIndex` from `GetFileInformationByHandle` (standard library, no CGO). If the path now points to a different file (renamed rotation), `SameFile` returns false. The tailer closes the old handle, reopens the path, resets `offset=0` and `lineNum=0`, and continues the loop without sleeping — to avoid missing lines written to the new file in the brief gap.

**Truncation detection (`size < offset` OR mtime change):**
Two conditions trigger a truncation reset:
- `newStat.Size() < offset` — the file shrank (classic `copytruncate` style: file is cleared in-place after the logger is told to reopen).
- `newStat.Size() == offset && newStat.ModTime().After(stat.ModTime())` — file was overwritten with content of identical length. Size alone cannot detect this; the mtime guard catches same-size in-place rewrites.

On truncation: `f.Seek(0, 0)` resets the file pointer, `offset` and `lineNum` are zeroed, `stat` is updated to the new `FileInfo`.

#### `(*Tailer).sleep(ctx context.Context) bool` (internal)
Blocks for `pollInterval` using `time.After`. Returns `false` if `ctx` is cancelled during the wait, `true` otherwise. Used as the sole sleep point in `runFile` — ensures context cancellation is never delayed by more than one poll interval.

#### `newScanner(f) *bufio.Scanner` (internal)
Creates a `bufio.Scanner` with:
- A **1 MB token buffer** (`scanner.Buffer(make([]byte, 1MB), 1MB)`) — overrides the 64KB default. Allows single log lines up to 1 MB without error.
- The custom `splitLines` split function.

#### `splitLines(data []byte, atEOF bool) (advance, token, err)` (internal)
A `bufio.SplitFunc` that splits on `'\n'` and returns the token **without** the newline character. A trailing `'\r'` is intentionally left in the token — this keeps the split function simple and lets `stripCR` remove it. The `advance` value is always `i+1` (past the `'\n'`), which means the offset arithmetic in `runFile` (`offset += int64(len(scanner.Bytes())) + 1`) is correct for both LF and CRLF files:

```
LF file:   raw bytes = "line\n"      token = "line"   (4 bytes)  advance = 5 = len(token)+1 ✓
CRLF file: raw bytes = "line\r\n"   token = "line\r"  (5 bytes)  advance = 6 = len(token)+1 ✓
```

#### `stripCR(s string) string` (internal)
Removes a trailing `'\r'` from a line if present. Called on `scanner.Text()` before setting `RawLine.Content`. This normalises CRLF Windows log files so the parser layer always receives bare text without carriage returns.
