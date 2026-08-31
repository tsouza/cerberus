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
posture (`auto`) only ever enables features the connected server actually
supports.

Auto-eligibility is a separate axis from maturity. A feature carries a
`stability` class (operator-facing maturity) **and** an `autoSelect` flag
(whether `auto` picks it by version). The two are decoupled: the native
`timeSeries*ToGrid` aggregates are `experimental` in maturity yet
auto-enabled on capable servers, because they are validated result-correct
and run at flat memory — auto picks them once the server meets their floor
**and** the server permits the experimental setting they need (see
[Capability probe](#capability-probe-experimental-ts_grid-setting)).
The lone opt-in-only feature is `columnar_result_decode` (`autoSelect: no`),
a perf tradeoff that auto never selects.

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

- **`auto`** (default) — enable every **auto-select** feature (`autoSelect:
  yes`) whose minimum version is `<=` the connected server's version.
  Auto-eligibility is independent of maturity, so this includes the
  `experimental` native `timeSeries*ToGrid` aggregates on a capable server —
  provided that server also **permits the experimental setting** they require;
  a server that forbids it silently keeps the native family on the fan-out path
  (see [Capability probe](#capability-probe-experimental-ts_grid-setting)).
  The only feature `auto` never picks is `columnar_result_decode`
  (`autoSelect: no`, a perf tradeoff), which requires explicit listing.
  `auto` may appear **alongside** explicit ids, so
  `auto,columnar_result_decode` means "the auto-selected set **plus**
  `columnar_result_decode`" — the way to add the opt-in feature without giving
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

Resolution runs after a runtime version probe and produces an immutable
`EnabledSet`. It is the single source of truth every consumer reads from;
nothing downstream re-reads the raw env.

It runs at startup, and again every `5m` while the process serves (see
[Re-probe](#re-probe)), so the set in force always describes the server
cerberus is actually connected to.

| `CERBERUS_CH_OPTIMIZATIONS`   | Effect                                                                                                                                        |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `off`                         | Empty set.                                                                                                                                    |
| `auto`                        | Every `autoSelect: yes` feature with `minVersion <= server` (includes the experimental native aggregates; excludes `columnar_result_decode`). |
| explicit list                 | Per id: supported -> enable; unsupported -> `enforcing`: FATAL / `permissive`: WARN + skip. Unknown id -> FATAL (both modes).                 |

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
| id                               | minVersion | stability    | autoSelect |
| -------------------------------- | ---------- | ------------ | ---------- |
| `aggregation_in_order`           | 24.8       | stable       | yes        |
| `condition_cache`                | 25.3       | stable       | yes        |
| `ts_grid_range`                  | 25.9       | experimental | yes        |
| `ts_grid_resample`               | 25.9       | experimental | yes        |
| `columnar_result_decode`         | none       | experimental | no         |
| `ts_grid_changes`                | 25.9       | experimental | no         |
| `ts_grid_resets`                 | 25.9       | experimental | yes        |
| `ts_grid_deriv`                  | 25.9       | experimental | yes        |
| `ts_grid_predict_linear`         | 25.9       | experimental | yes        |
| `ts_grid_recollapse`             | 25.9       | experimental | yes        |
| `ts_grid_increase`               | 25.9       | experimental | yes        |
| `ts_grid_histogram`              | 25.9       | experimental | yes        |
| `quantile_prom_histogram`        | 25.10      | experimental | no         |
| `ts_grid_delta`                  | 25.9       | experimental | yes        |
| `laginframe_adjacency`           | none       | experimental | yes        |
| `fixed_accumulator_extrapolated` | none       | experimental | no         |
| `map_bucketed_serialization`     | 26.4       | experimental | no         |
<!-- END GENERATED: chopt-feature-table -->

The rich, hand-authored columns below stay OUTSIDE the generated block: they
carry operator judgement the registry cannot derive. The "experimental setting"
column is informational -- where a feature needs an `allow_experimental_*`
setting, that setting is co-stamped by the **engine plan path** (it inspects the
post-optimize plan and stamps the setting on exactly the queries that use the
native node), not carried as a registry field — so the co-stamp fires whether
the feature was reached via `auto` or by explicit listing. Two features are
`autoSelect: no`, opt-in only: `columnar_result_decode` (a perf tradeoff) and
`ts_grid_changes` (a correctness gap — the native builtin diverges from
reference Prometheus on NaN-adjacent windows, tracked as
[#1721](https://github.com/tsouza/cerberus/issues/1721)).

| id                           | experimental setting                                 | effect                                                                                                                                                                                                                                                                                                                                                                                                                              |
| ---------------------------- | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `aggregation_in_order`       | (none)                                               | stamps `optimize_aggregation_in_order=1` when the plan's Aggregate GROUP BY is a bare-column prefix of the scanned table's sorting key. Result-equivalent.                                                                                                                                                                                                                                                                          |
| `condition_cache`            | (none)                                               | stamps `use_query_condition_cache=1` (+`enable_analyzer=1`, analyzer-gated) on predicate-stable read paths. Result-equivalent (a cache).                                                                                                                                                                                                                                                                                            |
| `ts_grid_range`              | `allow_experimental_time_series_aggregate_functions` | opts eligible `rate(<counter>[<range>])` query_range shapes onto the native `timeSeriesRateToGrid` aggregate. Auto-enabled on server >= 25.9 (experimental maturity).                                                                                                                                                                                                                                                               |
| `ts_grid_resample`           | `allow_experimental_time_series_aggregate_functions` | opts the range-mode instant-vector staleness shape onto the native `timeSeriesResampleToGridWithStaleness` aggregate, retiring the argMax fan-out. Auto-enabled on server >= 25.9.                                                                                                                                                                                                                                                  |
| `columnar_result_decode`     | (none)                                               | client-side: decodes the `query_range` matrix shape via the ch-go columnar path (label map built once per run, not per row). No server setting, no version floor. Opt-in only (never auto).                                                                                                                                                                                                                                         |
| `ts_grid_changes`            | `allow_experimental_time_series_aggregate_functions` | opts eligible `changes(<v>[<range>])` query_range shapes onto the native `timeSeriesChangesToGrid` aggregate, retiring the `arrayPopBack`/`arrayPopFront` fan-out. Opt-in only (never auto) — the builtin diverges from reference Prometheus on NaN-adjacent windows, [#1721](https://github.com/tsouza/cerberus/issues/1721); server still needs >= 25.9.                                                                          |
| `ts_grid_resets`             | `allow_experimental_time_series_aggregate_functions` | opts eligible `resets(<counter>[<range>])` query_range shapes onto the native `timeSeriesResetsToGrid` aggregate, retiring the `arrayPopBack`/`arrayPopFront` fan-out. Auto-enabled on server >= 25.9.                                                                                                                                                                                                                              |
| `ts_grid_deriv`              | `allow_experimental_time_series_aggregate_functions` | opts eligible `deriv(<gauge>[<range>])` query_range shapes onto the native `timeSeriesDerivToGrid` aggregate (per-window least-squares slope), retiring the `simpleLinearRegression`/`arrayReduce` fan-out. Auto-enabled on server >= 25.9.                                                                                                                                                                                         |
| `ts_grid_predict_linear`     | `allow_experimental_time_series_aggregate_functions` | opts eligible `predict_linear(<gauge>[<range>], t)` query_range shapes (whole-second literal `t`) onto the native `timeSeriesPredictLinearToGrid` aggregate (per-window slope\*t + intercept forecast), retiring the `simpleLinearRegression`/`arrayReduce` fan-out. Auto-enabled on server >= 25.9.                                                                                                                                |
| `ts_grid_recollapse`         | `allow_experimental_time_series_aggregate_functions` | defers the OTel -> Prometheus label-shaping tower PAST an eligible `ts_grid_range` rate grid, splitting it into `timeSeriesRateToGridState` over the raw keys and `timeSeriesRateToGridMerge` over the shaped ones, so the reshape runs once per raw series instead of once per raw row. Narrows `ts_grid_range`. Auto-enabled on server >= 25.9.                                                                                   |
| `ts_grid_increase`           | `allow_experimental_time_series_aggregate_functions` | opts eligible `increase(<counter>[<range>])` query_range shapes onto the SAME native `timeSeriesRateToGrid` aggregate `ts_grid_range` uses, multiplied back by the window seconds at emit time (`increase()` is `extrapolatedRate()` without the final `/range` divide). Retires the `arrayJoin` sample-per-anchor fan-out. Auto-enabled on server >= 25.9.                                                                         |
| `quantile_prom_histogram`    | (none)                                               | opts the classic `histogram_quantile(phi, ...)` rank walk onto the native `quantilePrometheusHistogram(phi)(le, cum)` aggregate, retiring the `arrayCumSum`/`arrayFirstIndex`/interpolation chain. Opt-in only (never auto): faster at real-world series counts but costs ~3.3x memory at high cardinality ([#2790](https://github.com/tsouza/cerberus/issues/2790)), on top of the new 25.10 floor still lacking fielded evidence. |
| `map_bucketed_serialization` | (none)                                               | stamps `map_serialization_version='with_buckets'` on new logs/traces tables' `CREATE TABLE` SETTINGS tail only, never metrics. Fully transparent to reads (no chsql/chplan change — ClickHouse's Map reader already resolves the bucket for a subscript/`mapContains` read). Opt-in only (never auto): a full-map read measures ~2x slower, and only NEW tables get it — an existing table keeps `basic` until re-provisioned.      |

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
- **`ts_grid_range`** is `experimental` in maturity but **auto-enabled** on a
  capable server (`>= 25.9`): a prod-data validation proved the native path
  result-correct (more correct than the buggy fan-out for `rate`) at flat
  memory, so `auto` picks it by version. It is also reachable by the legacy
  alias (below). Its native aggregate requires the experimental setting to be
  co-stamped on exactly the queries that emit the native node — and the engine
  co-stamps off the post-optimize plan, so the setting fires whether the
  feature was reached via `auto` or explicit listing.
- **`ts_grid_resample`** is `experimental` in maturity but **auto-enabled** on
  a capable server (no legacy alias). It shares
  the `timeSeries*ToGrid` family floor (25.9) and the same experimental setting
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
- **`ts_grid_changes`** is `experimental` in maturity and, unlike the rest of
  the `timeSeries*ToGrid` family, **opt-in only** (`autoSelect: no`): the
  native builtin overcounts by exactly 1 whenever a window's
  chronologically-earliest in-window sample is NaN, and implements no
  NaN-both-sides carve-out at all, so it diverges from reference Prometheus's
  `changes()` on any NaN-adjacent window — confirmed against a real reference
  Prometheus on the `compatibility/prometheus` substrate. Tracked as
  [#1721](https://github.com/tsouza/cerberus/issues/1721), closed by making
  the feature permanently opt-in (`autoSelect: no`) rather than waiting on an
  upstream fix: the divergence lives inside the ClickHouse builtin, which
  cerberus cannot patch, so `CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes` must
  be listed explicitly and `auto` never selects it. Its floor is still
  **25.9**, NOT the 25.6 of rate/resample: `timeSeriesChangesToGrid`/
  `timeSeriesResetsToGrid` shipped a full quarter later (ClickHouse 25.9). A
  25.6 floor would mis-advertise support on 25.6-25.8 servers and 502 with
  ClickHouse error code 46, `Function with name timeSeriesChangesToGrid does
  not exist` — an absent `timeSeries*ToGrid` member is reported as an unknown
  FUNCTION, not as `UNKNOWN_AGGREGATE_FUNCTION` (verified against 25.7 and
  25.8 servers). It shares the family's experimental setting, co-stamped on
  exactly the queries that emit the native changes node when explicitly
  listed.
- **`ts_grid_resets`** is the sibling of `ts_grid_changes` (same PR upstream):
  experimental maturity, auto-enabled on a capable server, same **25.9** floor,
  same experimental setting.
  It opts eligible `resets(<counter>[<range>])` shapes onto the native
  `timeSeriesResetsToGrid` aggregate, retiring the per-window counter-reset
  fan-out.
- **`ts_grid_increase`** reuses `ts_grid_range`'s own `timeSeriesRateToGrid`
  aggregate — there is no dedicated `timeSeriesIncreaseToGrid` upstream — and
  multiplies the per-grid-point result back by the window seconds at emit
  time: Prometheus's `increase()` IS `extrapolatedRate()` without the final
  `/range` divide, so `rate * range` recovers the same undivided extrapolated
  increase the fan-out publishes. Shares `ts_grid_range`'s **25.9** floor and
  experimental setting; `autoSelect: yes` because the divide-then-multiply
  round trip introduces only a documented, measured 1-ULP float64 rounding
  divergence from the fan-out's direct multiply-then-sum (proven by a
  dual-emit chDB parity test), never a wrong answer the way `ts_grid_changes`'
  NaN-adjacent overcount is. It carries no `ts_grid_recollapse` counterpart in
  this cut: the label-shaping hoist is not wired for `increase()`.
- **`ts_grid_deriv`** and **`ts_grid_predict_linear`** are the LAST members of
  the `timeSeries*ToGrid` family to adopt the native path: experimental
  maturity, auto-enabled on a capable server, same experimental setting. Their
  aggregates (`timeSeriesDerivToGrid` / `timeSeriesPredictLinearToGrid`) shipped
  in ClickHouse **25.8** (PR #84328) — a quarter EARLIER than changes/resets —
  but the registry pins them to the family's shared **25.9** floor (the
  left-open-window fix, PR #86588) so one probed capability verdict governs
  every member. `deriv` is the per-window least-squares slope; `predict_linear`
  projects that fit `t` seconds past the anchor. Both retire the
  `simpleLinearRegression`/`arrayReduce` fan-out; both NULL a window with < 2
  samples (filtered to absent rows), mirroring the fan-out's drop-series
  semantics. `predict_linear` threads its horizon `t` as the aggregate's 5th
  parametric arg, so ONLY a single whole-second literal `t` is native-eligible —
  a computed horizon (`predict_linear(v[r], scalar(x))`) or a fractional `t`
  stays on the exact fan-out arithmetic. The native == fan-out numeric
  differential (a Float64 fit, so ULP-close rather than bit-identical) is proven
  on a `>= 25.9` server in the prod/e2e lane, not on the sub-25.9 chDB CI
  substrate, where the version gate keeps both on the fan-out; the always-on
  SQL-shape goldens (`native_deriv_range_step.txtar`,
  `native_predict_linear_range_step.txtar`) pin the native emit unconditionally.
- **`ts_grid_recollapse`** is a **narrowing of `ts_grid_range`**, not an
  independent path: it changes how an already-native rate grid is shaped, so
  with `ts_grid_range` off there is nothing for it to defer and cerberus never
  consults it. On an OTel schema the Attributes map a PromQL series is keyed by
  is not stored — it is COMPUTED, by a `mapSort`/`mapConcat`/`mapUpdate` tower
  that sanitises attribute keys, overlays resource attributes, and folds
  `ServiceName` in. Two-level emit evaluates that tower once per raw SAMPLE ROW,
  beneath the aggregate; this feature evaluates it once per raw SERIES, above the
  aggregate, by splitting the grid into three levels: `…ToGridState` grouped on
  the RAW columns the tower reads, `…ToGridMerge` grouped on the shaped key the
  tower computes, and an outer level that renames the shaped key to its output
  name and ARRAY JOINs the grid against its timestamp axis. On a reference
  deployment's heaviest APM range query — 35,094 raw series over a 300s window —
  this is **-28.5% CPU time (1.40x)**: 310.3 CPU-seconds down to 221.9. Wall
  time on the same paired runs moved 28.0s to 19.8s, but wall and CPU diverge
  with concurrency and shard count, so the resource saving is the claim rather
  than the latency.
  The `-State`/`-Merge` pair is load-bearing rather than incidental. Key
  sanitisation is **non-injective**: those 35,094 raw series shape onto 33,557
  output series, and the colliding groups are frequently time-disjoint with a
  splice gap smaller than the range window, so grid anchors near a splice have
  windows straddling both halves. Prometheus `rate()` is defined over the POOLED
  sample set at each anchor, which is what merging partial states computes;
  combining two FINISHED grids arithmetically is a different number, wrong at
  481 of 93,757 points on that query and NULL where one half holds fewer than
  two samples. No other member of the family is eligible today, so the rest pass
  an empty re-collapse and emit the byte-identical two-level shape.
  The **25.9** floor is INHERITED from `ts_grid_range` rather than
  independently derived: with `ts_grid_range` off there is no native node to
  defer anything past, so that feature's floor is the effective one and nothing
  about the re-collapse raises it. Merge exactness is not the binding
  constraint. The exactness probe compares one pooled pass against a merge of
  two per-group partial states, over samples split into two time-disjoint
  halves whose splice gap is smaller than the range window, so the grid anchors
  near the splice straddle both halves:

  ```sql
  -- pooled
  SELECT timeSeriesRateToGrid(start, end, 60, 300)(ts, val) FROM src
  -- merged
  SELECT timeSeriesRateToGridMerge(start, end, 60, 300)(st) FROM (
    SELECT timeSeriesRateToGridState(start, end, 60, 300)(ts, val) AS st
    FROM src GROUP BY raw)
  ```

  Executed against 25.8.28.1, 25.9.7.56, 26.1.12.23, 26.2.19.43, 26.3.17.56,
  26.4.5.143 and 26.5.6.64 (no 26.0 image is pullable, so that version is
  bracketed by its neighbours) in each of three data regimes — time-disjoint,
  interleaved, and counter-reset-straddling — the merged grid equals the pooled
  grid in all 21 cells. The reset-straddling regime yields a grid that DIFFERS
  from the other two, which is what shows the reset correction is applied
  through the merge rather than skipped. The floor is therefore the same one
  every other family member pins, so one probed capability verdict still
  governs the whole set.
- **`ts_grid_histogram`** moves the classic-histogram `rate()` window fold
  behind `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))` from
  an array expression to an aggregate. The fold it replaces walks the union
  bucket bounds with `arrayMap(u -> …, U)` over a body that READS the group's
  bounds / counts `groupArray`s, and ClickHouse materialises a lambda's
  captured columns once per outer-array element ([ClickHouse
  #54967](https://github.com/ClickHouse/ClickHouse/issues/54967)), so the fold
  builds one copy of the whole per-series bucket matrix per rung. Aggregate
  functions consume rows through `addBatch` and never construct that replica,
  so the same arithmetic expressed as `timeSeriesRateToGrid` over the UNNESTED
  ladder — one row per `(series, le)` carrying that rung's cumulative counter,
  which is exactly what reference Prometheus models `<name>_bucket{le="X"}` as
  — removes the replication. Measured against a real ClickHouse 26.6 at
  realistic scale, same rows read (~88-89k), 121 anchors, 5m window: the array
  fold takes 4,123 ms / 3.411 GB peak / 51.5 CPU-s, the native aggregate
  148 ms / 0.130 GB peak / 0.4 CPU-s — **28x faster, 26x less memory, 129x
  less CPU**.
  Two details carry the semantics the fan-out owns. First, the shape reads
  `timeSeriesResetsToGrid` alongside the rate purely as a per-grid-point
  PRESENCE signal: it is NULL for a window holding zero samples, which is how
  a rung the anchor's window never carried is kept OFF the ladder rather than
  landing on it with a fabricated `0` that the monotonic-envelope repair would
  lift into a moved interpolation bound. Second, the `+Inf` rung is reported by
  every stored row, so its own sample set IS the series' whole in-window sample
  set and its NULL is exactly the per-series two-sample floor the fan-out
  spells as `RangeBucketFanout.MinSamples`.
  DELTA aggregation temporality stays on the fan-out — the native aggregate
  reads a cumulative counter and has no delta branch — so a schema declaring
  the column emits a two-arm `UNION ALL` whose arms read complementary row
  sets, the same split the scalar `ts_grid_range` path already makes.
  The **25.9** floor is INHERITED from the family rather than independently
  derived: the shape rides the same `timeSeriesRateToGrid` `ts_grid_range`
  pins, so it inherits that feature's binding constraint (the left-open /
  right-closed membership window, upstream PR #86588). The presence aggregate
  is a `ts_grid_resets` sibling from PR #86010, released in the same 25.9, so
  the floor is unchanged either way.
- **`quantile_prom_histogram`** replaces the classic-histogram
  `histogram_quantile(phi, <classic-selector>)` rank walk — steps 3-5 of the
  hand-rolled emitter (the observation total, the `arrayFirstIndex` rank-walk
  index, and the linear interpolation with all its edge-case branches) — with
  one `quantilePrometheusHistogram(phi)(le, cum)` call over an `ARRAY JOIN`
  unnest of the row's (coalesced ExplicitBounds, coalesced cumulative ladder)
  pair. It is a DIFFERENT node from `ts_grid_histogram` above: that feature
  picks how the range-mode per-series `rate` WINDOW stage is computed (the
  input feeding a `HistogramQuantile` node); this one picks how the quantile
  node ITSELF is computed, uniformly, for every shape that node handles
  (instant bare selector, instant cross-series merge, range-mode bare and
  aggregated, and the float-array variant) — unlike every other feature in
  this registry there is no per-shape fallback, because the native aggregate
  reproduces reference Prometheus's `bucketQuantile` (including its edge
  cases) for any row this node's IR contract accepts.
  Floor **25.10**: confirmed via a direct `system.functions` probe (the
  aggregate is undocumented as of this writing) — it is NOT part of the
  `timeSeries*ToGrid` family and shares none of that family's 25.9 floor or
  experimental setting. The emission still has to work around two input-
  contract quirks the probe also surfaced: the aggregate answers `nan`
  whenever no row carries `le = +Inf` (an unconditional terminal pair is
  appended — the genuine overflow rung when the row has one, a synthetic
  tie-cum entry when it does not), and its parametric phi argument must be a
  compile-time-constant value in `[0, 1]` — passing it a NaN or out-of-range
  value throws `PARAMETER_OUT_OF_BOUND` and fails the whole query, since an
  aggregate's parametric argument is evaluated once regardless of which
  branch of an enclosing scalar `if()` would select its result, so the
  argument is clamped unconditionally and reference Prometheus's `-inf` /
  `inf` / `nan` contract is answered in an outer branch that never lets an
  out-of-domain phi reach the aggregate. A real-CH differential
  (`internal/chsql`'s `TestHistogramQuantile_RankWalkNative_DifferentialRealCH`)
  confirmed exact agreement with the legacy walk across representative
  bucket layouts (a normal crossing, a duplicate-bound layout, the
  equal-length/no-overflow-rung shape, an empty histogram, a first-bucket
  non-positive upper bound, and an all-zero-count histogram) and the full
  phi domain (below range, the two saturating edges, interior crossings,
  above range, and a runtime NaN phi). `AutoSelect` is `false`: correctness
  parity is proven, but a real-scale measurement (25.10.7.6, a real OTel
  classic-histogram export) found a genuine performance TRADEOFF, not just
  an unproven new floor — the emission's `ARRAY JOIN` multiplies row count
  by the bucket-ladder length before `GROUP BY` collapses it back down,
  which the legacy walk never does. At real-world dashboard scale (3,677
  series) the native path was ~2x faster at equal memory; at high series
  cardinality (73,540 series, ~880k post-unnest rows) wall time stayed
  roughly even but memory grew ~3.3x. See
  [#2790](https://github.com/tsouza/cerberus/issues/2790) for the full
  numbers and the mitigation options left for future investigation. The
  feature is opt-in only (`CERBERUS_CH_OPTIMIZATIONS=quantile_prom_histogram`)
  pending that investigation and broader fielded evidence on the very new
  25.10 floor.
- **`map_bucketed_serialization`** ([#2774](https://github.com/tsouza/cerberus/issues/2774))
  is a SCHEMA feature, not a query-lowering one — it is the only registry
  entry that changes `internal/schema/ddl`'s `CREATE TABLE` output rather
  than the SQL an engine plan emits. It stamps ClickHouse's bucketed Map
  serialization (`map_serialization_version='with_buckets'`) on the logs
  table and the traces spans table only, distributing each Attributes-shaped
  Map column's keys across (by default) 32 hash-selected buckets so a
  single-key read (a PromQL/LogQL label matcher, a TraceQL attribute
  predicate, a tempo `tag_values` scan) decompresses only that key's bucket
  instead of the whole map. **Excludes the five metrics tables**: a metrics
  series' identity IS its whole Attributes map, so every metrics read is
  already a full-map read the family's own ~2x whole-map-read penalty would
  only tax, never benefit. **Read side needs no change at all** — bucket
  selection is internal to ClickHouse's Map column reader; `m['key']` and
  `mapContains(m, 'key')` already emit the identical SQL shape whether or
  not `with_buckets` is active, confirmed against the ClickHouse docs (not
  merely the announcement blog). **Scope is new tables only**: the setting
  lands in the `CREATE TABLE` SETTINGS tail `internal/schema/ddl`'s
  auto-create renders, so an existing deployed table is completely
  unaffected (byte-identical DDL) until it is re-provisioned or an operator
  runs their own `ALTER TABLE ... MODIFY SETTING` — no ALTER-driving
  migration tool ships in this feature. **Version floor is 26.4, not the
  26.3 the upstream backport (v26.3.2.3-lts) technically landed in**: this
  registry compares `(major, minor)` only, and 26.3.0/26.3.1 lack the
  feature, so a "26.3" floor would wrongly claim they have it. **Key order
  is only preserved from ClickHouse 26.8** (a `bucket_indexes` metadata
  stream); cerberus is safe below that solely because every stream-identity
  read already goes through `mapSort` canonicalization
  (`internal/chplan/canonical_attributes.go`) rather than trusting raw map
  order. `AutoSelect` is `false`: the same source that measures the
  single-key win also measures full-map reads (stream-label rendering, Loki
  `series`/`labels`/`detected_labels`/`index_stats`/`index_volume`, tempo
  `mapConcat`) roughly 2x SLOWER, so enabling it is a deliberate
  single-key-vs-whole-map tradeoff an operator opts into, not a version-gated
  pure win `auto` can assume.
- **Window-slide anchor injection** is a second ClickHouse-native lowering of
  the per-series window stage under
  `histogram_quantile(phi, <agg> by(le) (sum_over_time(<bucket>[range])))` in
  range mode, alongside `ts_grid_histogram`'s `rate` ladder. It is **not** a
  `chopt` registry entry and carries no row in the generated table above:
  unlike the rest of this family it needs no version-gated ClickHouse
  function, only plain window functions with a numeric `RANGE` frame that
  every floor cerberus targets already has, so it is
  `chopt.AlwaysAvailable` and wired unconditionally rather than resolved
  against the server version. The mechanism injects one sentinel row per grid
  anchor via `UNION ALL` and folds each series' contributing rows across it
  with `sumForEach(BucketCounts) OVER (RANGE BETWEEN <lookback> PRECEDING AND
  CURRENT ROW)`, so a single windowed aggregate produces the whole grid's
  ladder without materialising a per-anchor row replica the way the
  fan-out's array expression does.
  This path is **not** a blanket replacement for the fan-out: it is eligible
  only for the SUM-fold `sum_over_time` (`rate` keeps its own `rate()`
  extrapolation and reset-repair semantics on `ts_grid_histogram` instead),
  and only once the window's Lookback/Step ratio clears a threshold of 10 —
  below that, the extra `UNION ALL` and window-frame machinery are not worth
  their own correctness surface over the existing fan-out. The measured
  speedup **tracks that ratio directly and is not a flat multiplier**:
  a 5-minute window at a 1-minute step (ratio 5, the modal Grafana panel
  shape) measured only 1.12x — explicitly below the eligibility threshold, so
  that shape stays on the fan-out — while a 5-minute window at a 30-second
  step (ratio 10, the threshold) measured 1.70x, a 5-minute window at a
  15-second step (ratio 20) measured 2.65x, and a 30-minute window at a
  15-second step (ratio 120) measured 10-14x. A query whose window shape sits
  below the ratio-10 threshold, or whose fold is anything other than
  `sum_over_time` (`increase`, `delta`, `irate`, `idelta`), continues to lower
  through the existing array-expression fan-out unchanged.

## Runtime version probe

At connection init the client issues `SELECT version()` once, parses the
result to a comparable `major.minor` struct, and exposes a
`serverAtLeast(major, minor)` predicate. Resolution consumes this probe to
decide which registry features the server supports.

Patch and build suffixes (`25.8.2.1`, `25.8.2.1-lts`) are dropped: feature
availability lands at minor-version granularity, so the comparison is over
`(major, minor)` only. This mirrors the existing preflight version parse.

The probe repeats on the [re-probe](#re-probe) cadence, so a rolling ClickHouse
upgrade that crosses a feature floor is picked up by a running cerberus without
a restart.

## Capability probe (experimental ts_grid setting)

The native `timeSeries*ToGrid` features (`ts_grid_range`, `ts_grid_increase`,
`ts_grid_resample`, `ts_grid_changes`, `ts_grid_resets`, `ts_grid_deriv`,
`ts_grid_predict_linear`) need the server to run with
`allow_experimental_time_series_aggregate_functions=1`, which cerberus
co-stamps on exactly the queries that emit the native node. A server can be
**new enough** for the version floor yet still **forbid** that setting — a
hardened profile that pins or constrains it, or a readonly user. Auto-selecting
the native node there would only earn a `SETTING_CONSTRAINT_VIOLATION` (or
`READONLY`) rejection at query time, turning a deployment that worked on the
fan-out path into a 5xx.

So auto-selection of the native family is gated on **two** axes, not just the
version floor: alongside `SELECT version()`, cerberus runs a cheap
**capability canary** that stamps the experimental setting on a trivial query
over the always-present `default` database (independent of whether the
configured database exists yet). The verdict is tri-state:

- **available** — the server accepted the setting; the native family resolves
  per its version floor.
- **forbidden** — the server answered with a typed rejection (constrained /
  readonly profile); a *definitive* "no". The native family is **dropped to the
  fan-out path**.
- **unreachable** — the canary got no server verdict (a transport failure); an
  *inconclusive* result. Native stays off until a later re-probe gets a verdict,
  matching the version probe's connectivity fallback.

A verdict is therefore either **definitive** (`available` permits, `forbidden`
refuses) or **inconclusive** (`unreachable`, or `unknown` when the probe never
ran). How a non-`available` verdict is handled depends on both the verdict class
and how the feature was selected:

- under **`auto`** — the native features are **silently dropped** for *any*
  non-`available` verdict and a boot `WARN` is logged (`ch_opt "ts_grid_range"
  disabled: server forbids allow_experimental_time_series_aggregate_functions;
  falling back to fan-out`, or `... probe was inconclusive (unreachable);
  falling back to fan-out`). The deployment serves the fan-out path
  successfully.
- under an **explicit list** (or the legacy alias force-enable) — the handling
  splits on the verdict class:
  - **forbidden** is **FATAL under `enforcing`** (the operator required a
    feature the reachable server definitively will not run) and a `WARN` + skip
    under `permissive` — identical to listing a feature the server is too old
    for.
  - **inconclusive** (`unreachable` / `unknown`) is **never fatal** — under
    `enforcing` *and* `permissive` it degrades to the fan-out path with a
    `WARN`, mirroring the version probe's connectivity fallback. A probe that
    could not reach a verdict must not crash a deployment that may well be
    capable; the operator's "I require this" contract only fails loudly against
    a *definitive* refusal. (The version floor itself stays definitive: an
    explicitly-listed feature on a too-old server is still FATAL under
    `enforcing`.)

The canary rides with the version probe on every [re-probe](#re-probe), so a
profile change that later permits the setting is picked up without a restart.

**Escape hatch.** To run a forbidden server without any boot warnings, pin an
explicit `CERBERUS_CH_OPTIMIZATIONS` list that omits the `ts_grid_*` ids (e.g.
`aggregation_in_order,condition_cache`), or set `CERBERUS_CH_OPTIMIZATIONS=off`.
Conversely, permitting the setting in the ClickHouse profile (or using a
non-readonly user) lets `auto` pick the native family back up on the next
re-probe.

## Re-probe

The resolved set describes a server, and the server changes: a rolling
ClickHouse upgrade crosses a feature floor, a profile is relaxed, or a pod that
booted while ClickHouse was down pinned itself to the supported floor. So the
resolution is re-run every **5 minutes** for the life of the process, and a
changed answer is swapped into the query path in place.

Each pass repeats exactly the boot resolution — probe `version()`, run the
capability canary, resolve the *same* configured selection against both — so a
running process can never reach a posture boot could not have produced. The
selection itself is fixed for the life of the process, which is what makes the
loop safe to run unattended: a re-resolve cannot introduce an unknown feature id
or an unsupported explicit id, because those are config faults and config does
not change under a running process. Consequently a re-probe never exits the
process; a failed probe or resolve is logged and the set already in force is
kept.

Only a genuine transition is acted on. A pass whose resolved set and server
version both match the current ones does nothing at all, so the steady state is
silent and the log carries one line per real capability change:

```text
level=INFO msg="clickhouse optimizations re-resolved" server_version=25.9
  previous_server_version=24.8 enabled=aggregation_in_order,condition_cache,ts_grid_range
  previous_enabled=aggregation_in_order
```

What a transition swaps:

| Consumer                      | Effect of a re-resolved set                                                                                                                       |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| PromQL range lowering         | The native `timeSeries*ToGrid` strategy table is replaced, so subsequent `query_range` requests lower to the native shape (or back to fan-out).   |
| Engine per-query settings     | The `aggregation_in_order` / `condition_cache` rules are replaced, so subsequent queries stamp the settings the current server supports.          |
| `/info`                       | `clickhouse.serverVersion`, `optimizations.resolvedAgainstVersion`, and `optimizations.enabled` report the set in force, not the one booted with. |

`columnar_result_decode` is deliberately **not** in that list: it is
`AlwaysAvailable` and opt-in only, so no server upgrade can change its verdict
and the decode stays wired at boot.

The swap is per-consumer and lock-free — each consumer publishes its derived
value through one atomic pointer, so an in-flight request always reads a whole
strategy table or rule set, never a half-replaced one. A request that straddles
a swap may lower with one posture and execute with the other; both are valid
postures for the same server, because a dropped capability only ever disables a
native path and a gained one only ever enables it.

## Legacy alias: `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`

The legacy boolean `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` keeps working and
is **re-routed through the resolver** rather than read directly by its
downstream consumers. It maps onto the `ts_grid_range` registry feature:

The legacy alias only takes effect under the **default `auto`** selection;
any explicit `CERBERUS_CH_OPTIMIZATIONS` choice (a feature list **or** the
`off` kill-switch) overrides it.

- **explicitly `true`** (under `auto`) — force-enable `ts_grid_range` (as if
  it were listed), still subject to version gating and mode. On a `>= 25.9`
  server `auto` already enables it, so this is now mostly redundant.
- **explicitly `false`** (under `auto`) — force-disable `ts_grid_range`, even
  though `auto` now selects it on a capable server. This is the operator's
  escape hatch back to the fan-out rate path.
- **unset** — no effect. The framework resolves normally; under `auto`,
  `ts_grid_range` is enabled on a `>= 25.9` server (auto-selected by version,
  not by this flag).
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
- `ts_grid_range` and `ts_grid_resample` activate only on `>= 25.9`
  (experimental maturity, auto-enabled there); below 25.9 they are absent from
  the resolved set.
- `ts_grid_resets` activates only on `>= 25.9` (experimental maturity,
  auto-enabled there); below 25.9 it is absent from the resolved set.
- `ts_grid_changes` also needs `>= 25.9`, but unlike its sibling `ts_grid_resets`
  it is opt-in only (`autoSelect: no`, [#1721](https://github.com/tsouza/cerberus/issues/1721)),
  so `auto` never enables it regardless of server version; it is reachable
  only via an explicit `CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes` listing on
  a `>= 25.9` server.
- `columnar_result_decode` is client-side and version-agnostic (no server
  setting); it is opt-in only, so `auto` never engages it.
- Under `auto`, an unsupported feature is simply not enabled, so a deployment
  on ClickHouse 24.8 sees identical behaviour regardless of this change.
