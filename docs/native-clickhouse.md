# Native ClickHouse: what cerberus uses

This note records the native ClickHouse capability cerberus exploits
**today**. Cerberus is a consumer of ClickHouse's shipped native features,
not a contributor of new ones: no cerberus aggregate is proposed upstream.

## What cerberus uses today

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
  - **Known limitation (the native regression path is experimental maturity,
    auto-enabled only on a capable server — ClickHouse >= 25.9 and the server
    permits `allow_experimental_time_series_aggregate_functions`):**
    the aggregate
    accepts only a `DateTime`/`DateTime64` timestamp (it rejects `Float64` /
    `Decimal`), so its single ts argument drives both the regression x-axis *and*
    the window-membership bucketing — there is no way to keep a whole-second
    x-axis while bucketing membership on the raw timestamp. On sub-second-offset
    samples that straddle a window boundary, the native path buckets by the
    floored second while the fan-out (and Prometheus) decide membership on the raw
    timestamp, so a boundary sample can land in a different grid window between the
    two paths. This gap is characterised and pinned by
    `range_window_regression_subsecond_chdb_test.go`. Closing (or formally
    accepting) that gap is the gate before the path is promoted beyond
    experimental maturity;
    [`native-clickhouse.background.md`](native-clickhouse.background.md) carries
    the per-function analysis of why a raw-nanosecond axis is not the way out.
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

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [native-clickhouse.background.md](native-clickhouse.background.md).
