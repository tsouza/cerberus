# ClickHouse optimizations

This document is the canonical spec for cerberus's ClickHouse-optimization
suite: the configuration surface, the feature registry and version gating,
the runtime version probe, the per-feature behaviour, the legacy alias
migration, and the asynchronous `system.query_log` performance-corpus
reconciler that closes the loop between an emitted plan shape and its
observed server-side cost.

The suite is one cohesive capability. Every feature is version-safe by
construction: cerberus's supported ClickHouse floor is 24.8, and a feature
whose minimum version sits above the connected server is simply not
enabled, so the binary keeps emitting its 24.8-safe SQL unchanged. There is
no behaviour an operator must "turn off" to stay safe on 24.8 — the default
posture (`auto`) only ever enables stable features the server actually
supports, and never an experimental one.

## The two configuration knobs

Two environment variables drive the whole suite. Both follow the standard
cerberus config idiom (per-key viper `BindEnv`, fail-fast parse, env > file
> default).

| Env var                            | Type     | Default       | Meaning                                                                            |
| ---------------------------------- | -------- | ------------- | ---------------------------------------------------------------------------------- |
| `CERBERUS_CH_OPTIMIZATIONS`        | string   | `auto`        | `auto`, `off`, or a comma-separated list of feature ids (`auto` may appear in it). |
| `CERBERUS_CH_OPTIMIZATIONS_MODE`   | string   | `enforcing`   | `enforcing` or `permissive`. Governs how an unsupported requested id is handled.   |

### `CERBERUS_CH_OPTIMIZATIONS`

The value is a comma-separated list of tokens; each is `auto`, `off`, or a
feature id, and they **compose**:

- **`auto`** (default) — enable every **stable** feature whose minimum
  version is `<=` the connected server's version. Experimental / opt-in
  features are **never** auto-enabled; they require explicit listing. This
  preserves the historical "experimental paths off out of the box" default.
  `auto` may appear **alongside** explicit ids, so
  `auto,columnar_result_decode` means "the auto-selected set **plus**
  `columnar_result_decode`" — the way to add an opt-in feature without giving
  up version-aware auto-selection of the rest.
- **`off`** — enable nothing. The empty set. Every optimization stays dark.
  `off` is **absolute** and may not be combined with any other token.
- **a feature id** — e.g. `aggregation_in_order,condition_cache`. Enable
  exactly the listed feature ids (subject to version gating, see mode below).
  An explicit id keeps its "I require this" semantics even next to `auto`. An
  **unknown** id is **always** a fatal startup error (a typo guard),
  regardless of mode.

### `CERBERUS_CH_OPTIMIZATIONS_MODE`

The mode only matters for an **explicit list** that names a feature the
connected server is too old to support. It is **ignored** under `auto` and
`off` (under `auto` an unsupported feature is silently skipped because auto
is "best available"; under `off` nothing is selected at all).

- **`enforcing`** (default) — an explicitly-requested but unsupported feature
  is a **FATAL startup error** naming the feature, the required version, and
  the server version. The process exits non-zero. This is the default because
  `auto`/`off` already cover the graceful paths, so an operator who names an
  explicit feature list is asserting "I require these".
- **`permissive`** — an explicitly-requested but unsupported feature is
  **skipped with a `WARN`**:
  `ch_opt '<id>' disabled: needs ClickHouse >=X.Y, server is A.B`.
  Startup continues.

An **unknown** feature id is fatal in **both** modes.

## Resolution

Resolution runs **once at startup**, after the runtime version probe, and
produces an immutable `EnabledSet` that is logged at boot. It is the single
source of truth every consumer reads from; nothing downstream re-reads the
raw env.

| `CERBERUS_CH_OPTIMIZATIONS`   | Effect                                                                                                                          |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `off`                         | Empty set.                                                                                                                      |
| `auto`                        | Every stable feature with `minVersion <= server`. Experimental features excluded.                                               |
| explicit list                 | Per id: supported -> enable; unsupported -> `enforcing`: FATAL / `permissive`: WARN + skip. Unknown id -> FATAL (both modes).   |

The boot log records the resolved set, the server version it resolved
against, and any skips or the deprecation notice (below).

## Feature registry

Each feature is a registry entry: a stable id, a minimum `major.minor`
version, a stability class (`stable` or `experimental`), and an `apply`
behaviour that acts on the per-query path when the feature is in the resolved
set.

The structural columns below (`id` / `minVersion` / `stability`) are
**generated** from `internal/chopt/registry.go` -- the single source of truth --
and live inside the `BEGIN/END GENERATED` markers. Do not hand-edit them: run
`just gen-opt-docs` (it calls `chopt.Registry()` and rewrites the block), and CI
fails any PR whose block drifts from the registry. Adding a feature to the
registry therefore lands here automatically; it can never go missing from the
table.

<!-- BEGIN GENERATED: chopt-feature-table (do not edit; regenerate with `just gen-opt-docs`) -->
| id                       | minVersion | stability    |
| ------------------------ | ---------- | ------------ |
| `aggregation_in_order`   | 24.8       | stable       |
| `condition_cache`        | 25.3       | stable       |
| `ts_grid_range`          | 25.6       | experimental |
| `ts_grid_resample`       | 25.6       | experimental |
| `columnar_result_decode` | none       | experimental |
| `ts_grid_changes`        | 25.9       | experimental |
| `ts_grid_resets`         | 25.9       | experimental |
<!-- END GENERATED: chopt-feature-table -->

The rich, hand-authored columns below stay OUTSIDE the generated block: they
carry operator judgement the registry cannot derive. The "experimental setting"
column is informational -- where a feature needs an `allow_experimental_*`
setting, that setting is co-stamped by the **engine plan path** (it inspects the
post-optimize plan and stamps the setting on exactly the queries that use the
native node), not carried as a registry field. `columnar_result_decode` is the
`experimental`-class opt-in feature whose perf-tradeoff framing is described as
"opt-in" in the effect prose.

| id                       | experimental setting                                 | effect                                                                                                                                                                                             |
| ------------------------ | ---------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `aggregation_in_order`   | (none)                                               | stamps `optimize_aggregation_in_order=1` when the plan's Aggregate GROUP BY is a bare-column prefix of the scanned table's sorting key. Result-equivalent.                                         |
| `condition_cache`        | (none)                                               | stamps `use_query_condition_cache=1` (+`enable_analyzer=1`, analyzer-gated) on predicate-stable read paths. Result-equivalent (a cache).                                                           |
| `ts_grid_range`          | `allow_experimental_time_series_aggregate_functions` | opts eligible `rate(<counter>[<range>])` query_range shapes onto the native `timeSeriesRateToGrid` aggregate. Explicit-only (never auto).                                                          |
| `ts_grid_resample`       | `allow_experimental_time_series_aggregate_functions` | opts the range-mode instant-vector staleness shape onto the native `timeSeriesResampleToGridWithStaleness` aggregate, retiring the argMax fan-out. Explicit-only (never auto).                     |
| `columnar_result_decode` | (none)                                               | client-side: decodes the `query_range` matrix shape via the ch-go columnar path (label map built once per run, not per row). No server setting, no version floor. Explicit-only (never auto).      |
| `ts_grid_changes`        | `allow_experimental_time_series_aggregate_functions` | opts eligible `changes(<v>[<range>])` query_range shapes onto the native `timeSeriesChangesToGrid` aggregate, retiring the `arrayPopBack`/`arrayPopFront` fan-out. Explicit-only (never auto).     |
| `ts_grid_resets`         | `allow_experimental_time_series_aggregate_functions` | opts eligible `resets(<counter>[<range>])` query_range shapes onto the native `timeSeriesResetsToGrid` aggregate, retiring the `arrayPopBack`/`arrayPopFront` fan-out. Explicit-only (never auto). |

Notes:

- **`aggregation_in_order`** is the migration of the dark
  `optimize_aggregation_in_order` rule into the registry. The eligibility
  check (single Aggregate, all GROUP BY keys bare columns, single physical
  table, GROUP BY an ordered prefix of the schema sorting key) is unchanged;
  only its enablement now flows from the resolved set.
- **`condition_cache`** activates only on server `>= 25.3` and only on a
  predicate-stable read path, gated conservatively (it needs the analyzer);
  below 25.3 it is a no-op. The query condition cache is result-equivalent,
  so it is safe to ship under `auto` for supporting servers.
- **`ts_grid_range`** is experimental: it is **never** enabled by `auto`. It
  is reachable only by explicit listing or by the legacy alias (below). Its
  native aggregate requires the experimental setting to be co-stamped on
  exactly the queries that emit the native node.
- **`ts_grid_resample`** is experimental: **never** enabled by `auto`, no
  legacy alias — list it in `CERBERUS_CH_OPTIMIZATIONS` to enable. It shares
  the `timeSeries*ToGrid` family floor (25.6) and the same experimental setting
  as `ts_grid_range`, co-stamped on exactly the queries that emit the native
  resample node. The two features are independent (either can be on without the
  other): the PromQL lowering wires each as a separate boot-decided strategy.
  The native function uses a CLOSED left-edge staleness window
  (`[anchor - lookback, anchor]`) which matches reference Prometheus, vs the
  fan-out's half-open `(anchor - lookback, anchor]`; they diverge only on a
  sample landing exactly on the left boundary.
- **`columnar_result_decode`** is a **client-side** decode optimization with
  **no version floor** (`minVersion` is the always-available zero floor): it
  changes how cerberus reads the result blocks, not what it asks the server to
  do, so it works on any native-protocol server and touches no ClickHouse
  setting. It is **opt-in only** (a perf tradeoff — it owns a second ch-go dial,
  established lazily on the first `query_range` matrix query), so `auto` never
  selects it; list it explicitly
  (`CERBERUS_CH_OPTIMIZATIONS=columnar_result_decode`) to engage it. The decode
  is byte-parity-verified against the row path (`TestColumnarMatrixParity_E2E`).
  It is the registry's example of a non-version-gated opt-in feature.
- **`ts_grid_changes`** is experimental and explicit-only (**never** `auto`,
  no legacy alias). Its floor is **25.9**, NOT the 25.6 of rate/resample:
  `timeSeriesChangesToGrid`/`timeSeriesResetsToGrid` shipped a full quarter
  later (ClickHouse 25.9). A 25.6 floor would mis-advertise support on
  25.6-25.8 servers and 502 with `UNKNOWN_AGGREGATE_FUNCTION`. It shares the
  family's experimental setting, co-stamped on exactly the queries that emit
  the native changes node.
- **`ts_grid_resets`** is the sibling of `ts_grid_changes` (same PR upstream):
  experimental, explicit-only, same **25.9** floor, same experimental setting.
  It opts eligible `resets(<counter>[<range>])` shapes onto the native
  `timeSeriesResetsToGrid` aggregate, retiring the per-window counter-reset
  fan-out.

## Runtime version probe

At connection init the client issues `SELECT version()` once, parses the
result to a comparable `major.minor` struct, and exposes a
`serverAtLeast(major, minor)` predicate. Resolution consumes this probe to
decide which registry features the server supports.

Patch and build suffixes (`25.8.2.1`, `25.8.2.1-lts`) are dropped: feature
availability lands at minor-version granularity, so the comparison is over
`(major, minor)` only. This mirrors the existing preflight version parse.

The probe runs **once**. A rolling ClickHouse upgrade that crosses a feature
floor needs a cerberus **restart/reconnect** to re-probe and re-resolve.
This is the documented v1 behaviour.

## Legacy alias: `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`

The legacy boolean `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` keeps working and
is **re-routed through the resolver** rather than read directly by its
downstream consumers. It maps onto the `ts_grid_range` registry feature:

The legacy alias only takes effect under the **default `auto`** selection;
any explicit `CERBERUS_CH_OPTIMIZATIONS` choice (a feature list **or** the
`off` kill-switch) overrides it.

- **explicitly `true`** (under `auto`) — force-enable `ts_grid_range` (as if
  it were listed), still subject to version gating and mode.
- **explicitly `false`** (under `auto`) — force-disable `ts_grid_range`, even
  if it would otherwise be selected.
- **unset** — no effect. The framework resolves normally; under `auto`,
  `ts_grid_range` stays off because it is experimental.
- **legacy set AND any explicit `CERBERUS_CH_OPTIMIZATIONS` choice** (a feature
  list **or** `off`) — the new `CERBERUS_CH_OPTIMIZATIONS` **wins**. The legacy
  flag is ignored with a `WARN` (or FATAL under `enforcing`). In particular
  `off` is **absolute**: a stale legacy env var can never resurrect
  `ts_grid_range` under `off`.

When the legacy flag is set, cerberus emits a **one-time startup deprecation
warning** pointing to `CERBERUS_CH_OPTIMIZATIONS`.

The existing `Config.ExperimentalTSGridRange` bool field still exists and
keeps compiling for its consumers (the PromQL lowering, the engine native
gate, the preflight version floor). It is now **populated from the resolved
`EnabledSet`** — `ts_grid_range in set` — so the set is the single source of
truth and the consumers read a derived value, not the raw env.

> **Deprecated:** `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` is soft-deprecated.
> Use `CERBERUS_CH_OPTIMIZATIONS` (list `ts_grid_range` to enable the native
> rate path). The legacy flag remains honoured for backward compatibility.

## The `system.query_log` performance-corpus reconciler

A background reconciler closes the loop between a plan shape cerberus
emitted and the cost ClickHouse actually paid for it, building a durable
corpus an operator can mine to decide which optimizations to enable.

It is **disabled by default** and gated behind its own `CERBERUS_*` flag. It
requires `system.query_log` access and is **production-only**: chDB (the
parity test substrate) has no `system.query_log`, so the reconciler is
guarded off there.

### What it does

- Keeps a **bounded** in-memory ring/map of recently-dispatched cerberus
  `query_id`s, each mapped to `{shape-id, enabled-opts, query language}`.
  The `query_id` is the per-dispatch `<trace id>-<span id>-<counter>` stamp
  (unique per CH dispatch, with the trace id as its prefix) and the shape-id is
  the literal-free `cerb:<root>[;<modifier>...]` log_comment shape from the
  instrumentation foundation.
- **Periodically** (configurable interval) issues a single rate-limited
  `SELECT` against `system.query_log` for the recent ids:
  `WHERE query_id IN (recent ids) AND type = 'QueryFinish'`, reading
  `read_rows`, `read_bytes`, `query_duration_ms`, `memory_usage`,
  `ProfileEvents` (notably `QueryConditionCacheHits` and
  `RowsReadByPrewhereReaders`), and `normalized_query_hash`. The scan is
  bounded to a recent event-time window **and** carries conservative
  ClickHouse resource caps (`max_execution_time`, `max_threads=1`, a low
  `priority`, `max_rows_to_read` / `max_bytes_to_read` with `break` overflow)
  plus a client-side context deadline, so it can never starve the data plane
  or pin the reconciler goroutine even on a huge `system.query_log`.
- **Joins** each row back to its shape-id and writes the
  `(shape-id, enabled-opts, timings)` tuple to a durable sink. The v1 sink
  is a JSONL file at a configurable path; the row shape is exposed so a
  later ClickHouse-table sink is a trivial swap.

### Guarantees

- Memory is bounded: a fixed-size circular ring evicts the oldest id in
  O(1) (no per-query reindex).
- **Data-plane isolation**: the dispatch seam does a single non-blocking
  channel send and returns — it never takes the ring lock, never serializes
  the prom/loki/tempo head engines against each other, and never pays any
  per-query ring cost. The `Run` goroutine drains that channel into the ring.
  Under a momentary burst the seam drops the sample (the corpus is a
  best-effort sample, not a system of record) rather than block a query.
- The query is rate-limited to one batch per interval and resource-capped
  (see above) so it cannot compete with data-plane queries unbounded.
- Errors are **logged, never fatal** — a query_log read failure degrades the
  corpus, it never takes the binary down.
- Clean shutdown on context cancel.

### Config flags

| Env var                              | Type       | Default   | Meaning                                                      |
| ------------------------------------ | ---------- | --------- | ------------------------------------------------------------ |
| `CERBERUS_CH_OPT_CORPUS_ENABLED`     | bool       | `false`   | Enable the reconciler (needs `system.query_log` access).     |
| `CERBERUS_CH_OPT_CORPUS_INTERVAL`    | duration   | `60s`     | How often to reconcile recent query_ids against query_log.   |
| `CERBERUS_CH_OPT_CORPUS_SINK_PATH`   | string     | (unset)   | JSONL sink path. Empty disables the file sink.               |

### Mining the corpus

The JSONL corpus (and the `log_comment` shape ids directly in
`system.query_log`) let an operator rank plan shapes by cost. Top shapes by
p99 duration:

```sql
SELECT
  normalized_query_hash,
  any(log_comment)                          AS shape,
  count()                                   AS runs,
  quantile(0.99)(query_duration_ms)         AS p99_ms,
  max(memory_usage)                         AS peak_mem
FROM system.query_log
WHERE type = 'QueryFinish'
  AND log_comment LIKE 'cerb:%'
  AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY p99_ms DESC
LIMIT 20;
```

Top shapes by peak memory:

```sql
SELECT
  normalized_query_hash,
  any(log_comment)         AS shape,
  count()                  AS runs,
  max(memory_usage)        AS peak_mem,
  avg(read_rows)           AS avg_rows
FROM system.query_log
WHERE type = 'QueryFinish'
  AND log_comment LIKE 'cerb:%'
  AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY peak_mem DESC
LIMIT 20;
```

Condition-cache effectiveness (once `condition_cache` is enabled):

```sql
SELECT
  any(log_comment)                                           AS shape,
  sum(ProfileEvents['QueryConditionCacheHits'])              AS cache_hits,
  count()                                                    AS runs
FROM system.query_log
WHERE type = 'QueryFinish'
  AND log_comment LIKE 'cerb:%'
  AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY cache_hits DESC
LIMIT 20;
```

## Version safety

Nothing in this suite can break ClickHouse 24.8:

- `aggregation_in_order` and `log_comment` are 24.8-safe (long-standing
  result-equivalent / free-form knobs).
- `condition_cache` activates only on `>= 25.3`; below that it is a no-op.
- `ts_grid_range` and `ts_grid_resample` activate only on `>= 25.6` and are
  experimental (explicit-only).
- `ts_grid_changes` and `ts_grid_resets` activate only on `>= 25.9` and are
  experimental (explicit-only); below 25.9 they are absent from the resolved
  set.
- `columnar_result_decode` is client-side and version-agnostic (no server
  setting); it is explicit-only, so `auto` never engages it.
- Under `auto`, an unsupported feature is simply not enabled.

---

## Build contract

This section pins the exact public API, config names, consumption points,
and wiring the builders must implement verbatim. It is derived from the
`feat/chclient-query-instrumentation` foundation
(`internal/chclient/query_settings.go`, `internal/engine/plan_shape_id.go`,
`internal/engine/query_settings_rules.go`, `internal/config/config.go`,
`internal/preflight/preflight.go`, `internal/chclient/experimental.go`).

### Package `internal/chopt` — public API

```go
package chopt

// Version is a comparable major.minor ClickHouse version. Patch/build
// suffixes are dropped (feature availability is minor-grained). Parse mirrors
// internal/preflight parseCHVersion/leadingInt.
type Version struct {
  Major int
  Minor int
}

func ParseVersion(s string) (Version, bool)
func (v Version) AtLeast(min Version) bool
func (v Version) String() string // "Major.Minor"

// Stability classifies a feature.
type Stability int

const (
  Stable Stability = iota
  Experimental
)

// Feature is one registry entry. The per-feature allow_experimental_* setting
// is NOT a field: stamping it lives in the engine plan path (planHasTSGridNative
// -> chclient.WithTSGridSetting), so it fires on exactly the queries that use
// the native node rather than on every query the feature is enabled for.
type Feature struct {
  ID         string    // stable id, e.g. "aggregation_in_order"
  MinVersion Version   // minimum supporting CH version
  Stability  Stability // Stable | Experimental
  Doc        string    // one-line operator-facing description
}

// Mode governs how an unsupported explicit request is handled.
type Mode int

const (
  Enforcing  Mode = iota // FATAL (default)
  Permissive            // WARN + skip
)

func ParseMode(s string) (Mode, error) // "permissive"|"enforcing"

// Config is the resolver input (parsed from env by internal/config).
type Config struct {
  // Optimizations is the raw CERBERUS_CH_OPTIMIZATIONS value:
  // "auto" | "off" | comma-separated ids.
  Optimizations string
  // Mode is the parsed CERBERUS_CH_OPTIMIZATIONS_MODE.
  Mode Mode
  // LegacyTSGrid carries the deprecated CERBERUS_EXPERIMENTAL_TS_GRID_RANGE:
  // Set=false means unset (no effect); Set=true means the bool in Value applies.
  LegacyTSGrid LegacyFlag
}

// LegacyFlag models a tri-state legacy bool: unset vs explicitly true/false.
type LegacyFlag struct {
  Set   bool
  Value bool
}

// EnabledSet is the immutable resolved result. Consumers query it by id.
type EnabledSet struct { /* unexported */ }

func (s EnabledSet) Has(id string) bool
func (s EnabledSet) IDs() []string // sorted, for boot logging

// Registry returns the seeded feature registry (aggregation_in_order,
// condition_cache, ts_grid_range). Exposed so tests can enumerate it.
func Registry() []Feature

// Resolve runs ONCE at startup, after the version probe. It returns the
// immutable EnabledSet plus a slice of human-readable warnings (deprecation
// + permissive skips) to log at boot. A fatal condition (unknown id in any
// mode; unsupported explicit id under Enforcing) is returned as err -> the
// caller exits non-zero.
func Resolve(cfg Config, server Version) (set EnabledSet, warnings []string, err error)
```

Feature id constants (exported, so config/engine reference them without
stringly-typed drift):

```go
const (
  FeatureAggregationInOrder = "aggregation_in_order"
  FeatureConditionCache     = "condition_cache"
  FeatureTSGridRange        = "ts_grid_range"
)
```

Seeded `Registry()` entries (verbatim):

| ID                       | MinVersion   | Stability        |
| ------------------------ | ------------ | ---------------- |
| `aggregation_in_order`   | `{24, 8}`    | `Stable`         |
| `condition_cache`        | `{25, 3}`    | `Stable`         |
| `ts_grid_range`          | `{25, 6}`    | `Experimental`   |

### New config field names + env consts (`internal/config`)

New env consts (add to the const block at ~`internal/config/config.go:425`
alongside `envExperimentalTSGrid`, and append to `allEnvKeys`):

```go
envCHOptimizations     = "CERBERUS_CH_OPTIMIZATIONS"          // string, default "auto"
envCHOptimizationsMode = "CERBERUS_CH_OPTIMIZATIONS_MODE"     // string, default "enforcing"
envCHOptCorpusEnabled  = "CERBERUS_CH_OPT_CORPUS_ENABLED"     // bool,   default false
envCHOptCorpusInterval = "CERBERUS_CH_OPT_CORPUS_INTERVAL"    // dur,    default 60s
envCHOptCorpusSinkPath = "CERBERUS_CH_OPT_CORPUS_SINK_PATH"   // string, default ""
```

Defaults via `v.SetDefault(envCHOptimizations, "auto")` and
`v.SetDefault(envCHOptimizationsMode, "enforcing")` etc., in `newLoader`.
Reads use the existing idiom: `getString` for the two optimization knobs and
the sink path, `getBool` for the corpus-enable, `getDuration` for the
interval. The legacy tri-state uses `explicitlySet(v, envExperimentalTSGrid)`
together with `getBool` to populate `LegacyFlag{Set, Value}`.

New `Config` struct fields:

```go
// CHOptimizations is the raw CERBERUS_CH_OPTIMIZATIONS string ("auto" |
// "off" | comma list). Resolved at startup against the probed server
// version into the chopt.EnabledSet.
CHOptimizations string

// CHOptimizationsMode is the parsed enforcing/permissive mode.
CHOptimizationsMode chopt.Mode

// CHOptCorpus configures the async query_log performance-corpus reconciler.
CHOptCorpus CHOptCorpusConfig
```

```go
type CHOptCorpusConfig struct {
  Enabled  bool
  Interval time.Duration
  SinkPath string
}
```

The existing `Config.ExperimentalTSGridRange bool` field **stays** (its
consumers in `internal/api/prom`, `internal/promql`, `internal/preflight`,
`internal/chplan` keep reading it). It is **no longer** populated directly
from `flags.TSGridRange` at `config.go:729`; instead `FromEnv` resolves the
`EnabledSet` (using the probed version) and sets
`ExperimentalTSGridRange = set.Has(chopt.FeatureTSGridRange)`.

> Important: the version probe happens at connection init (`internal/chclient`),
> but `FromEnv` runs before the client connects. Resolution therefore happens
> in `cmd/cerberus/main.go` **after** the client is built and the version
> probed — not inside `FromEnv`. `FromEnv` carries the raw `CHOptimizations`
> string + parsed `Mode` + legacy `LegacyFlag`; `main.go` calls
> `chopt.Resolve(cfg, serverVersion)` once, logs the warnings, exits on err,
> then back-fills `cfg.ExperimentalTSGridRange` and builds `SettingsRules`
> from the resolved set before any head handler is constructed.

The legacy `LegacyFlag` is carried on `Config` (e.g.
`Config.LegacyTSGridFlag chopt.LegacyFlag`) so `main.go` can pass it into
`chopt.Config` at resolve time. `bootFlagsFromEnv` keeps reading
`envExperimentalTSGrid` to produce the tri-state, not a plain bool.

### `EnabledSet` consumption points

1. **`internal/engine.SettingsRules`** — gains a feature-gated form. Its
   `OptimizeAggregationInOrder bool` is driven solely by
   `set.Has(chopt.FeatureAggregationInOrder)` (there is no separate env flag;
   per-feature control is via the `CERBERUS_CH_OPTIMIZATIONS` list); add a
   `ConditionCache bool` driven by `set.Has(chopt.FeatureConditionCache)`,
   whose `apply` stamps `use_query_condition_cache=1` on predicate-stable
   read paths. `LogCommentShape` is unchanged (its own dark flag /
   corpus-driven). `settingsRules(cfg)` in `cmd/cerberus/main.go:378`
   becomes `settingsRules(cfg, set)` and reads the resolved set.

2. **`Config.ExperimentalTSGridRange`** — back-filled from
   `set.Has(chopt.FeatureTSGridRange)` (see above). Its existing consumers
   (`internal/api/prom` `Handler.ExperimentalTSGridRange`,
   `internal/promql.LowerOpts.ExperimentalTSGridRange`,
   `internal/preflight` `Requirements.NativeRateEnabled`,
   `internal/chplan/range_window_native.go`) are **unchanged** — they keep
   reading the bool, now a derived value.

3. **`cmd/cerberus/main.go`** — owns the one-shot resolve: build client ->
   probe `version()` -> `chopt.Resolve` -> log boot line + warnings (or
   `return err` to exit non-zero on fatal) -> back-fill
   `cfg.ExperimentalTSGridRange` -> `settingsRules(cfg, set)` for all three
   heads -> optionally start the corpus reconciler when
   `cfg.CHOptCorpus.Enabled`.

### condition_cache setting

```go
// internal/engine/query_settings_rules.go
const settingUseQueryConditionCache = "use_query_condition_cache" // value 1
const settingEnableAnalyzer = "enable_analyzer"                   // value 1
```

Stamped via `chclient.WithQuerySetting(ctx, settingUseQueryConditionCache, 1)`
only when `SettingsRules.ConditionCache` is true AND the read path is
predicate-stable. Because the condition cache is gated behind the analyzer,
cerberus co-stamps `enable_analyzer=1` alongside it (result-equivalent) so the
cache is honored even if an operator disabled the analyzer. No-op below 25.3
(the feature is simply absent from the resolved set there, so `ConditionCache`
is false).

### Reconciler package (`internal/optcorpus`)

Public surface the builders implement (shape, not prescriptive on internals):

```go
package optcorpus

// Record is one dispatched query's identity, registered when cerberus sends it.
type Record struct {
  QueryID  string   // CH query_id (<traceID>-<spanID>-<counter>), join key into query_log
  ShapeID  string   // cerb:<root>[;mod...] from engine.planShapeID
  Opts     []string // resolved enabled-opts that rode this query
  Language string   // "promql" | "logql" | "traceql"
}

// Row is the durable corpus tuple written to the sink (JSONL v1). Field
// shape is stable so a later CH-table sink is a column-for-column swap.
type Row struct {
  ShapeID             string
  Opts                []string
  Language            string
  NormalizedQueryHash uint64
  ReadRows            uint64
  ReadBytes           uint64
  QueryDurationMS     uint64
  MemoryUsage         uint64
  ProfileEvents       map[string]int64 // QueryConditionCacheHits, RowsReadByPrewhereReaders, ...
}

// Sink is the durable write target. JSONLSink(path) is the v1 implementation.
type Sink interface {
  Write(rows []Row) error
  Close() error
}

// QueryLogSource reads finished rows for a batch of query_ids. A fake
// implementation backs the unit tests; *chclient.Client backs production.
type QueryLogSource interface {
  FinishedByQueryID(ctx context.Context, ids []string) ([]Row, error)
}

// Reconciler holds the bounded ring of Records and reconciles them on an
// interval. Errors are logged, never fatal; Run returns on ctx cancel.
type Reconciler struct { /* bounded ring, source, sink, interval, logger */ }

func New(src QueryLogSource, sink Sink, opts Options) *Reconciler
func (r *Reconciler) Observe(rec Record) // ring-buffered, bounded, drops oldest
func (r *Reconciler) Run(ctx context.Context)
```

`Options` carries the ring capacity, the interval, and the logger. The
production wiring registers a `Record` at the engine dispatch seam (where
`query_id` and the shape-id are already computed) and runs `Run` from a
goroutine started in `cmd/cerberus/main.go` only when
`cfg.CHOptCorpus.Enabled`. chDB has no `system.query_log`, so the reconciler
is never started under the chDB build/test substrate.
