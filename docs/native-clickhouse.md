# Native ClickHouse: what cerberus uses, and upstream positioning

This note records two things and nothing more: the native ClickHouse
capability cerberus exploits **today**, and where cerberus sits relative
to ClickHouse's own observability tracks (the upstream positioning).

The `timeSeries*ToGrid` aggregates cerberus's heavy lowerings would push
upstream are largely already shipped or in-flight by ClickHouse staff; the
one cerberus-adjacent gap lives in an engine cerberus does not use. cerberus's
actual job sits in the gap both of ClickHouse's observability tracks leave. See
**Upstream positioning** below.

## A. What cerberus uses today

Cerberus opportunistically lowers PromQL/LogQL/TraceQL to ClickHouse's
*shipped* native aggregates and engine features whenever the connected
server supports them. Each lowering is version-floored and feature-gated,
so an older or differently-configured server transparently falls back to
the portable SQL path. What it currently exploits:

- **`timeSeriesRateToGrid`** (`rate`, ClickHouse 25.6; auto-enabled at 25.9,
  the left-open window fix) — computes a whole PromQL grid in one columnar pass
  per series instead of exploding each sample into per-anchor copies.
- **`timeSeriesChangesToGrid` / `timeSeriesResetsToGrid`**
  (`changes` / `resets`, ClickHouse 25.9) — same one-pass shape for the
  adjacent-pair counters.
- **`timeSeriesDerivToGrid` / `timeSeriesPredictLinearToGrid`**
  (`deriv` / `predict_linear`, ClickHouse 25.8, registry-pinned to the family's
  25.9 floor) — the last members of the family to adopt the native path: a
  per-window least-squares fit whose slope is `deriv` and whose `slope*t +
  intercept` projection is `predict_linear`, retiring the
  `simpleLinearRegression`/`arrayReduce` fan-out. `predict_linear` threads its
  whole-second horizon `t` as the aggregate's 5th parametric arg; a computed or
  fractional `t` stays on the fan-out. Both regression aggregates are fed a
  whole-second timestamp axis (`toDateTime(ts)`) that matches the fan-out's
  `dateDiff('second', anchor, ts)` regression x-axis — without it
  `timeSeriesDerivToGrid` computes a per-nanosecond slope (1e9× too small) off
  the raw `DateTime64(9)` column. The whole-second axis is chosen so that native
  stays **bit-identical to the fan-out**, whose own x-axis is that floored
  `dateDiff('second', …)`. With the matching axis native == fan-out is
  **bit-identical for whole-second-aligned samples**, proven directly on the chDB
  CI substrate by the dual-emit parity tests (`range_window_deriv_chdb_test.go` /
  `range_window_predict_linear_chdb_test.go`): the substrate is ClickHouse 26.5,
  above the 25.9 floor, so it ships the aggregates and the native half genuinely
  fires in the `chdb` lane.
  - **Known limitation (why the native regression path stays experimental,
    default-off behind `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`):** the aggregate
    accepts only a `DateTime`/`DateTime64` timestamp (it rejects `Float64` /
    `Decimal`), so its single ts argument drives both the regression x-axis *and*
    the window-membership bucketing — there is no way to keep a whole-second
    x-axis while bucketing membership on the raw timestamp. On sub-second-offset
    samples that straddle a window boundary, the native path buckets by the
    floored second while the fan-out (and Prometheus) decide membership on the raw
    timestamp, so a boundary sample can land in a different grid window between the
    two paths. This gap is characterised and pinned by
    `range_window_regression_subsecond_chdb_test.go`. The per-function outlook:
    - `deriv`: feeding the raw `DateTime64(9)` axis and scaling the slope by 1e9
      is actually **more** correct (raw-ts membership + fractional-second x =
      Prometheus's deriv) and is numerically sound at production ns magnitude —
      the least-squares slope is a centered difference, not an absolute-magnitude
      sum, so it does not overrun float64's exact range. It is kept on the
      whole-second axis only to stay bit-identical to the (floored) fan-out;
      moving it to raw-ns would improve correctness at the cost of that guard.
    - `predict_linear`: raw-ns is genuinely broken — its result is an *absolute*
      forecast (`intercept + slope*(anchor + offset)`) evaluated at ~10¹⁸ ns,
      where catastrophic cancellation destroys all precision, and no scale trick
      recovers it. It cannot be made sub-second-correct via the native aggregate.
    Because the flag gates the whole family, that inherent `predict_linear`
    limitation is what keeps the native regression path default-off; closing (or
    formally accepting) the sub-second membership gap is the gate before it is
    promoted.
- **`timeSeriesResampleToGridWithStaleness`** — native instant-vector
  selection with Prometheus staleness, retiring the staleness fan-out.
- **`condition_cache`** and **`aggregation_in_order`** — server-side
  execution settings cerberus enables where available.
- **Client-side columnar result decode** — decoding result blocks
  column-at-a-time over the Native protocol cerberus already speaks,
  rather than row-scanning.

The authoritative, generated list — exact aggregates, version floors,
experimental-setting names, and feature gates — is the catalog in
[`docs/clickhouse-optimizations.md`](clickhouse-optimizations.md). That
file is the source of truth; this note deliberately does not duplicate it.

## B. Upstream positioning

ClickHouse pursues observability along two parallel tracks, and cerberus
is on neither:

1. **Core "Prometheus backend"** — the `TimeSeries` table engine, the
   `prometheusQuery` / `prometheusQueryRange` PromQL engine, and the
   native `timeSeries*ToGrid` aggregates (experimental, behind
   `allow_experimental_time_series_aggregate_functions`). This track
   assumes data lives in ClickHouse's own Prometheus-shaped schema.
2. **ClickStack / HyperDX** — OpenTelemetry `otel_metrics_*` tables
   queried via SQL or Lucene, with **no** PromQL surface at all.

Cerberus's job is PromQL/LogQL/TraceQL over **arbitrary, pre-existing**
ClickHouse schemas. That sits in the gap both tracks leave: track 1
requires you adopt ClickHouse's Prometheus schema, and track 2 offers no
PromQL. Arbitrary-schema PromQL is cerberus's moat, and it is exactly the
thing neither upstream track provides.

### We are not upstreaming aggregates to ClickHouse

The decision is to **not** contribute aggregates upstream. ClickHouse's AI
contribution policy is permissive, so policy is not the blocker. The
reasons are substantive:

- Most candidate native aggregates cerberus would have proposed
  (`increase`, the `*_over_time` family, classic `histogram_quantile`) are
  **already shipped or in-flight** by ClickHouse staff.
- The one cerberus-adjacent gap — exp-histogram `histogram_quantile` —
  lives inside ClickHouse's **own** `prometheusQuery` PromQL engine, which
  cerberus does not use. Fixing it there would not help cerberus.
- Cerberus's real value — arbitrary-schema PromQL/LogQL/TraceQL — is not
  expressible as a single aggregate and sits in the gap both ClickHouse
  tracks leave. There is nothing schema-agnostic to upstream.

So cerberus stays a consumer of ClickHouse's shipped native features
(section A), not a contributor of new ones. If that calculus changes — a
genuinely novel aggregate with no upstream equivalent and clear
cross-track value — this note is where the reasoning to revisit lives.
