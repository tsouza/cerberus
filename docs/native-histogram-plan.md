# Native (exp) histogram support — plan + deferred phases

OTel-CH writes exponential ("native") histograms into the
`otel_metrics_exp_histogram` table. PromQL's `histogram_quantile(phi, X)`
needs three pieces to support these end-to-end:

1. **Schema columns** — name the per-row Scale / ZeroCount / ZeroThreshold /
   Positive\* / Negative\* columns so the lowering can build column
   references without string scattering.
2. **chplan IR node** — encode the quantile-over-exp-histogram operation
   as a self-contained plan node so the chsql emitter has one switch
   case to render.
3. **chsql emitter** — render the log-scale interpolation as a portable
   CH expression chain over `arrayCumSum` / `arrayFirstIndex` /
   `pow(2, pow(2, -Scale))`.

PR [#171](https://github.com/tsouza/cerberus/pull/171) ("feat(promql):
histogram_quantile on native (exp) histograms (RC2 schema H)") shipped
the v0.1 seed: bare-selector instant-mode quantile over the positive
bucket array.

This document tracks **what's done** and **what's deferred** so the next
phase has a citable reference rather than rediscovering the design.

## Phase 1 (shipped via #171)

- `internal/schema/otel.go`
  - `Metrics.ExpHistogramTable` ("otel\_metrics\_exp\_histogram").
  - `Metrics.ScaleColumn` / `ZeroCountColumn` / `ZeroThresholdColumn` /
    `PositiveOffsetColumn` / `PositiveBucketCountsColumn` /
    `NegativeOffsetColumn` / `NegativeBucketCountsColumn`.
  - `Metrics.ExpHistogramSuffix` (default `"_exp_hist"`) + the
    `IsExpHistogramMetric` predicate for routing.
- `internal/chplan/histogram_quantile_native.go`
  - `HistogramQuantileNative` IR node carrying every column name
    above. Negative\* columns are surfaced on the node even though
    Phase 1 only walks the positive side — Phase 4 (negative-side
    observations) extends the emitter without an IR break.
- `internal/promql/histogram_quantile.go`
  - `lowerHistogramQuantileNative` lowering for bare-selector instant
    eval. Routes through `IsExpHistogramMetric` so the suffix knob is
    the single dispatch point.
- `internal/chsql/histogram_quantile_native.go`
  - `emitHistogramQuantileNative` renders the positive-only walk:
    `cum = arrayCumSum(arrayConcat([ZeroCount], PositiveBucketCounts))`,
    `idx = arrayFirstIndex(c -> c >= phi*total, cum)`,
    log-linear interpolation as
    `pow(base, PositiveOffset + (idx - 2) + fraction)` where
    `base = pow(2, pow(2, -Scale))`.
  - Four edge-case branches: `total = 0 -> NaN`, `phi <= 0 -> 0`,
    `phi >= 1 -> pow(base, PositiveOffset + length(pbc))`,
    `idx = 1 -> ZeroThreshold`.
- TXTAR fixtures under `test/spec/promql/`:
  - `histogram_quantile_native_p50.txtar` /
    `histogram_quantile_native_p99.txtar` (instant eval, two phi
    points).
  - `histogram_quantile_native_single_metric.txtar` (single-series
    sanity).
  - `histogram_quantile_native_multi_series.txtar` (per-series
    quantile keyed by Attributes).
  - `histogram_quantile_native_empty.txtar` (empty result set).
  - `edge_hq_native_p25.txtar` / `edge_hq_native_p999.txtar` (edge phi
    values).

## Phase 2 (shipped) — aggregated-input native path

PromQL's canonical histogram idiom is
`histogram_quantile(phi, sum by(le)(rate(<metric>[5m])))`. Cerberus's
classic-histogram path lowers this via `lowerHistogramQuantileAgg` /
`lowerHistogramQuantileClassicAggRange`
(`internal/promql/histogram_quantile.go`) by rewriting the inner chain
to `sumForEach(BucketCounts)` + `any(ExplicitBounds)` over a
time-bounded Filter.

The native equivalent is harder because OTel exp-histogram rows carry
variable-length / variable-offset / variable-scale bucket arrays —
two series in the same aggregation group may differ along every
axis. Phase 2 implements the full merge in `lowerHistogramQuantileNativeAgg`
(`internal/promql/histogram_quantile.go`) as a two-layer chplan:

- **Inner Aggregate**: groups by the user's `by/without` clause (after
  dropping `le` — exp histograms have no `le` label) and collects per-row
  bucket data into groupArrays plus min/max/sum aggregates of the
  scalar fields:

  - `min(Scale) AS _hq_merged_scale` — the coarsest Scale across the
    group, used as the merged distribution's target Scale.
  - `sum(ZeroCount) AS ZeroCount` — trivially sums (the merged zero
    bucket count is the sum of individual ZeroCounts).
  - `max(ZeroThreshold) AS ZeroThreshold` — the merged zero bucket
    spans the largest individual zero bucket.
  - `groupArray({Scale, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts})`
    — five aliases (`_hq_scales`, `_hq_pos_offsets`,
    `_hq_pos_buckets`, `_hq_neg_offsets`, `_hq_neg_buckets`) carrying
    per-row data through to the wrapping Project.

- **Outer Project** (`expHistogramMergeOffsetExpr` +
  `expHistogramMergeBucketsExpr` in `internal/promql/histogram_quantile.go`)
  folds the groupArrays into a single merged distribution by:

  - **Scale folding.** Per-row downscale to merged Scale via the
    canonical "absolute bucket idx >> (origScale - targetScale)"
    mapping (mirrors `model/histogram/float_histogram.go::targetIdx`
    in the upstream Prom fork). Uniform-Scale groups (the common
    case) collapse to identity since `delta = 0`.
  - **Offset alignment + zero-pad.** Each row's downscaled bucket
    array contributes to the merged array starting at
    `PositiveOffset >> delta` (absolute bucket index at merged
    scale); the merged array spans
    `[arrayMin(downscaled_start), arrayMax(downscaled_end))` across
    rows. Rows that don't cover the full range contribute 0 to the
    uncovered positions (the per-target-bucket sum naturally yields
    0 when no row contributes a value to that index).
  - **Element-wise sum.** For each target absolute bucket index `T`
    in the merged range, the merged bucket count is the sum over
    rows of `arraySum(arrayMap(j -> if((off_i + j - 1) >> delta_i ==
    T, arr_i[j], 0), arrayEnumerate(arr_i)))`.

The merge expression is built from the typed chplan `Lambda` /
`BareIdent` / `Subscript` Expr types introduced for this milestone
(`internal/chplan/lambda.go`); the chsql emitter renders them as
`(p1, p2) -> body` / `bare_ident` / `<container>[<key>]`
respectively, with the lambda body composed of standard chplan
`FuncCall` / `Binary` / `ColumnRef` / `LitInt` nodes that flow
through the existing `Builder.Expr` dispatch.

Test coverage (Phase 2):

- Layer 2a (TXTAR + chplan + chDB roundtrip):
  - `test/spec/promql/histogram_quantile_native_agg.txtar`
    (`sum by(le)`, two series, uniform Scale).
  - `test/spec/promql/histogram_quantile_native_agg_p50.txtar`
    (p50 sanity).
  - `test/spec/promql/histogram_quantile_native_agg_groupby.txtar`
    (`sum by(le, service)`, multiple groups).
  - `test/spec/promql/histogram_quantile_native_agg_mixed_scale.txtar`
    (two series at different Scales — exercises the consolidation
    path).
- Layer 2b (chplan shape):
  - `TestLower_HistogramQuantile_OverAggregation_Native`
    (`internal/promql/histogram_quantile_test.go`) — pins the
    Project / HistogramQuantileNative / Project / Aggregate / Filter
    / Scan tree shape across `sum by`, `sum without`, bare `rate`,
    and `increase`.
  - `TestLower_HistogramQuantile_OverAggregation_Native_LeDropped` —
    pins the `le`-drop rule on the native side.
- Layer 6a (end-to-end chDB regression):
  - `TestQuery_HistogramQuantileNativeAgg_ChDB`
    (`internal/api/prom/handler_chdb_range_mode_test.go`) — uniform
    Scale, asserts the interpolated value `~7.294`.
  - `TestQuery_HistogramQuantileNativeAgg_MixedScale_ChDB` —
    mixed-Scale merge, asserts the consolidated-then-interpolated
    value `~3.801` against chDB.

Phase 4 (negative-side observations) is shipped; the IR contract
already carries the columns each phase needs.

## Phase 3 (shipped) — range-mode (per-step anchor grid)

The classic-histogram per-step rewrite landed via PRs #347 (LWR
rework) and #353 (classic-bucket range-mode anchor). The native path
now mirrors that scaffold under the same `histogramRangeApplies` gate
(`internal/promql/histogram_quantile_range.go`): `query_range` over a
native-histogram metric emits one quantile row per (series, anchor)
instead of collapsing to a single `now64(9)` instant.

Two range-mode lowerings live alongside the classic helpers:

- `lowerHistogramQuantileNativeBareRange` — bare `_exp_hist` selector,
  per-anchor LWR via `argMax(<col>, TimeUnix)` over every per-row
  exp-histogram field, lookback = `instantLookback` (5m).
- `lowerHistogramQuantileNativeAggRange` — the aggregated idiom
  `sum [by/without](rate(<sel>_exp_hist[r]))`, per-anchor merge via
  `groupArray`-and-fold (same `expHistogramMergeOffsetExpr` /
  `expHistogramMergeBucketsExpr` helpers as the instant-mode
  aggregated path), lookback = the rate's `[range]`.

Both surface `anchor_ts AS TimeUnix` in the outer Sample-row Project
so the matrix pivot in `internal/api/prom/handler.go` reads one row
per step. Modifier-bearing selectors (`@` / `offset`) fall back to the
instant-mode path until matrix-anchor handling lands — rare in
practice (Grafana never threads modifiers through `histogram_quantile`
on `query_range`).

## Phase 4 (shipped) — negative-side observations

OTel exp-histograms can carry observations < 0 in the `Negative*`
arrays. Cerberus's original positive-only emitter ignored them — quantiles
over distributions with negative observations returned the quantile of
the non-negative subset only.

The Phase 4 emitter walks the full distribution:

- The `cum` array walks `Negative` (in reverse via `arrayReverse`),
  then `ZeroCount`, then `Positive` so the cumulative sum reflects the
  natural ordering of the distribution. `arrayReverse([]) = []`, so
  distributions with no negative observations collapse to the Phase 1
  shape.
- Negative-bucket interpolation uses
  `-pow(base, NegativeOffset + (nlen - idx) + 1 - fraction)`.
- The zero band interpolates linearly between
  `-ZeroThreshold` and `+ZeroThreshold`.
- Edge cases:
  - `phi <= 0` returns `-pow(base, NegativeOffset + nlen)` when
    negative observations exist, else `0.0`.
  - `phi >= 1` returns the positive upper edge when positive
    observations exist; falls back to `ZeroThreshold` when only the
    zero bucket is non-empty; falls back to `-pow(base, NegativeOffset)`
    (least-negative upper edge) when only negative observations exist.
- The IR node already carried `NegativeOffsetColumn` /
  `NegativeBucketCountsColumn`; the change was isolated to the emitter
  in `internal/chsql/histogram_quantile_native.go`.

## Routing knob

`schema.Metrics.ExpHistogramSuffix` controls dispatch. Default
`"_exp_hist"`; setting empty disables native routing entirely (all
`histogram_quantile` calls fall through to the classic path). The
upstream PromQL wire format has no per-metric tag distinguishing
native from classic, so cerberus uses metric-name convention as the
deployment-level signal.

Operators that follow a different convention override the suffix via
`Config.Schema.Metrics.ExpHistogramSuffix`.

## Test coverage map

| Layer | Path                                                                              | Phase 1 | Phase 2 | Phase 3 | Phase 4 |
| ----- | --------------------------------------------------------------------------------- | ------- | ------- | ------- | ------- |
| 2a    | `test/spec/promql/histogram_quantile_native{,_agg,_range}*.txtar` (chplan + SQL)  | yes     | yes     | yes     | yes     |
| 2b    | `internal/promql/histogram_quantile_{native,}_test.go` (lowering)                 | yes     | yes     | yes     | n/a     |
| 3     | `internal/chplan/equal_invariants_test.go` (IR Equal)                             | yes     | reuses  | reuses  | reuses  |
| 5     | `internal/chsql/histogram_quantile_native_test.go` (emitter shape)                | yes     | reuses  | reuses  | yes     |
| 6a    | TXTAR `-- seed --` / `-- expected_rows --` chDB roundtrip + `handler_chdb_*_test` | yes     | yes     | yes     | yes     |

Each subsequent phase opens its own PR; the box above tracks which
layers gain coverage. The Layer 6a roundtrip is the strongest signal
that the algorithm is correct against a real CH engine — Phase 1's
fixtures already pin it for the bare-selector path.
