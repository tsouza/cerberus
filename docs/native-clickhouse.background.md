# Native ClickHouse — background

This document collects the design rationale, rejected alternatives, and upstream
positioning behind [`native-clickhouse.md`](native-clickhouse.md). It answers
"why does cerberus use ClickHouse this way, and why does it not push work
upstream" rather than "what does it use" — nothing here is needed to read the
capability list, the version floors, or the feature gates;
`native-clickhouse.md` is self-sufficient for that.

## Why the native regression path is default-off

The sub-second window-membership gap characterised in `native-clickhouse.md`
has a different outlook per function:

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

Because `allow_experimental_time_series_aggregate_functions` gates the whole
family, that inherent `predict_linear` limitation is what keeps the native
regression path default-off: the one function that cannot be fixed sets the
maturity of the family it shares a flag with.

## Upstream positioning

The `timeSeries*ToGrid` aggregates cerberus's heavy lowerings would push
upstream are largely already shipped or in-flight by ClickHouse staff; the
one cerberus-adjacent gap lives in an engine cerberus does not use. cerberus's
actual job sits in the gap both of ClickHouse's observability tracks leave.

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

So cerberus stays a consumer of ClickHouse's shipped native features, not a
contributor of new ones. If that calculus changes — a genuinely novel
aggregate with no upstream equivalent and clear cross-track value — this
document is where the reasoning to revisit lives.
