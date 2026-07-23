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
  fractional `t` stays on the fan-out. The native == fan-out numeric parity is a
  Float64 fit (ULP-close, not bit-identical), proven on a `>= 25.9` server in
  the prod/e2e differential lane — the sub-25.9 chDB CI substrate lacks the
  aggregates, so it runs the fan-out and the always-on SQL-shape goldens pin the
  native emit.
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
