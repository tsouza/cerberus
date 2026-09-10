# Observability — background

This document collects the design rationale, rejected alternatives, and
incident history behind [`observability.md`](observability.md). It answers "why
is the telemetry shaped this way" rather than "what does it emit" — nothing
here is required to consume cerberus's own metrics, logs or traces correctly;
`observability.md` carries the full inventory, the label sets and the
cardinality contract.

## Why `cerberus_queries_duration_exp_hist` is a native histogram

Collecting the query-duration histogram as a native/exponential histogram
(cerberus issue #3170) removes this metric from the class of risk a wide
classic histogram carries under the `RangeBucketGridNative` query-time cost
guard (`internal/chsql/range_bucket_grid_native_bound.go`) — this metric
already tripped that guard once, at a 24h self-monitoring window, before the
guard's own recalibration fixed that specific incident.

## Why the breaker phase rides a gauge level, not a `state` label

The phase rides `cerberus_ch_breaker_state`'s numeric LEVEL, never a `state`
label: an OTel observable gauge only overwrites the series it re-observes, so a
phase-keyed label would orphan the previous phase's series and leave a
recovered breaker still exporting `state="open"=1` forever.

## Why statement-scoped rejections are counted separately

Counting statement-scoped ClickHouse rejections as breaker failures turns a
handful of bad queries into a shed-everything outage: exactly what happened
when ~21 code-704 rejections became 1015 compat divergences and read like a
total engine regression for 32 hours.

## Why `cerberus_error_reason` cannot be derived from the status

Upstream wire parity pins two statuses onto three meanings, so a status-derived
reason would file every timeout under `backend_unavailable`, indistinguishable
from a real ClickHouse outage, and every capacity refusal under `bad_request`,
indistinguishable from a malformed query.

A client cancellation is the third collision, and the one where the heads
disagreed outright before the reason travelled on the error itself. Derived
from the status, the same event read `bad_request` on one head (Tempo's 499)
and `backend_unavailable` on the other two (Prometheus's and Loki's 503), and
neither is true.

## Why the duration ladders reach the minute scale

Every observation past the top FINITE bucket is unresolvable — the `+Inf`
bucket has no upper bound for `histogram_quantile` to interpolate against, so
once it holds more than 5% of the observations p95 and p99 both collapse onto
the top finite bound and the slow tail disappears exactly where an
investigation needs it.

## Why the stage histogram carries `cerberus_ql`

One process serves all three heads. Without the label, a slow `parse` or
`execute` cannot be attributed to a language, and the metric would only be
separable in a deployment that happened to split the heads into separate
processes — a property of that deployment, not of the metric.

## Why SDK export failures log at `WARN`

`WARN` rather than `INFO` because the condition is actionable and degrading: an
export failure means the gateway has lost its OWN telemetry, which is precisely
what an operator reaches for when diagnosing a failure.

## Why the query-failure log line exists

Before it, a query that failed against ClickHouse — a timeout, an OOM /
memory-limit abort, a ClickHouse exception, a caller cancellation, or any other
non-`ok` exit — left no trace in `kubectl logs` at all. The only record was
cerberus's own ClickHouse-side performance corpus (`internal/optcorpus`,
`docs/router-rules.md`), which is off by default
(`CERBERUS_CH_OPT_CORPUS_ENABLED`) and, even when on, is written on an interval
into a table an operator troubleshooting "things are slow" has no reason to
think of querying. A routine incident investigation ended up as a multi-hour
ClickHouse-side deep-dive instead of a five-minute `grep`.
