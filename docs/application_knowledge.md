# Application Knowledge

Implementation reference for all components built in the log_analyser project.
Each section covers: purpose, data structures, every function, and key implementation notes.

---

## Table of Contents

1. [pkg/ringbuf — Generic Circular Buffer](#pkgringbuf--generic-circular-buffer)
2. [internal/parser — ParsedEvent type](#internalparser--parsedevent-type)
3. [internal/counter — Bucket Aggregation](#internalcounter--bucket-aggregation)
4. [internal/window — Sliding Window](#internalwindow--sliding-window)
5. [internal/window/stats — Statistical Functions](#internalwindowstats--statistical-functions)
6. [internal/config — Configuration Loading](#internalconfig--configuration-loading)

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

## internal/parser — ParsedEvent type

**File:** `internal/parser/parser.go`

**Purpose:** Defines the normalised event type that all log-format parsers must produce. Acts as the contract between the parser layer and the counter layer.

### Data Structure

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
- `Level` is lower-cased by convention so comparisons are simple string equality.

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

| Branch | Action |
|--------|--------|
| `case e, ok := <-in` | If `ok`, calls `cur.add(e)`. If channel is closed (`!ok`), sets `in = nil` to prevent the select from spinning on zero-value reads from a closed channel. |
| `case t := <-ticker.C` | Calls `cur.flush(t)`, sends result to `out`, starts a fresh `newBucket(t)`. Fires even if no events arrived — emits a zero-total bucket which the analyzer needs for silence detection. |
| `case <-ctx.Done()` | Performs a non-blocking drain of any events still buffered in `in`, then calls `cur.flush(time.Now())` and sends the final partial bucket to `out` before returning. Guarantees no events are lost on SIGTERM. |

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

| Rule | Condition |
|------|-----------|
| Format | Must be one of: `auto`, `nginx`, `apache`, `json`, `syslog` |
| DetectionMethod | Must be `ratio` or `sigma` |
| WindowSize | Must be >= BucketDuration |
| SpikeMultiplier | Must be > 0 |
| ErrorRateThreshold | Must be in (0, 1] |
| HostFloodFraction | Must be in (0, 1] |
| LatencyMultiplier | Must be > 0 |
| MinBaselineSamples | Must be > 0 |
| AlertCooldown | Must be >= 0 |
| Webhook enabled | URL must not be empty |
| File enabled | Path must not be empty |

Returns the first error encountered (early-return style). Returns `nil` if all rules pass.
