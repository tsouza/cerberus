# Observability — background

This document collects the design rationale, rejected alternatives, and
incident history behind [`observability.md`](observability.md). It answers "why
is the telemetry shaped this way" rather than "what does it emit" — nothing
here is required to consume cerberus's own metrics, logs or traces correctly;
`observability.md` carries the instrument inventory, the label sets and the
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

ClickHouse answering a statement with a typed exception is positive proof it
is serving. Counting statement-scoped rejections as breaker failures turns a
handful of bad queries into a shed-everything outage: exactly what happened
when ~21 code-704 rejections became 1015 compat divergences and read like a
total engine regression for 32 hours.

## Why `cerberus_error_reason` cannot be derived from the status

Upstream wire parity pins two statuses onto three meanings — every head
answers a timeout with 503 because upstream Prometheus and Loki do, and a
budget refusal with 422 — so a status-derived reason would file every timeout
under `backend_unavailable`, indistinguishable from a real ClickHouse outage,
and every capacity refusal under `bad_request`, indistinguishable from a
malformed query. Recording the reason on a request-scoped cell keeps the wire
bytes unchanged while the label tells the truth.

A client cancellation is the third collision, and the one where the heads
disagreed outright before the reason travelled on the error itself. Tempo
answers 499, deliberately outside the 5xx band so a client hanging up is never
read as "cerberus is unhealthy"; Prometheus and Loki answer 503 to stay
byte-compatible with upstream's `errorCanceled` envelope. Derived from the
status, the same event read `bad_request` on one head and
`backend_unavailable` on the other two, and neither is true.

## Why the duration ladders reach the minute scale

The SDK default ladder is millisecond-shaped and these instruments record
seconds; a gateway fronting an analytical database can serve a request slower
than any single-digit-second bound, so both ladders reach the minute scale.
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

## Why the asynchronous-metrics queries never name `key_values`

ClickHouse 26.8 added the `key_values Map(LowCardinality(String), Float64)`
column to `system.asynchronous_metrics` and moved every per-core / per-device
family into it (ClickHouse #115333, #116791), leaving `value` NaN on those rows.
The collector had selected `metric AS name, value`, so on a 26.8 server every
per-core and per-device reading arrived as a single NaN series and the key was
lost.

Selecting `key_values` directly fixes 26.8 and breaks every older server with
`UNKNOWN_IDENTIFIER`, and the collector cannot branch on the server version.
`tupleElement(tuple(*), 'key_values', map())` resolves at analysis time against
whatever columns the table has: with `enable_named_columns_in_function_tuple`
the `*` expansion builds a named tuple, and `tupleElement` returns its default
when the name is absent. Measured against 24.8, 25.9, 26.6 and 26.8.10.6: both
queries run everywhere, and the key-value query returns zero rows on the servers
that have no such column. A `formatRow('JSONEachRow', *)` + `JSONExtract`
variant also works but serialises every row to JSON to read one field.

## Why scalar and key-value rows are two queries

One query with `attribute_columns: [name, key]` would stamp `key=""` on every
scalar series, changing the identity of every series the dashboards already
read. Two queries against the same metric name keep the scalar series exactly
as they were and add `key` only where a key exists.

## Why legacy duplicates are recognised by their description

Under `asynchronous_metrics_key_values_mode: both` the server publishes each
key twice: once inside its family's `key_values` map and once as a scalar with
the key folded into the name. The folding is not one pattern — `OSUserTimeCPU3`,
`CPUFrequencyMHz_0`, `BlockReadBytes_sda`, `Temperature0`, `EDAC0_Correctable`
— so name matching would need a per-family rule that silently misses the next
family. Every legacy row carries its family's description verbatim instead.
Measured on 26.8.10.6 in `both` mode: 163 map entries across the key-value
families, and exactly 163 scalar rows whose description equals a key-value
row's; in `key_values` mode no scalar row shares a key-value row's description.
A family ClickHouse introduces after 26.8 has no legacy form and is unaffected.

## Why the key attribute is called `key`

`system.asynchronous_metrics` publishes the map without naming what its keys
are; only ClickHouse's own Prometheus endpoint picks a per-family label name
(`device`, `cpu`, …). The collector reads the table, not the endpoint, so the
one name that is true for every family is `key`.
