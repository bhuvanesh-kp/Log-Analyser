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
9. [internal/alerter — Alert Delivery](#internalalerter--alert-delivery)
10. [internal/pipeline — Goroutine Wiring and Lifecycle](#internalpipeline--goroutine-wiring-and-lifecycle)
11. [internal/metrics — Prometheus Metrics Exposition](#internalmetrics--prometheus-metrics-exposition)

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

#### `Anomaly` (public — passed to alerters, serialised to JSON)
```go
type Anomaly struct {
    DetectedAt    time.Time     `json:"detected_at"`
    Kind          AnomalyKind   `json:"kind"`
    Severity      SeverityLevel `json:"severity"`
    Message       string        `json:"message"`
    CurrentValue  float64       `json:"current_value"`
    BaselineValue float64       `json:"baseline_value"`
    ThresholdUsed float64       `json:"threshold_used"`
    SpikeRatio    float64       `json:"spike_ratio"`
    OffendingHost string        `json:"offending_host,omitempty"`
}
```
`OffendingHost` uses `omitempty` — the field is absent from JSON output for all kinds except `host_flood`. `time.Time` marshals as RFC3339 string natively. `AnomalyKind` and `SeverityLevel` are `string` type aliases and marshal as plain strings.

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

---

## internal/alerter — Alert Delivery

**Files:**
- `internal/alerter/alerter.go` — `Alerter` interface
- `internal/alerter/console.go` — colored terminal output
- `internal/alerter/webhook.go` — HTTP POST with HMAC-SHA256 signing and retry
- `internal/alerter/file.go` — append JSONL to file
- `internal/alerter/multi.go` — concurrent fan-out to all alerters

**Purpose:** Delivers `analyzer.Anomaly` values to one or more destinations. The interface is deliberately minimal — implementation complexity lives in the concrete types, not the contract. `MultiAlerter` is the only component the pipeline interacts with directly; the others are registered into it.

---

### Interface

#### `Alerter` (public)
```go
type Alerter interface {
    Name() string
    Send(ctx context.Context, a analyzer.Anomaly) error
}
```

`Name()` returns a short identifier used in logs and metrics (e.g. `"console"`, `"webhook"`, `"file"`, `"multi"`).

`Send` delivers the anomaly to the destination. `ctx` cancellation must abort in-flight work (HTTP requests, retry sleeps). Returns `nil` on success, a non-nil error on permanent or exhausted-retry failure.

**`io.Closer` is opt-in, not part of the interface.** Only `FileAlerter` holds a persistent resource (an `*os.File`). `ConsoleAlerter` and `WebhookAlerter` have no resources to release. The pipeline type-asserts each alerter to `io.Closer` at shutdown and calls `Close()` only if it is implemented. Forcing `Close()` into the interface would add meaningless boilerplate to implementations that have nothing to close.

---

### `ConsoleAlerter` — `console.go`

#### `NewConsoleAlerter(w io.Writer, color bool) *ConsoleAlerter`
Returns a `ConsoleAlerter` that writes to `w` with ANSI color codes enabled or disabled by `color`. Using an injected `io.Writer` (instead of writing directly to `os.Stderr`) makes the alerter testable with a `bytes.Buffer` without subprocess tricks.

#### `NewConsoleAlerterAuto(w io.Writer) *ConsoleAlerter`
Auto-detects color support:
1. `NO_COLOR` env var set → `color=false` (respects the `NO_COLOR` convention)
2. `w` is `*os.File` and `Stat().Mode() & os.ModeCharDevice != 0` → `color=true` (actual TTY)
3. Otherwise → `color=false` (piped to file or another process)

#### `(*ConsoleAlerter).Name() string`
Returns `"console"`.

#### `(*ConsoleAlerter).Send(_ context.Context, a analyzer.Anomaly) error`
Formats and writes one line to `w`. The context parameter is accepted but not used — console writes are synchronous and instant; there is nothing to cancel.

**Output format (no color):**
```
[WARNING]  15:04:05 rate_spike     3000 req/sec (30.0× baseline of 100 req/sec)
[CRITICAL] 15:04:05 silence        no log events for 30 consecutive seconds
```

**With color:**
- Warning → `\033[33m` (yellow) before the line, `\033[0m` (reset) after
- Critical → `\033[31m` (red) before the line, `\033[0m` (reset) after

Timestamp formatted as `HH:MM:SS` via `a.DetectedAt.Format("15:04:05")`.

---

### `WebhookAlerter` — `webhook.go`

#### `WebhookOption` (functional option type)
```go
type WebhookOption func(*WebhookAlerter)
```

#### `WithRetryDelays(delays []time.Duration) WebhookOption`
Overrides the inter-attempt backoff durations. `delays[i]` is the sleep before the `(i+1)`-th attempt. Setting all delays to 0 is the test pattern — retries happen immediately without `time.Sleep`.

#### `NewWebhookAlerter(url, secret string, client *http.Client, maxRetries int, opts ...WebhookOption) (*WebhookAlerter, error)`
Returns a `WebhookAlerter`. Validates that `url` is non-empty — returns an error immediately otherwise.

- `client == nil` → a default `http.Client` with a 5-second timeout is created internally.
- `maxRetries` is the **total** number of attempts (including the first).
- Default `retryDelays = [1s, 2s, 4s]` (exponential backoff up to 3 retries).
- `opts` are applied after defaults, allowing tests to inject zero-duration delays.

#### `(*WebhookAlerter).Name() string`
Returns `"webhook"`.

#### `(*WebhookAlerter).Send(ctx context.Context, a analyzer.Anomaly) error`
Marshals the payload, then retries up to `maxRetries` times:

**Payload structure:**
```go
struct {
    AlertID string `json:"alert_id"`
    analyzer.Anomaly              // embedded — all Anomaly fields appear at the top level
}
```
`alert_id` is a fresh `uuid.New().String()` per `Send` call. The UUID is identical across retries of the same `Send` invocation, enabling idempotent processing at the receiver.

**Retry logic:**
```
attempt 0: send immediately
attempt i (i > 0):
    sleep retryDelays[min(i-1, len(retryDelays)-1)]
    if ctx.Done() → return ctx.Err()
    send
    if err == nil → return nil
    if ctx error → return immediately
    if 4xx status → return immediately (no retry)
    if 5xx or network error → continue loop
if all attempts exhausted → return last error
```

Zero-delay sleeps still honour ctx cancellation via a non-blocking `select { case <-ctx.Done(): ... default: }`.

#### `(*WebhookAlerter).do(ctx, alertID, body)` (internal)
Single HTTP POST attempt:
1. `http.NewRequestWithContext` — binds the context so the client aborts on cancellation.
2. Sets `Content-Type: application/json` and `X-Alert-ID: <uuid>`.
3. If `secret != ""` → computes `HMAC-SHA256(secret, body)` and sets `X-Signature-SHA256: sha256=<hex>`. The signature covers the raw JSON bytes — receivers can verify by hashing the raw request body.
4. `client.Do(req)` — performs the HTTP call.
5. `resp.Body.Close()` — always closed; body content is not read.
6. Status `>= 400` → returns `*httpStatusError{code}`.

#### `httpStatusError` (internal)
```go
type httpStatusError struct{ code int }
func (e *httpStatusError) Error() string
```
Wraps a non-2xx/3xx status code. Used by the retry loop to distinguish 4xx (abort) from 5xx (retry) via `errors.As`.

---

### `FileAlerter` — `file.go`

#### `NewFileAlerter(path string) (*FileAlerter, error)`
Opens (or creates) the file at `path` with `O_APPEND | O_CREATE | O_WRONLY` flags and `0o644` permissions.

- `O_APPEND` — all writes go to the end of the file; safe from multiple-writer corruption on POSIX.
- `O_CREATE` — creates the file if it does not yet exist.
- Returns an error if the **parent directory** does not exist — the file system error propagates directly.

Creates a `json.Encoder` bound to the file handle. `SetEscapeHTML(false)` prevents `<`, `>`, `&` characters in log messages from being unicode-escaped (e.g. `\u003c`) in the output.

#### `(*FileAlerter).Name() string`
Returns `"file"`.

#### `(*FileAlerter).Send(_ context.Context, a analyzer.Anomaly) error`
Acquires `sync.Mutex`, calls `enc.Encode(a)`, releases the mutex. `json.Encoder.Encode` writes one JSON object followed by a newline (`\n`) — exactly the JSONL format (one JSON document per line).

**Thread safety:** `MultiAlerter` launches each alerter's `Send` in its own goroutine. The mutex ensures concurrent calls cannot interleave partial JSON lines in the output file.

The context parameter is accepted but not used — file writes are synchronous and instant.

#### `(*FileAlerter).Close() error`
Acquires the mutex, closes the `*os.File`, releases the mutex. Called by the pipeline during shutdown via `io.Closer` type assertion. After `Close()`, further `Send` calls will return a file-closed error.

---

### `MultiAlerter` — `multi.go`

#### `NewMultiAlerter(alerters ...Alerter) *MultiAlerter`
Returns a `MultiAlerter` holding a slice of all provided alerters. The variadic signature allows constructing with zero alerters (no-op fanout), one alerter (pass-through), or many.

#### `(*MultiAlerter).Name() string`
Returns `"multi"`.

#### `(*MultiAlerter).Send(ctx context.Context, a analyzer.Anomaly) error`
Concurrent fan-out:
```
if len(alerters) == 0 → return nil

errs := make([]error, len(alerters))
for each alerter[i]:
    go func() { errs[i] = alerter[i].Send(ctx, a) }()
wg.Wait()
return errors.Join(errs...)
```

**One goroutine per alerter** — a slow webhook retry (up to 7 s with default backoff) does not delay the console or file alerters. All three complete independently.

**Error aggregation:** `errors.Join(errs...)` returns `nil` if all elements are `nil`, otherwise returns a combined error containing all non-nil errors separated by newlines. This means:
- One alerter failing does not mask another's success.
- The caller (pipeline) sees a non-nil error if *any* alerter failed.
- The combined error message contains all individual failure messages — useful for log output.

**Context propagation:** The same `ctx` is passed to every alerter's `Send`. If the pipeline cancels the context during a SIGTERM drain, all in-flight webhook retries abort simultaneously.

---

### JSON output example (`FileAlerter` / `WebhookAlerter`)

```json
{
  "detected_at": "2026-04-01T12:00:01Z",
  "kind": "rate_spike",
  "severity": "critical",
  "message": "3000 req/sec (30.0× baseline of 100 req/sec)",
  "current_value": 3000,
  "baseline_value": 100,
  "threshold_used": 300,
  "spike_ratio": 30,
  "alert_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

`offending_host` is absent when not a `host_flood` anomaly (`omitempty`). `alert_id` is present only in the webhook payload (embedded struct); the file alerter writes the `Anomaly` struct directly without a wrapper.

---

## internal/pipeline — Goroutine Wiring and Lifecycle

**File:** `internal/pipeline/pipeline.go`

**Purpose:** Owns all inter-stage channels, constructs every component from `config.Config`, starts all goroutines via a single `Run(ctx)` method, and implements ordered shutdown that guarantees no detected anomalies are dropped. This is the only file that imports all stage packages — no other component needs to know the full topology.

---

### Data Structure

#### `Pipeline` (public)
```go
type Pipeline struct {
    cfg      config.Config
    alerter  alerter.Alerter    // typically a MultiAlerter
    recorder metrics.Recorder   // NoopRecorder when metrics disabled
}
```

The pipeline does not store channels or stage references as struct fields. All channels and stages are created inside `Run` — they are local variables scoped to the lifetime of a single `Run` invocation. This prevents accidental reuse of closed channels if `Run` were called twice (it shouldn't be, but the design is defensive).

---

### Functions

#### `New(cfg config.Config, al alerter.Alerter, rec metrics.Recorder) *Pipeline`
Pure construction — stores config, alerter, and recorder. No goroutines, no channels, no I/O. If `rec` is nil, substitutes `metrics.NoopRecorder{}` — callers never need nil checks. The alerter is injected by the caller (`main.go`) after constructing the appropriate `MultiAlerter` from enabled alerter configs.

#### `(*Pipeline).Run(ctx context.Context) error`
The single blocking entry point. Returns `nil` on clean shutdown, or an error if the log file cannot be opened.

**Execution flow:**

1. **Preflight check:** If `cfg.LogFile != ""` (not stdin), calls `os.Open(cfg.LogFile)` and returns immediately with a wrapped error if the file does not exist or is unreadable. The file handle is closed immediately — the tailer will re-open it when it starts.

2. **Channel allocation (instrumented + real):**
   ```
   instrRawC  chan tailer.RawLine      buffer 1024   (tailer writes here)
   rawC       chan tailer.RawLine      buffer 1024   (parser reads here)
   instrEvtC  chan parser.ParsedEvent  buffer 512    (parser writes here)
   evtC       chan parser.ParsedEvent  buffer 512    (counter reads here)
   instrCntC  chan counter.EventCount  buffer 64     (counter writes here)
   cntC       chan counter.EventCount  buffer 64     (analyzer reads here)
   anomC      chan analyzer.Anomaly    buffer 32     (analyzer writes, alert loop reads)
   ```
   Instrumented channels (`instr*`) sit between stages. Forwarding goroutines read from `instr*`, call the appropriate `Recorder` method, and forward to the real channel. All channels are created here — no stage creates or closes a channel.

3. **Stage construction:**
   - `tailer.New(cfg.LogFile, cfg.Follow, cfg.PollInterval)`
   - `parser.NewPool(cfg.Format, workers)` — workers defaults to `runtime.NumCPU()` if `cfg.ParserWorkers < 1`
   - `counter.New(cfg.BucketDuration)`
   - `window.New(int(cfg.WindowSize / cfg.BucketDuration))`
   - `analyzer.New(cfg, win)`

4. **Context derivation — `ctrCtx`:**
   ```go
   ctrCtx, ctrCancel := context.WithCancel(ctx)
   ```
   The counter receives `ctrCtx` instead of the parent `ctx`. `ctrCancel` is called from two places:
   - The parser pool goroutine calls `ctrCancel()` after `pool.Run` returns — this handles the `follow=false` (natural EOF) case where `ctx` is never cancelled.
   - The counter goroutine calls `defer ctrCancel()` — idempotent safety net.

   **Why this is needed:** When `follow=false`, the tailer reads to EOF and exits, which cascades through the pool (rawC closes → workers exit → evtC closes). The counter sets `in = nil` when evtC closes but does not exit — it keeps running its ticker loop, waiting for `ctx.Done()`. Without `ctrCancel()`, the counter would loop forever since no one cancels the parent context. Deriving `ctrCtx` and cancelling it when the pool exits solves this cleanly.

5. **Goroutine topology — 9 goroutines (5 stages + 3 forwarding + 1 sampler):**

   | # | Role | Input | Output | Context | Exit trigger |
   |---|------|-------|--------|---------|-------------|
   | 1 | Tailer | file/stdin | instrRawC | `ctx` | `ctx.Done()` or EOF (follow=false) |
   | 2 | Fwd: IncLinesRead | instrRawC | rawC | — | instrRawC closes |
   | 3 | Parser pool | rawC | instrEvtC | `ctx` | rawC closes (all workers see `!ok`) |
   | 4 | Fwd: IncLinesParsed | instrEvtC | evtC | — | instrEvtC closes |
   | 5 | Counter | evtC | instrCntC | `ctrCtx` | `ctrCtx.Done()` (fires on SIGTERM or pool exit) |
   | 6 | Fwd: SetEventsPerSecond + SetBaselineRate | instrCntC | cntC | — | instrCntC closes |
   | 7 | Analyzer | cntC | anomC | `context.Background()` | cntC closes (`!ok` branch) |
   | 8 | Alert loop + ObserveAnomaly + IncAlertsSent | anomC | — | `ctx` | anomC closes (`range` exits) |
   | 9 | Channel depth sampler | — | — | `ctx` | `ctx.Done()` or `samplerDone` closes |

   Each goroutine adds to a shared `sync.WaitGroup` and calls `defer wg.Done()`.

   **Forwarding goroutines (2, 4, 6)** are transparent: they read from an `instr*` channel, call one or more `Recorder` methods, and forward the item unchanged to the real channel. They exit when their input channel closes and close their output channel via `defer close(...)`.

   **Channel depth sampler (9)** runs a 1-second `time.Ticker` and samples `len()` of all 4 real channels (`rawC`, `evtC`, `cntC`, `anomC`). It exits when either `ctx` is cancelled or `samplerDone` is closed (by the alert loop goroutine). The `samplerDone` signal prevents the sampler from blocking `wg.Wait()` in `follow=false` mode where `ctx` is never cancelled.

   **Counter output forwarding (6)** also calls `rec.SetBaselineRate(window.Mean(snap))` — it takes a snapshot of the `Window` via `win.Snapshot()`. This is safe because `Window` is protected by `sync.RWMutex` and the forwarding goroutine only reads.

6. **`wg.Wait()`** — blocks until all 9 goroutines have exited.

7. **Alerter cleanup:** After `wg.Wait()`, type-asserts the alerter to `io.Closer`. If the assertion succeeds (e.g. `FileAlerter`), calls `Close()` to release the file handle. Errors from `Close()` are logged via `slog.Warn` but do not change the return value.

---

### Ordered Shutdown Cascade

The shutdown wave propagates downstream via `defer close(ch)` in each goroutine:

```
ctx.Cancel() (SIGTERM) or EOF (follow=false)
  │
  ▼  goroutine 1: tailer.Run returns
  │  └─ defer close(instrRawC)
  ▼  goroutine 2: fwd drains instrRawC (IncLinesRead per line)
  │  └─ defer close(rawC)
  ▼  goroutine 3: pool.Run returns (workers drain rawC → see !ok → exit)
  │  └─ defer close(instrEvtC)
  │  └─ defer ctrCancel()        ← wakes counter for follow=false case
  ▼  goroutine 4: fwd drains instrEvtC (IncLinesParsed per event)
  │  └─ defer close(evtC)
  ▼  goroutine 5: counter.Run returns (ctrCtx.Done → drains evtC → flushes final bucket)
  │  └─ defer close(instrCntC)
  ▼  goroutine 6: fwd drains instrCntC (SetEventsPerSecond, SetBaselineRate per bucket)
  │  └─ defer close(cntC)
  ▼  goroutine 7: analyzer.Run returns (cntC closed → !ok branch)
  │  └─ defer close(anomC)
  ▼  goroutine 8: alert loop returns (range anomC exits; ObserveAnomaly + IncAlertsSent)
  │  └─ defer close(samplerDone)
  ▼  goroutine 9: sampler exits (samplerDone or ctx.Done)
  │
  ▼  wg.Wait() unblocks
  │  → alerter.Close() if io.Closer
  │  → Run returns nil
```

**Why the analyzer uses `context.Background()`:** If the analyzer received the same `ctx` as the tailer, it would exit immediately on `ctx.Done()` — potentially before the counter has flushed its final bucket into cntC. By using `context.Background()`, the analyzer only exits when cntC is closed (which happens *after* the counter has finished draining and flushing). This ensures no final-bucket anomalies are lost.

**Why the alert loop uses `range anomC`:** `range` blocks until the channel is closed, guaranteeing every anomaly queued before shutdown is delivered to the alerter before `Run` returns. No anomaly is silently dropped.

---

### Alert Loop (with Metrics)

```go
samplerDone := make(chan struct{})
defer close(samplerDone)
for a := range anomC {
    rec.ObserveAnomaly(string(a.Kind))
    if err := p.alerter.Send(ctx, a); err != nil {
        slog.Error("alert delivery failed", ...)
    } else {
        rec.IncAlertsSent(p.alerter.Name())
    }
}
```

- `rec.ObserveAnomaly(kind)` is called for **every** anomaly, regardless of delivery success.
- `rec.IncAlertsSent(alerterName)` is called **only** after successful delivery.
- `close(samplerDone)` after the loop exits signals the channel depth sampler to stop.

The loop passes `ctx` to `Send`, so webhook retries respect the parent context's cancellation. If `ctx` is already cancelled when an anomaly arrives, the `MultiAlerter`'s goroutines will abort quickly (webhook `Send` checks `ctx.Done()` before each retry sleep). Console and file alerters ignore the context — their `Send` is synchronous and instant.

Errors from `Send` are logged but do not stop the loop — a failed webhook delivery must not prevent the next anomaly from being attempted.

---

### Metrics Wiring

All `Recorder` method calls live in `pipeline.go` — no changes to tailer, parser, counter, or analyzer packages.

| Recorder Method | Where Called | Trigger |
|-----------------|-------------|---------|
| `IncLinesRead()` | Forwarding goroutine (instrRawC → rawC) | Once per raw line from tailer |
| `IncLinesParsed()` | Forwarding goroutine (instrEvtC → evtC) | Once per successfully parsed event |
| `SetEventsPerSecond(v)` | Forwarding goroutine (instrCntC → cntC) | Once per EventCount bucket, value = `ec.Total` |
| `SetBaselineRate(v)` | Forwarding goroutine (instrCntC → cntC) | Once per EventCount bucket, value = `window.Mean(snap)` |
| `ObserveAnomaly(kind)` | Alert loop | Once per anomaly received |
| `IncAlertsSent(alerter)` | Alert loop | Once per successful `Send()` |
| `SetChannelDepth(name, depth)` | Channel depth sampler goroutine | Once per second for 4 channels: `raw`, `parsed`, `counted`, `anomaly` |

**Channel depth sampler** exits on either `ctx.Done()` (SIGTERM) or `samplerDone` closed (alert loop finished). The `samplerDone` channel prevents the sampler from blocking `wg.Wait()` in `follow=false` mode.

---

### Channel Buffer Sizes

| Channel | Buffer | Rationale |
|---------|--------|-----------|
| instrRawC / rawC | 1024 each | Absorbs bursty file reads; forwarding goroutine between them adds negligible latency |
| instrEvtC / evtC | 512 each | Parser pool is N× concurrent; forwarding goroutine calls IncLinesParsed inline |
| instrCntC / cntC | 64 each | Counter emits one bucket per `BucketDuration` (typically 1/s); forwarding goroutine calls SetEventsPerSecond + SetBaselineRate |
| anomC | 32 | Anomalies are rare events (at most 5 kinds per bucket × cooldown filtering); 32 is ample |

---

### What is NOT in pipeline

- **Signal handling** — `main.go` owns `signal.NotifyContext`; it passes an already-cancellable `ctx` to `Run`.
- **Config validation** — done in `main.go` before `New` is called.
- **Metrics exposition** — pipeline stages call `metrics.Recorder` methods; the metrics HTTP server is started by `main.go`, not by the pipeline.

---

## internal/metrics — Prometheus Metrics Exposition

**File:** `internal/metrics/metrics.go`

**Purpose:** Provides pipeline instrumentation via Prometheus counters and gauges. Exposes a `/metrics` HTTP endpoint for scraping. Designed around a `Recorder` interface so the pipeline is decoupled from Prometheus — a `NoopRecorder` is used when metrics are disabled.

---

### Recorder Interface

```go
type Recorder interface {
    IncLinesRead()
    IncLinesParsed()
    SetEventsPerSecond(v float64)
    SetBaselineRate(v float64)
    ObserveAnomaly(kind string)
    IncAlertsSent(alerter string)
    SetChannelDepth(name string, depth int)
}
```

All methods are safe to call concurrently. Pipeline stages call these methods — no direct prometheus imports outside the metrics package.

---

### NoopRecorder

```go
type NoopRecorder struct{}
```

All 7 methods are empty no-ops. Used when `--metrics-addr` is not set. Zero allocation, zero overhead. Satisfies `Recorder` as a value type — no pointer needed.

---

### PrometheusRecorder

```go
type PrometheusRecorder struct {
    registry     *prometheus.Registry
    linesRead    prometheus.Counter
    linesParsed  prometheus.Counter
    eventsPerSec prometheus.Gauge
    baselineRate prometheus.Gauge
    anomalies    *prometheus.CounterVec   // label: kind
    alertsSent   *prometheus.CounterVec   // label: alerter
    channelDepth *prometheus.GaugeVec     // label: channel
}
```

**Key design: custom registry.** Each `PrometheusRecorder` creates its own `prometheus.Registry` instead of using `prometheus.DefaultRegisterer`. This means:
- Parallel tests never collide ("already registered" panics impossible)
- Multiple pipeline instances can coexist in the same process
- The `Handler()` method serves only this recorder's metrics

#### Constructor

```go
func NewPrometheusRecorder() *PrometheusRecorder
```

Creates all 7 collectors with namespace `log_analyser`, registers them on a fresh private registry, and returns the recorder. No goroutines are started.

#### Methods

| Method | Prometheus Type | Metric Name | Labels |
|--------|----------------|-------------|--------|
| `IncLinesRead()` | Counter | `log_analyser_lines_read_total` | — |
| `IncLinesParsed()` | Counter | `log_analyser_lines_parsed_total` | — |
| `SetEventsPerSecond(v)` | Gauge | `log_analyser_events_per_second` | — |
| `SetBaselineRate(v)` | Gauge | `log_analyser_baseline_rate` | — |
| `ObserveAnomaly(kind)` | Counter | `log_analyser_anomalies_total` | `kind` |
| `IncAlertsSent(alerter)` | Counter | `log_analyser_alerts_sent_total` | `alerter` |
| `SetChannelDepth(name, depth)` | Gauge | `log_analyser_pipeline_channel_depth` | `channel` |

Counters use `Inc()` (monotonically increasing). Gauges use `Set()` (overwrites previous value).

#### Handler

```go
func (r *PrometheusRecorder) Handler() http.Handler
```

Returns a `promhttp.HandlerFor` backed by the recorder's private registry. Used by `Server` and directly by tests via `httptest.NewRecorder`.

---

### Server

```go
type Server struct {
    http *http.Server
}
```

Wraps `http.Server` to expose `/metrics`.

#### Constructor

```go
func NewServer(addr string, rec *PrometheusRecorder) *Server
```

Creates an `http.ServeMux` with a single `/metrics` route backed by the recorder's handler. Binds to `addr` (e.g. `":9090"`, `"127.0.0.1:0"`).

#### Methods

| Method | Description |
|--------|-------------|
| `Handler() http.Handler` | Returns the server's root handler (for `httptest`) |
| `ListenAndServe() error` | Blocks until shutdown; returns `http.ErrServerClosed` on graceful stop |
| `Shutdown() error` | Graceful shutdown with a 5-second drain timeout via `context.WithTimeout` |

---

### Label Cardinality

All labels have bounded, enum-like values:

| Label | Possible Values | Source |
|-------|----------------|--------|
| `kind` | `rate_spike`, `error_surge`, `latency_spike`, `host_flood`, `silence` | `analyzer.AnomalyKind` constants |
| `alerter` | `console`, `webhook`, `file` | `alerter.Name()` return values |
| `channel` | `raw`, `parsed`, `counts`, `anomalies` | Pipeline channel names |

No risk of label explosion — all values are compile-time constants.

---

### Integration Pattern

```
main.go:
  if metricsAddr != "" {
      rec := metrics.NewPrometheusRecorder()
      srv := metrics.NewServer(metricsAddr, rec)
      go srv.ListenAndServe()
      defer srv.Shutdown()
  } else {
      rec = metrics.NoopRecorder{}
  }
  pipeline.New(cfg, alerter, rec).Run(ctx)
```

Pipeline calls `rec.IncLinesRead()` etc. inside `Run`. The `Recorder` interface means pipeline code has no `if metricsEnabled` conditionals — the noop path is handled by the type system.

---

## cmd/loganalyser — CLI Entry Point

**File:** `cmd/loganalyser/main.go`

**Purpose:** Single binary entry point that wires config loading, signal handling, alerter construction, metrics server, and pipeline startup. No business logic — pure orchestration.

---

### Package-Level Variables

```go
var version = "dev"
```

Set at build time via `-ldflags "-X main.version=v1.0.0"`. The `--version` flag prints this value.

---

### Functions

#### `main()`

Calls `newRootCmd().Execute()` and exits with code 1 on error.

#### `newRootCmd() *cobra.Command`

Builds the single Cobra root command. No subcommands.

**Flag registration** (21 flags):

| Flag | Short | Default | Type | Config key mapped to |
|------|-------|---------|------|---------------------|
| `--file` | `-f` | `""` (stdin) | string | `LogFile` |
| `--follow` | `-F` | `true` | bool | `Follow` |
| `--format` | | `"auto"` | string | `Format` |
| `--window` | | `60s` | duration | `WindowSize` |
| `--bucket` | | `1s` | duration | `BucketDuration` |
| `--spike-multiplier` | | `3.0` | float64 | `SpikeMultiplier` |
| `--error-rate` | | `0.05` | float64 | `ErrorRateThreshold` |
| `--host-flood` | | `0.5` | float64 | `HostFloodFraction` |
| `--latency-multiplier` | | `3.0` | float64 | `LatencyMultiplier` |
| `--silence` | | `30` | int | `SilenceThreshold` |
| `--cooldown` | | `30s` | duration | `AlertCooldown` |
| `--min-baseline` | | `10` | int | `MinBaselineSamples` |
| `--detection-method` | | `"ratio"` | string | `DetectionMethod` |
| `--workers` | | `0` (NumCPU) | int | `ParserWorkers` |
| `--webhook` | | `""` | string | `Alerters.Webhook.URL` |
| `--alert-file` | | `""` | string | `Alerters.File.Path` |
| `--metrics-addr` | | `""` | string | `MetricsAddr` |
| `--config` | `-c` | `""` | string | (config file path) |
| `--verbose` | `-v` | `false` | bool | (logging level) |

**RunE execution flow:**

1. `loadConfig(cmd, cfgFile)` — load from file/env, apply flag overrides, validate
2. `setupLogging(verbose)` — configure slog level
3. `buildAlerters(cfg)` — construct MultiAlerter
4. `buildRecorder(cfg)` — construct Recorder + optional Server
5. Start metrics server goroutine (if non-nil), defer `Shutdown()`
6. Get context from `cmd.Context()` or create via `signal.NotifyContext(SIGINT, SIGTERM)`
7. `pipeline.New(cfg, alerter, recorder).Run(ctx)`

#### `loadConfig(cmd *cobra.Command, cfgFile string) (*config.Config, error)`

Two paths:
- `cfgFile != ""` → `config.Load(cfgFile)` (file + env merge)
- `cfgFile == ""` → `config.LoadEnv()` (env + defaults only)

Then calls `applyFlagOverrides(cmd, cfg)` to write CLI flag values into the config, but **only for flags that were explicitly set** (`cmd.Flags().Changed(name)`). This preserves the precedence: flag > env > file > default.

Finally calls `cfg.Validate()`.

#### `applyFlagOverrides(cmd *cobra.Command, cfg *config.Config)`

Checks `cmd.Flags().Changed(name)` for each of 18 flags. Only overrides the config field if the user explicitly passed the flag on the command line. This prevents default flag values from overwriting file/env values.

#### `setupLogging(verbose bool)`

Sets `slog.SetDefault` with `LevelInfo` (default) or `LevelDebug` (when `--verbose`). Uses `slog.NewTextHandler` writing to `os.Stderr`.

#### `buildAlerters(cfg config.Config) (*alerter.MultiAlerter, error)`

Always includes `ConsoleAlerter(os.Stderr, cfg.Alerters.Console.UseColor)`.

Conditionally adds:
- **WebhookAlerter** — when `URL != ""` or `Enabled == true`. Passes `cfg.Alerters.Webhook.Timeout` as `http.Client` timeout and `MaxRetries` from config. Returns error if `NewWebhookAlerter` fails (e.g. empty URL with Enabled=true).
- **FileAlerter** — when `Path != ""`. Returns error if `NewFileAlerter` fails (e.g. parent directory doesn't exist).

Wraps all in `alerter.NewMultiAlerter(alerters...)`.

#### `buildRecorder(cfg config.Config) (metrics.Recorder, *metrics.Server)`

- `MetricsAddr == ""` → returns `(NoopRecorder{}, nil)` — zero overhead
- `MetricsAddr != ""` → returns `(PrometheusRecorder, Server)` — caller starts server in goroutine

---

### Pipeline Signature Change

`pipeline.New` was updated from 2 to 3 arguments:

```go
// Before
func New(cfg config.Config, al alerter.Alerter) *Pipeline

// After
func New(cfg config.Config, al alerter.Alerter, rec metrics.Recorder) *Pipeline
```

If `rec` is nil, the constructor substitutes `metrics.NoopRecorder{}`. All existing pipeline tests pass `nil` as the third argument.

---

### Signal Handling

```go
ctx := cmd.Context()
if ctx == nil {
    ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()
}
```

In production, `main()` calls `cmd.Execute()` which provides no context, so the signal handler creates one. In tests, `cmd.ExecuteContext(ctx)` provides a pre-built context — no signal wiring needed, making tests deterministic.

---

### What is NOT in main.go

- **Business logic** — all detection, parsing, alerting logic lives in `internal/` packages
- **Config validation rules** — implemented in `config.Validate()`
- **Alerter retry logic** — handled inside `WebhookAlerter.Send()`
- **Ordered shutdown** — handled by `pipeline.Run()` via defer-close cascade
