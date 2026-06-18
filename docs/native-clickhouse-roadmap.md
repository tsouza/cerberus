# Native ClickHouse roadmap — shipping cerberus's heavy lowerings as `timeSeries*ToGrid` aggregates

Status: research synthesis + ranked roadmap. The first weapon
(`timeSeriesIncreaseToGrid`) is staged ready-to-apply under
[`docs/clickhouse-contrib/timeSeriesIncreaseToGrid/`](./clickhouse-contrib/timeSeriesIncreaseToGrid/).

CI-UNVALIDATED: no C++ in this roadmap or the staged contrib was compiled in
this environment. Everything is source-grounded and fidelity-reviewed against
ClickHouse `master` as of 2026-06-18 (see the cited file:line refs), then
staged inside cerberus for a maintainer to apply to a ClickHouse fork.

## Thesis

Cerberus's most expensive ClickHouse SQL lowerings are per-anchor array
fan-outs: every sample row is exploded to `(lookback / step)` anchor copies and
re-grouped, inflating scanned rows ~60x on wide `[range]/step` ratios (the
RANK-1 offender, `internal/chsql/range_window.go`). ClickHouse 25.6 shipped a
family of parametric aggregates — `timeSeries*ToGrid` — that compute a whole
PromQL grid in ONE columnar pass over each series, never materialising the
`O(grid_points x window)` sample matrix. Cerberus already adopted exactly one
member (`timeSeriesRateToGrid`, `internal/chsql/range_window_native.go`) and
proved the win.

The strategy is therefore not "invent a new engine" — it is **finish the
family**: ship cerberus's heavy lowerings upstream as new members of the
`timeSeries*ToGrid` aggregate family, reusing the proven
`AggregateFunctionTimeseriesBase` template, then wire each into cerberus's
existing single extension point (`nativeTSGridFn`) once differentially proven
equivalent.

"Drop SQL entirely" has exactly ONE sane version — **native columnar result
decode** over the protocol cerberus already speaks (clickhouse-go/v2 Native).
The rest of the "bypass SQL" space (submit a `QueryPlan` packet, embed chDB,
inject custom operators) ranges from prototype-worthy to fantasy. The verdict
is in the [Bypass-SQL verdict](#bypass-sql-verdict) section.

## Why the family template is the right vehicle

All `timeSeries*ToGrid` functions inherit one base,
`src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesBase.h`, whose
state is a SPARSE bucket map `UnorderedMapWithMemoryTracking<size_t, Bucket>`
keyed by grid-bucket index. A new function is a `Traits` struct (Bucket type +
`getName()` + per-bucket math) plus one `factory.registerFunction(...)` line in
`AggregateFunctionTimeseriesHelpers.cpp`. The base already provides the
overflow-safe `bucketIndexForTimestamp` / `isSampleOutOfGrid` /
`isSampleOutOfWindow` (Int128) guards and the 16M-bucket cap, so a derived
function inherits the hardening for free. Every new aggregate ALSO gains
`-State`/`-Merge`/`-MergeState`/`-Array`/`-ForEach`/`-Resample` for free from
the combinator factory — which is the genuine downsampling/rollup story
cerberus's lowered SQL fundamentally cannot express.

## Ranked candidate table

Boldness: **code-now** (incremental, math already in-tree) / **ambitious**
(new but well-scoped Traits) / **moonshot** (research bet).

| Rank | Function                                                                         | PromQL op                                        | Cerberus SQL it replaces                                                           | Why faster                                                               | CH impl (template)                                                                         | Academic basis                                                | Effort    | Boldness  |
|------|----------------------------------------------------------------------------------|--------------------------------------------------|------------------------------------------------------------------------------------|--------------------------------------------------------------------------|--------------------------------------------------------------------------------------------|---------------------------------------------------------------|-----------|-----------|
| 1    | `timeSeriesIncreaseToGrid`                                                       | `increase()`                                     | RANK-1 rate/increase/delta fan-out `range_window.go` ~590-994                      | One columnar pass; no `O(grid x window)` matrix; ~10-60x fewer rows      | Reuse `ExtrapolatedValue` (is_rate path) minus the divide-by-window                        | Prometheus `extrapolatedRate`; `increase=rate*range` identity | Small     | code-now  |
| 2    | `timeSeriesReduceOverTimeToGrid` (family)                                        | `*_over_time` (avg/min/max/sum/count/stddev/...) | `*_over_time` branches of `range_window.go` + `promql/range_fns.go`                | O(1)-state streaming accumulators; Welford moments are LLVM-JIT-able     | New Traits over base; sparse O(1) bucket (model `ToGridSparse`)                            | Welford 1962; Prometheus `funcAvgOverTime`                    | Medium    | ambitious |
| 3    | adopt `timeSeriesResampleToGridWithStaleness` **(SHIPPED — `ts_grid_resample`)** | instant-vector selection / staleness             | RANK-3 `range_lwr.go` `emitRangeLWR` staleness fan-out                             | Already-shipped native; retires the argMax fan-out outright              | DONE — `chplan.RangeWindowResample` + `chsql.emitRangeWindowResample`, boot-wired strategy | Prometheus 5m lookback staleness                              | Tiny      | code-now  |
| 4    | `timeSeriesHoltWintersToGrid`                                                    | `holt_winters` / `double_exponential_smoothing`  | `range_fns.go` `lowerHoltWinters` arrayMap recurrence                              | Removes per-anchor window-array materialisation; sort-then-fold in place | New Traits; sample-buffer bucket (copy `LinearRegression` sort-by-ts)                      | Holt 1957 / Winters 1960                                      | Small-Med | ambitious |
| 5    | `timeSeriesHistogramQuantileToGrid`                                              | `histogram_quantile` (classic + OTel exp)        | RANK-2/4/5 `histogram_quantile.go` (~48KB) + `histogram_quantile_native.go` (325L) | Replaces a 7-deep `pow` walk + double fan-out with one pass              | New sibling base (Bucket = scale/zeroCount/pos+neg arrays)                                 | OTel base-2 exp-histogram perfect-subsetting                  | Large     | ambitious |
| 6    | `timeSeriesQuantileSketchToGrid`                                                 | `quantile_over_time` (mergeable, exact-ish)      | `range_fns.go` Prom-exact rank interpolation (replacing CH t-digest)               | Mergeable sketch state; relative-error bound; downsample-safe `-State`   | New Traits carrying DDSketch/KLL state per bucket                                          | DDSketch (Masson 2019); KLL (Karnin 2016)                     | Large     | moonshot  |
| 7    | `timeSeriesXRateToGrid` / `XIncrease`                                            | MetricsQL `xrate`/`xincrease`                    | nothing today (new accuracy knob)                                                  | Leaner inner loop (no boundary clamp); first-class pre-window sample     | `ExtrapolatedValue` variant, extrapolation factor forced to 1.0                            | VictoriaMetrics MetricsQL                                     | Medium    | ambitious |
| 8    | `timeSeriesJoinToGrid` (asof-on-grid)                                            | vector-vector binary ops                         | RANK-6 `vector_join.go` (805L) INNER JOIN of per-series argMax                     | One grid-aligned pass vs JOIN + per-anchor argMax + cardinality guards   | New table-function or two-arg grid aggregate (does NOT fit base cleanly)                   | QuestDB ASOF/WINDOW JOIN vectorization                        | Large     | moonshot  |
| 9    | `timeSeriesGorillaScanToGrid` (+ `-State` rollups)                               | instant-vector + ingest-time rollups             | RANK-3 `range_lwr.go` AND the absent downsampling layer                            | Gorilla-packed state; `-MergeState` raw->1m->5m->1h rollups              | Hardest fit; variable-length bit-packed bucket + bespoke serialize                         | Gorilla (Pelkonen 2015); Monarch (Adams 2020)                 | Weeks     | moonshot  |

## The single best first PR: `timeSeriesIncreaseToGrid`

`increase()` is the single most-used counter op with **no** native function,
and it is the cleanest possible win because the math already exists in-tree.

`src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesExtrapolatedValue.h`
already computes both rate and delta via an `is_rate` flag. Crucially, `is_rate`
is OVERLOADED — it gates three things in `fillResultValue()`:

1. counter-reset accumulation (`adjust_to_resets = is_rate`, in
   `doInsertResultInto`);
2. the rising-counter zero-extrapolation clamp
   (`if (is_rate && value_difference > 0 && first_value >= 0)`);
3. the final divide-by-window
   (`if constexpr (is_rate) factor = factor * timestamp_scale_multiplier / Base::window`).

`increase()` is `rate()` WITHOUT step (3): it shares the reset correction and the
zero-clamp, and emits the un-divided total. It is therefore NOT the existing
`delta` path (delta skips reset correction AND the zero-clamp), and cannot be
produced by merely toggling the two-state `is_rate` flag. The lazy-correct fix
is to introduce a third compile-time mode.

### Implementation plan (laziest correct)

Add a `is_increase` static constexpr to
`AggregateFunctionTimeseriesExtrapolatedValueTraits` (default `false`).
`increase` registers with `is_rate = true` (so reset + zero-clamp + the rate
branch of `getName()` all stay) **and** `is_increase = true`, then guard ONLY
the divide-by-window with `if constexpr (is_rate && !is_increase)`. `getName()`
returns `timeSeriesIncreaseToGrid` when `is_increase`. One registration line in
`AggregateFunctionTimeseriesHelpers.cpp` mirroring the `timeSeriesRateToGrid`
entry. One functional test asserting `increase == rate * window` on a known
counter series. No new abstractions, no base-class changes.

Differential invariant (verified from the shipped golden in
`tests/queries/0_stateless/03254_timeseries_functions.reference`): for window
`w`, `increase[t] == rate[t] * w` exactly, because increase reuses rate's
reset/zero-clamp path and only drops the `/ Base::window` divide. (Note this is
NOT `== delta[t]`: delta lacks reset correction, e.g. at `ts=1734955680`,
`rate*300 = 8.1138` while `delta = 8.3491`.)

Cerberus-side follow-up: add `"increase": "timeSeriesIncreaseToGrid"` to
`nativeTSGridFn` (`internal/chsql/range_window_native.go:35`) and widen the
`lower.go` eligibility gate (`internal/promql/lower.go:1924-1926`) to admit
`rw.Func == "increase"` once the differential sweep is green. The experimental
setting is stamped by the engine (`chopt.FeatureTSGridRange` ->
`allow_experimental_time_series_aggregate_functions=1`), not emitted as SQL.

Full instructions, the C++ snippet/patch, and the test are staged under
[`docs/clickhouse-contrib/timeSeriesIncreaseToGrid/`](./clickhouse-contrib/timeSeriesIncreaseToGrid/).

## Bypass-SQL verdict

The radical "stop emitting SQL" question splits into four sub-options. Honest
verdict per option:

| Option | Idea                                                                       | Verdict       | Rationale                                                                                                                                                                                                                                          |
|--------|----------------------------------------------------------------------------|---------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 5A     | Native columnar result handoff (Native-format blocks, external-table push) | **PURSUE**    | Cerberus already speaks the Native protocol (clickhouse-go/v2). Decode result blocks column-at-a-time (no row scanning); push client-computed sets as external temp tables JOINed server-side in one round-trip. Real, incremental, no fork.       |
| 5B     | Ship new native aggregates (the `timeSeries*ToGrid` family)                | **PURSUE**    | The whole thesis of this roadmap. Proven seam (`nativeTSGridFn`), proven template, upstream-shippable. Highest leverage, lowest risk.                                                                                                              |
| 5C     | Embed chDB (in-process ClickHouse) for client-side post-processing         | **PROTOTYPE** | chDB lets cerberus run ClickHouse SQL in-process over result blocks — useful for the differential harness (`range_window_native_chdb_test.go` already exists) and for fusing small client-side steps. Prototype-only: not a production query path. |
| 5D     | Lower cerberus's `chplan` IR straight to a ClickHouse `QueryPlan` packet   | **FANTASY**   | The `QueryPlan` packet (`process_query_plan_packet`) exists but is an internal, version-coupled C++ ABI with no stable wire contract. Coupling cerberus to it is a perpetual version-skew chase. Park it.                                          |

## Adversarial honesty

- **"60x" is a scan-row figure, not a wall-clock promise.** The native pass
  eliminates row inflation, but the real win depends on series cardinality and
  window/step ratio. Every adoption MUST clear cerberus's chDB differential
  harness AND a real-cardinality benchmark before flipping `nativeTSGridFn`.
- **Pre-window-sample parity is a known bug class.** Prometheus/MetricsQL
  rate/increase need the last sample strictly before each window's left edge.
  Whether `timeSeriesRateToGrid` (and thus `IncreaseToGrid`) includes it must be
  confirmed by a source read of `isSampleOutOfGrid` before claiming parity with
  cerberus's `RangeLWR` left-inclusive `(t-Offset-Lookback, t-Offset]` scan.
- **The family moves fast.** `timeSeriesChangesToGrid`/`ResetsToGrid` landed
  after the original 25.6 set; signatures and the experimental-setting name have
  churn (see [Source-fidelity notes](#source-fidelity-notes-clickhouse-master)).
  Re-verify against the target ClickHouse tag before applying.
- **Histogram-quantile does NOT fit the base cleanly.** Its bucket is
  `(scale, zeroCount, pos/neg arrays)`, not scalar `(ts, value)`. It needs a
  sibling base, not a Traits — hence "Large" not "Small". Don't undersell it.
- **The moonshots are moonshots.** DDSketch-state, asof-grid-join, and
  Gorilla-state aggregates are genuine research bets with on-disk-format
  (`FORMAT_VERSION`) commitments and uncertain upstream reception. They are on
  the roadmap to be honest about the ceiling, not to be scheduled.

## First 90 days

1. **Weeks 1-2 — `timeSeriesIncreaseToGrid` (code-now).** Apply the staged
   patch to a ClickHouse fork, run the functional test, open the upstream PR
   (closes the obvious rate-shipped/increase-didn't gap in PR #80590's family).
   In parallel, land the cerberus-side `nativeTSGridFn` + gate change behind the
   existing experimental flag, gated on the chDB differential sweep.
2. **Weeks 3-4 — adopt `timeSeriesResampleToGridWithStaleness` (tiny).** Pure
   wire adoption of an already-shipped native function; retires the RANK-3
   `range_lwr.go` staleness fan-out. No upstream work. Confirm half-open-window
   semantics match `RangeLWR` argMax first.
3. **Weeks 5-8 — `*_over_time` moments (ambitious).** Ship the O(1)-state
   reducers (avg/min/max/sum/count/stddev via Welford) as the
   `timeSeriesReduceOverTimeToGrid` family — explicitly invited by the upstream
   `applyFunctionOverRange.cpp` TODO. Moments first, `mad`/`quantile` later.
   One upstream function pays off across PromQL, LogQL, and TraceQL because all
   three funnel through one `chplan` IR.
4. **Weeks 9-12 — native columnar decode prototype (5A) + holt_winters
   (ambitious).** Prototype Native-block result decode + external-table push on
   the existing clickhouse-go/v2 transport. Land `timeSeriesHistWintersToGrid`
   if the moments work proves the Traits pattern is mechanical.

Histogram-quantile (rank 5) and the moonshots are explicitly NOT in the first
90 days — they are the next quarter's bets, pending the moments work proving out
the sibling-base / sketch-state patterns.

## Source-fidelity notes (ClickHouse `master`)

Mirrored against ClickHouse `master` as of 2026-06-18. Exact files/lines:

- `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesExtrapolatedValue.h`
  — `AggregateFunctionTimeseriesExtrapolatedValueTraits` (template params
  `<array_arguments_, TimestampType_, IntervalType_, ValueType_, is_rate_>`,
  lines ~21-57); `getName()` rate/delta switch (line ~33); `Bucket =
  absl::flat_hash_map<TimestampType, ValueType>` (line ~38); `FORMAT_VERSION`
  / `DateTime64Supported = true` (line ~65); `fillResultValue()` with the
  `if (is_rate && value_difference > 0 && first_value >= 0)` zero-clamp and the
  `if constexpr (is_rate) factor = factor * Base::timestamp_scale_multiplier /
  Base::window` divide.
- `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesHelpers.cpp`
  — `createAggregateFunctionTimeseries<is_rate_or_resets, is_predict,
  FunctionTraits, Function>` template; `registerAggregateFunctionTimeseries`
  with `factory.registerFunction("timeSeriesRateToGrid", {... <true, false,
  ExtrapolatedValueTraits, ExtrapolatedValue>, ...})` (line ~371) and
  `"timeSeriesDeltaToGrid"` `<false, false, ...>` (line ~449); the
  experimental gate at line ~253 reads
  `allow_experimental_time_series_aggregate_functions` (OR
  `allow_experimental_time_series_table`).
- `src/Core/Settings.cpp:8238` — `DECLARE_WITH_ALIAS(Bool,
  allow_experimental_time_series_aggregate_functions, false, ..., EXPERIMENTAL,
  allow_experimental_ts_to_grid_aggregate_function)`. Confirms the short name
  (`allow_experimental_ts_to_grid_aggregate_function`, used by the docs and the
  stateless tests) IS the registered alias of the gate-checked long name, so
  setting either enables the function.
- `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesBase.h`
  — `State { UnorderedMapWithMemoryTracking<size_t, Bucket> buckets; }`;
  `doInsertResultInto` sets `adjust_to_resets = is_rate` and accumulates
  `accumulated_resets_in_window` when `samples_in_window.back().second > value`.
- `tests/queries/0_stateless/03254_timeseries_functions.{sql,reference}` —
  the canonical rate/delta golden; `SET
  allow_experimental_ts_to_grid_aggregate_function = 1;`.

Uncertainty / things to re-verify before applying:

- **Setting-name split (RESOLVED).** The runtime ENFORCEMENT check
  (`AggregateFunctionTimeseriesHelpers.cpp:253`) reads
  `allow_experimental_time_series_aggregate_functions`; the docs strings and the
  stateless tests use `allow_experimental_ts_to_grid_aggregate_function`. These
  are confirmed aliases: `src/Core/Settings.cpp:8238` declares the long name with
  `DECLARE_WITH_ALIAS(..., EXPERIMENTAL,
  allow_experimental_ts_to_grid_aggregate_function)`, so setting the short name
  sets the gate-checked long name. (DeepWiki incorrectly reported them as
  distinct settings; the raw `Settings.cpp` source disproves that.) The staged
  contrib documents both; cerberus's engine stamps the long name (`chopt` note
  in `internal/chopt/registry.go`). Re-confirm the `DECLARE_WITH_ALIAS` survives
  on the target release tag.
- **`fillResultValue` line numbers drift.** The function body was read via
  DeepWiki + raw GitHub; exact line numbers shift between releases. The patch is
  expressed as minimal anchored edits, not absolute-line hunks, to survive this.
- **Pre-window-sample semantics** of `isSampleOutOfGrid` (see Adversarial
  honesty) were not byte-confirmed; flagged for the differential sweep.
