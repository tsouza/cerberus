# Observability

Cerberus is its own first-class observability customer: it queries OTel-CH
metrics + logs + traces, and it ships the same shape of telemetry back into
that store so a self-dashboard works against a running cluster.

The self-observability stack covers all three OTel pillars over the
same OTLP gRPC transport. Each pillar exports to the same collector,
which writes to the same ClickHouse tables cerberus queries on its
Grafana-facing side — the deployment dogfoods itself end-to-end.

| Pillar      | Surface                                                                                      |
| ----------- | -------------------------------------------------------------------------------------------- |
| **Logs**    | `log/slog` → `bridges/otelslog` → OTLP gRPC → `otel_logs` (this page §Logging)               |
| **Traces**  | `otelhttp.NewHandler` (one span per HTTP request) + parse/lower/optimize/emit/execute stages |
| **Metrics** | Request count + latency + stage duration + CH rows/bytes + in-flight, all OTLP-exported      |

Resource attributes (`service.name = cerberus`, `service.version`,
`service.instance.id`) are attached identically to every span, metric
data point, AND log record so a Grafana dashboard can pivot on them
across all three signal types.

The k3s manifest at `test/e2e/k3s/otel-collector.yaml` and the provisioned
`test/e2e/grafana/dashboards/cerberus.json` wire the full export path
end-to-end against a running cluster.

## Logging

Cerberus uses the standard library's [`log/slog`](https://pkg.go.dev/log/slog)
for structured logging. Records fan out to **two sinks simultaneously**:

1. **stderr** — text or JSON per `CERBERUS_LOG_FORMAT`, so
   `kubectl logs` / `docker logs` tail cleanly.
2. **OTLP gRPC** — every record bridged via
   [`go.opentelemetry.io/contrib/bridges/otelslog`](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog)
   to the same collector endpoint that receives traces and metrics.
   Records land in `otel_logs` with full structured attributes
   preserved (no text-format round-trip).

The OTLP sink is enabled whenever `CERBERUS_OTLP_ENDPOINT` (`otlp.endpoint`) is
set; unset means no-op bridge (stderr-only fallback). Two settings steer the
stderr-side handler:

| Variable              | Config file  | Default | Allowed values                                       | Effect                                |
| --------------------- | ------------ | ------- | ---------------------------------------------------- | ------------------------------------- |
| `CERBERUS_LOG_FORMAT` | `logFormat`  | `text`  | `text`, `json` (case-insensitive)                    | slog handler kind                     |
| `CERBERUS_LOG_LEVEL`  | `logLevel`   | `info`  | `debug`, `info`, `warn`, `error` (+ `warning` alias) | Minimum level retained by the handler |

Every setting below names its environment variable; each one has an equivalent
`cerberus.yaml` path, listed alongside it in
[`docs/configuration.md`](configuration.md).

Invalid values surface as a startup error rather than silently downgrading
observability — a typo never ships to prod undetected.

### Format choice

- **`text`** is the local-dev default. Produces a `time=… level=… msg=… key=value …`
  stream that tails cleanly under `kubectl logs` or `docker logs`.
- **`json`** is the recommended setting for any deployment with a log
  aggregator (Loki, GCP Logging, ECS, Splunk). Each record is one
  newline-delimited JSON object, ready for ingest.

### Level vocabulary in cerberus code

- **`Debug`** — per-request SQL + arg traces. Off in prod by default; flip to
  `debug` to capture the lowered SQL for a complaint window.
- **`Info`** — lifecycle events only (`cerberus starting`, `HTTP listener
ready`, `signal received, shutting down`, `cerberus stopped`).
- **`Warn`** — recoverable conditions where the request can still be served
  meaningfully or the client is at fault (e.g. WebSocket upgrade rejected
  by the peer in the Loki `/tail` handler), plus degradation of a
  background subsystem — a dropped self-telemetry export, an optcorpus
  sink write that failed. Carries `component` so the subsystem is
  selectable. Also every query that did NOT complete cleanly — a
  timeout, an OOM abort, a ClickHouse exception, a caller cancellation
  (§"Query failure log line") — even though the request itself failed:
  most of these are not cerberus defects, so `Error`'s alerting posture
  would misclassify them.
- **`Error`** — handler-level failures that produce a 5xx (CH connection
  reset, plan emission internal error). The bridge to alerting.

### Attribute conventions

The codebase follows a small set of consistent keys so a future query
across `otel_logs` can filter without guessing:

| Key                            | Type   | Notes                                                                                                                 |
| ------------------------------ | ------ | --------------------------------------------------------------------------------------------------------------------- |
| `api`                          | string | `prom` / `loki` / `tempo`, set on the per-handler logger via `.With("api", ...)` in `cmd/cerberus/main.go`            |
| `promql` / `logql` / `traceql` | string | The query text as received                                                                                            |
| `sql`                          | string | The emitted ClickHouse SQL                                                                                            |
| `args`                         | []any  | Parameterised SQL args                                                                                                |
| `err`                          | error  | Native `error` value — slog encodes via `.Error()` for json + `%v` for text                                           |
| `trace_id`                     | string | Tempo `traceByID` handler only                                                                                        |
| `tag`                          | string | Tempo tag-values handler                                                                                              |
| `cerberus_ql`                  | string | `promql` / `logql` / `traceql`, on the query-failure log line (§"Query failure log line")                             |
| `shape_id`                     | string | The literal-free `cerb:<root>[;mod...]` plan shape id (`internal/engine.planShapeID`), same line                      |
| `decision_reason`              | string | The sharded-pushdown solver's `Reason` for this dispatch, or `""` when no classification ran, same line               |
| `exit_class`                   | string | The failure class (`timeout` / `oom` / `sample_budget` / `byte_budget` / `breaker` / `canceled` / `error`), same line |
| `duration_ms`                  | int64  | Wall-clock milliseconds the failing dispatch ran for, same line                                                       |
| `query_id`                     | string | The ClickHouse `query_id` (the join key into `system.query_log`), same line, empty when dispatch never reached CH     |

Always pass the native `error` as `"err", err` rather than `err.Error()`
so a future `slog.Handler` middleware can branch on `errors.As` /
`errors.Is`.

### Examples

```text
# CERBERUS_LOG_FORMAT=text CERBERUS_LOG_LEVEL=info (defaults)
time=2026-05-13T10:14:01.000Z level=INFO msg="cerberus starting" version=v1.0.0 http_addr=:8080 ch_addr=clickhouse:9000 ch_db=otel log_format=text log_level=INFO

# CERBERUS_LOG_FORMAT=json
{"time":"2026-05-13T10:14:01Z","level":"INFO","msg":"cerberus starting","version":"v1.0.0","http_addr":":8080","ch_addr":"clickhouse:9000","ch_db":"otel","log_format":"json","log_level":"INFO"}
```

## Schema-shape overrides

Cerberus reads the OpenTelemetry ClickHouse Exporter layout by default
(table names + column names mirror the upstream
`clickhouseexporter` DDL — see [`docs/upstream-forks.md`](upstream-forks.md)).
Deployments with a customised CH layout — renamed tables, sharded
clusters, alternate database conventions — override the table names via
env vars at startup; nothing rebuild-related is required.

| Variable                                      | Default                              | Effect                                             |
| --------------------------------------------- | ------------------------------------ | -------------------------------------------------- |
| `CERBERUS_SCHEMA_METRICS_GAUGE_TABLE`         | `otel_metrics_gauge`                 | Gauge-metrics table name.                          |
| `CERBERUS_SCHEMA_METRICS_SUM_TABLE`           | `otel_metrics_sum`                   | Sum / counter metrics table name.                  |
| `CERBERUS_SCHEMA_METRICS_HISTOGRAM_TABLE`     | `otel_metrics_histogram`             | Classic histogram metrics table name.              |
| `CERBERUS_SCHEMA_METRICS_EXP_HISTOGRAM_TABLE` | `otel_metrics_exponential_histogram` | Exponential / native histogram metrics table name. |
| `CERBERUS_SCHEMA_METRICS_SUMMARY_TABLE`       | `otel_metrics_summary`               | Summary metrics table name.                        |
| `CERBERUS_SCHEMA_LOGS_TABLE`                  | `otel_logs`                          | Logs table name read by the Loki API.              |
| `CERBERUS_SCHEMA_TRACES_TABLE`                | `otel_traces`                        | Spans table name read by the Tempo API.            |

The remaining schema knobs are opt-ins rather than table-name overrides:

| Variable                                            | Default | Effect                                                                                                                                                                                                                                                                                                                |
| --------------------------------------------------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_SCHEMA_TRACES_TS_LOOKUP`                  | `false` | When truthy, the Tempo trace-by-ID path window-prunes the spans scan through the OTel-CH `<spans>_trace_id_ts` lookup MV. Enable only after confirming that MV is populated; the lookup-table name derives from the (possibly overridden) spans table as `<spans>_trace_id_ts`.                                       |
| `CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED`              | `false` | Provision the DELTA-temporality prefix-reconstruction aggregate table and its materialized view (`schema.Metrics.DeltaPrefixTable`); a no-op unless `CERBERUS_AUTO_CREATE_SCHEMA` is also `true`. Provisioning only — the read path is gated separately by `CERBERUS_DELTA_PREFIX_READ_ENABLED` (`configuration.md`). |
| `CERBERUS_SCHEMA_TRACES_MATERIALIZED_ATTRS_ENABLED` | `false` | Provision and route to the curated materialized span/resource attribute columns on the spans table (`http.status_code`, `rpc.method`, `k8s.namespace.name`); a no-op unless `CERBERUS_AUTO_CREATE_SCHEMA` is also `true`.                                                                                             |
| `CERBERUS_PROM_RESOURCE_LABELS`                     | `""`    | Allowlist of OTel `ResourceAttributes` keys (dotted form, e.g. `k8s.namespace.name`) projected as Prometheus labels; empty promotes every resource key. Config-file key `prom.resourceLabels`.                                                                                                                        |

The active ClickHouse **database** is set by `CERBERUS_CH_DATABASE`
(default `default`) — that single knob covers both the connection's
default schema and the database the auto-create DDL targets, so no
separate `CERBERUS_SCHEMA_DATABASE` is required.

Whitespace-only values (e.g. an empty `""` or a value with stray
newlines) are treated as unset and fall back to the default. Non-empty
values are trimmed before use. Column-name overrides are not in the
current surface — open a tracking issue if a deployment needs them.

## Tracing + metrics export

Cerberus emits structured logs, spans, and self-metrics so an operator can
see what the gateway is doing in production.

### Topology

```text
cerberus ──OTLP gRPC──▶ OTel Collector ──CH exporter──▶ ClickHouse
                                                              │
                                                              ▼
                                                       Grafana ◀── cerberus (query)
```

The same `otel_traces` / `otel_metrics_*` tables cerberus queries on the
Grafana-facing side are also the ones its own telemetry lands in — the
deployment dogfoods itself.

### Environment variables

All OTel knobs are optional. With no env vars set, cerberus installs
no-op trace and meter providers and runs as a zero-collector-dependency
binary.

| Variable                        | Default | Meaning                                                                                          |
| ------------------------------- | ------- | ------------------------------------------------------------------------------------------------ |
| `CERBERUS_OTLP_ENDPOINT`        | `""`    | gRPC target, e.g. `otel-collector.observability.svc:4317`. Empty disables both exporters.        |
| `CERBERUS_OTLP_INSECURE`        | `false` | When `true`, dial the endpoint without TLS. Use for local dev / k3d only.                        |
| `CERBERUS_OTLP_HEADERS`         | `""`    | Comma-separated `key=value` list attached as gRPC metadata (e.g. `authorization=Bearer abc...`). |
| `CERBERUS_OTLP_TIMEOUT`         | `10s`   | Per-request OTLP roundtrip timeout.                                                              |
| `CERBERUS_OTLP_EXPORT_INTERVAL` | `10s`   | Metric `PeriodicReader` flush interval — how often self-metrics are pushed to the collector.     |

Standard OTel SDK env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_RESOURCE_ATTRIBUTES`, …) are read by
the SDK on top of the cerberus-specific knobs above. When both are set,
the `CERBERUS_OTLP_*` value wins for that field because cerberus passes
it explicitly to the exporter constructor.

`service.name` and `service.version` are fixed by the binary — `cerberus`
and the `Version` var — and `service.instance.id` is derived from the
hostname; the binary passes the first two explicitly, so `OTEL_SERVICE_NAME`
does not override them. `OTEL_RESOURCE_ATTRIBUTES` adds dimensions to that
resource and is the only channel for the ones cerberus has no dedicated
knob for. Describe cerberus along the same axes as every other producer in
the stack — Grafana's Traces Drilldown breaks a selected service down by
`resource.service.namespace`, so a cerberus that publishes none produces an
empty breakdown:

```sh
OTEL_RESOURCE_ATTRIBUTES=service.namespace=platform,deployment.environment=prod
```

### Self-metrics

The instrument set lives in `internal/telemetry`. Names, units and
attribute keys are a public contract — dashboards and alert rules
reference them verbatim. `internal/telemetry/contract_test.go` pins the
names and units of the query-pipeline instruments (the first eight rows)
so a rename there cannot ship silently.

| Metric                                        | Type               | Attributes                                                                                  |
| --------------------------------------------- | ------------------ | ------------------------------------------------------------------------------------------- |
| `cerberus_queries_total`                      | counter            | `cerberus_ql`, `cerberus_route`, `result`, `cerberus_error_reason`, `cerberus_status_class` |
| `cerberus_queries_duration_exp_hist`          | histogram (native) | `cerberus_ql`, `cerberus_route`, `result`                                                   |
| `cerberus_pipeline_stage_duration_seconds`    | histogram          | `stage`, `cerberus_ql`                                                                      |
| `cerberus_optimizer_rules_applied`            | histogram          | —                                                                                           |
| `cerberus_optimizer_fixpoint_cap_hits_total`  | counter            | `cerberus_optimizer_batch`                                                                  |
| `cerberus_clickhouse_rows_read`               | histogram          | `cerberus_ql`                                                                               |
| `cerberus_clickhouse_bytes_read`              | histogram          | `cerberus_ql`                                                                               |
| `cerberus_query_inflight`                     | gauge              | `cerberus_ql`                                                                               |
| `cerberus_route_memo_hit_skipped_total`       | counter            | `reason`                                                                                    |
| `cerberus_route_memo_pressure_active`         | gauge (0/1)        | —                                                                                           |
| `cerberus_routed_dispatch_inflight`           | gauge              | —                                                                                           |
| `cerberus_route_ab_success_total`             | counter            | `cerberus_route_choice`                                                                     |
| `cerberus_tempo_exemplar_failures_total`      | counter            | `stage`                                                                                     |
| `cerberus_solver_estimate_drift_ratio`        | histogram          | `cerberus_actuals_source`                                                                   |
| `cerberus_solver_estimate_drift_alerts_total` | counter            | `cerberus_actuals_source`                                                                   |

The four `cerberus_route_*` / `cerberus_routed_*` instruments describe the
failure-driven route memo and the A/B route outcome; the two
`cerberus_solver_estimate_drift_*` instruments describe the
predicted-vs-actual drift tracker. `docs/solver.md` covers both.

Two further instruments live under their own meter scopes:
`cerberus_admit_rejected_total` (counter; `cerberus_ql`, `budget`,
`reason`) under `internal/api/admit` counts requests the per-handler
concurrency cap refused, and `cerberus_solver_parallelism_clamped_total`
(counter, no attributes) under `internal/solver` counts routed requests
whose shard parallelism was clamped below the configured `P`.

`cerberus_tempo_exemplar_failures_total` counts Tempo `/api/metrics/query_range` (and its gRPC `MetricsQueryRange` counterpart) exemplar-enrichment failures, split by which half of the best-effort exemplar attach failed: `stage="emit"` for a `chsql.EmitMetricsExemplars` render failure, `stage="execute"` for a ClickHouse query failure on the rendered SQL. Both failures still return the matrix response with an empty `exemplars` array — the same wire shape as a window with genuinely no exemplars — so this counter is the only way to notice a systematic exemplar outage without reading logs.

`cerberus_optimizer_fixpoint_cap_hits_total` counts optimizer `FixedPoint` batches that exhausted their iteration cap with a rule still reporting change, by batch name. Every production batch converges well inside its cap, so a non-zero rate is a rule bug (two rules undoing each other, or one that always reports a change) that otherwise shows only as a slower `optimize` stage; the same event is logged at WARN as `optimizer: fixpoint batch hit its iteration cap without converging` with the batch name.

query-duration histogram. It is deprecated and emitted alongside
`cerberus_queries_duration_exp_hist`, with its original classic
explicit-bucket aggregation, so an existing dashboard or alert rule keeps
working across the upgrade. Move queries to the new name; the removal of
the legacy instrument is tracked in cerberus issue #3569.
`cerberus_queries_duration_exp_hist` is collected as a native/exponential
histogram (cerberus issue #3170), not the classic explicit-bucket shape
`cerberus_pipeline_stage_duration_seconds` and the other histograms above
still use — the `_exp_hist` suffix is required, not cosmetic: cerberus's
own PromQL read path has no wire-format way to tell a native histogram
from a classic one, so it routes on that suffix alone
(`schema.Metrics.ExpHistogramSuffix`). A PromQL query against it has no
`_bucket` series or `le` label:
`histogram_quantile(0.95, sum by (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))`
is the whole expression, not `sum by (le, cerberus_ql) (rate(..._bucket[5m]))`.

#### ClickHouse connection lifecycle

A second instrument set lives in `internal/chclient`, under the
`github.com/tsouza/cerberus/internal/chclient` meter scope. It describes
the connection pool rather than the query pipeline.

| Metric                              | Type    | Attributes | Meaning                                                                    |
| ----------------------------------- | ------- | ---------- | -------------------------------------------------------------------------- |
| `cerberus_ch_conn_dials_total`      | counter | —          | TCP connections opened to ClickHouse.                                      |
| `cerberus_ch_cursor_teardown_total` | counter | `outcome`  | Cursor teardowns, split `drained` / `abandoned` / `cancelled`.             |
| `cerberus_ch_conn_open`             | gauge   | `pool`     | Pooled connections open (busy + idle), read live from the driver.          |
| `cerberus_ch_conn_idle`             | gauge   | `pool`     | Pooled connections idle and reusable.                                      |

A dial happens only when the pool has no warm connection to hand back, so
`rate(cerberus_ch_conn_dials_total[5m])` is the bottom-line cost of
connection churn — but it does not say WHO paid it. Three destroyers
share the bill: a query cancelled mid-flight (the driver tears the socket
down rather than leave undrained bytes on the wire), a cursor teardown
that outran its drain budget, and the driver's own age eviction at
`CERBERUS_CH_CONN_MAX_LIFETIME`. The teardown counter names cerberus's
own share directly, and its three outcomes are the three fates a pooled
connection can meet:

- `drained` — `Close` returned inside the drain budget on a still-live
  query context, so teardown ran on cerberus's terms rather than being
  aborted. It does **not** assert that the driver reached end-of-stream,
  and so does not by itself promise a pool release: an early `Close` on a
  partially-read cursor lands here too, and the driver destroys that
  socket instead of pooling it.
- `abandoned` — the cursor did not finish inside the drain budget, so
  teardown cancelled it. Bounded by design: an unread remainder must not
  pin a pool slot, and paying a dial is the cheaper of the two.
- `cancelled` — the query context was already dead when teardown began
  (client hang-up, request deadline, an upstream cancellation). The
  socket was destroyed by that cancellation, not by cerberus's budget.

Separating `cancelled` from `abandoned` is what makes the counter
actionable. Both destroy a socket, but only `abandoned` is a cerberus
tuning signal — a rising `abandoned` rate says the drain budget is too
tight for the result sizes in flight, while a rising `cancelled` rate
says clients are walking away and no budget change will help. Summed,
the two account for cerberus's contribution to churn, and the residual
against `cerberus_ch_conn_dials_total` is the driver's own age eviction.

The `pool` attribute exists because the connection gauges are
process-wide while the pools are not: alongside the long-lived `serving`
pool, startup opens five short-lived bootstrap pools — `version-probe`,
`tsgrid-probe`, `result-cache-probe`, `query-workload-probe` and
`schema-apply`. The gauges are
observable instruments keyed by attribute set, so without the label
those pools would collapse onto one series with a single registration
silently winning — `cerberus_ch_conn_open{pool="serving"}` is the one to
alert on, and a bootstrap pool still visible long after startup is
itself the finding.

Every counter is zero-initialised at startup — including one series per
teardown outcome — so a replica that has never churned a connection
exports a flat `0` rather than "No data". The seeding is why the
MeterProvider is installed before any instrument is minted: OTel's
package-global provider is a delegating shim, and a synchronous `Add`
recorded before delegation is dropped with no buffering and no error.

#### ClickHouse circuit breaker

The same `internal/chclient` meter scope carries the circuit breaker's own
instruments. One breaker fronts each logical head, so every series is keyed on
`head` (`prom` / `loki` / `tempo` / `probe`).

| Metric                                           | Type    | Attributes      | Meaning                                                                          |
| ------------------------------------------------ | ------- | --------------- | -------------------------------------------------------------------------------- |
| `cerberus_ch_breaker_state`                      | gauge   | `head`          | Lifecycle phase: `0` closed, `1` open, `2` half-open.                            |
| `cerberus_ch_breaker_trips_total`                | counter | `head`, `cause` | Cumulative CLOSED→OPEN trips, split by why the breaker tripped.                  |
| `cerberus_ch_breaker_statement_rejections_total` | counter | `head`          | ClickHouse rejections classified as statement-scoped and deliberately uncounted. |

A trip is the highest-blast-radius event cerberus has — it fast-fails every
query behind that head with a 503. It does not by itself flip `/readyz`:
readiness pings through a dedicated `probe` breaker, reports the tripped head
in its `heads` object, and goes 503 only when EVERY enabled head is open — so
under combined mode one tripped head leaves the pod in its Service, while
under split mode, where a Deployment serves one head, that head's trip *is*
exhaustion. `cause` names which kind of backend trouble caused the trip:

- `no-server-answer` — ClickHouse never answered: a refused dial, a dropped
  connection, a socket timeout, an unrecognised driver failure.
- `server-condition` — ClickHouse answered, but with a code naming a condition
  of the server or its cluster (saturation, coordination loss, allocator
  failure). The trip's WARN log additionally names the code and its symbolic
  name.

Statement-scoped rejections — a syntax error, an unknown column, an
unsatisfiable setting combination — never trip the breaker. They are
counted separately, and the pair is what makes triage decidable:

- rising `statement_rejections_total` with a flat `trips_total` — cerberus is
  emitting SQL ClickHouse declines. The backend is fine; look at the query
  pipeline.
- rising `trips_total` — the backend is in trouble; `cause` says which kind.

#### Failure classification

`result` alone answers "did the query fail", never "whose fault was
it" — and those demand opposite responses. A 4xx means the caller sent
something cerberus cannot answer; a 5xx means cerberus could not answer
something valid. `cerberus_error_reason` and `cerberus_status_class`
carry that distinction on the counter. Both are closed enums, so the
label cardinality is fixed:

| `cerberus_error_reason` | Meaning                                                                                                                                                                               |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `none`                  | The query succeeded. Carried so ok and error series share one label set.                                                                                                              |
| `bad_request`           | Not answerable as written — unparseable, unsupported, or an unevaluable shape. Caller has to change it.                                                                               |
| `backend_unavailable`   | ClickHouse could not be reached or refused the work, or a head's circuit breaker is open.                                                                                             |
| `resource_exhausted`    | A per-query budget refused the work: the sample budget, the wide-projection byte budget, or ClickHouse's own memory-limit abort. The query asked for too much; the server is healthy. |
| `timeout`               | The request ran out of time — the ClickHouse `max_execution_time` cap, or the request's own deadline.                                                                                 |
| `canceled`              | The caller went away before the answer was ready. Nothing failed and nothing ran out of time; the client stopped waiting. Never worth acting on.                                      |
| `internal`              | A defect in cerberus — a recovered panic or an unclassified 5xx. Worth a page.                                                                                                        |

`cerberus_status_class` is derived purely from the response's status
family. `cerberus_error_reason` is not: every head answers a query
wall-clock timeout with **503** and a per-query budget refusal with
**422**, so the handler that classified the failure records the reason
directly (`telemetry.SetReason`, on a request-scoped cell the query
middleware installs) and the middleware prefers it over its
status-derived default. A handler that records nothing is classified from
its status. A recovered panic stays pinned to `internal` whatever the
handler recorded.

A client cancellation is answered **499** by Tempo and **503** by
Prometheus and Loki (upstream's own `errorCanceled` envelope). The reason
label is `canceled` on every head regardless: `httperr.TelemetryReason`
names a `context.Canceled` failure, and the two constructors that restate
the message in upstream's wording carry the reason on the error itself.

A cancellation still counts as `result="error"`, because the query was
not answered. It is the one error reason expected in normal operation
rather than a fault: Grafana cancels every in-flight request on a panel
re-render, a query edit or a tab switch. Exclude it before alerting on an
error ratio, or a healthy dashboard reads as an incident.

Note that `cerberus_status_class` is NOT unified the same way — a
cancellation stays `4xx` on Tempo and `5xx` on Prometheus/Loki, because
that label reports the status actually sent and those statuses are fixed
by the compatibility contract. Alerts keyed on
`cerberus_status_class="5xx"` should exclude
`cerberus_error_reason="canceled"` for that reason.

Admission-control rejections are not in this counter at all: the
limiter middleware sits OUTSIDE `telemetry.QueryMiddleware`, so a
saturation rejection never enters the query pipeline and never lands in
`cerberus_queries_total`.

`cerberus_status_class` is the HTTP family (`1xx` … `5xx`, or `unknown`
for a code outside them). Alert on
`cerberus_queries_total{cerberus_status_class="5xx"}` for "cerberus is
broken" and leave `4xx` to a separate, lower-urgency rule.

Both labels reach the surface on the provisioned Cerberus dashboard,
which carries an "Errors by status class" panel
(`sum by (cerberus_status_class) (rate(cerberus_queries_total{result="error"}[5m]))`)
beside an "Errors by failure reason" panel over
`cerberus_error_reason`. The pair is what the plain "Error rate by
language" panel above them cannot answer: that panel says a head is
failing, these two say whether the caller or the gateway is at fault
and which failure mode is behind it. The dashboard ships in three
hand-maintained copies — the k3d source at
`test/e2e/grafana/dashboards/cerberus.json`, the ConfigMap literal in
`test/e2e/k3s/grafana-dashboards.yaml` that the k3d stack actually
serves, and the compose copy at
`test/e2e/grafana/compose/dashboards/cerberus.json` — held in lockstep
by `test/regression/grafana_dashboard_copy_parity_test.go`, so a panel
added to one is a failure until it is added to all three.

#### Duration buckets

`cerberus_queries_duration_exp_hist` is aggregated as a base-2
exponential histogram by an SDK view (`internal/telemetry/telemetry.go`),
so it has no explicit ladder. The classic histograms —
`cerberus_pipeline_stage_duration_seconds` and the rows/bytes/rules
instruments — carry explicit boundaries (`StageDurationBoundaries`, … in
`internal/telemetry/metrics.go`); the stage ladder reaches the minute
scale because `execute` carries the ClickHouse round trip.

#### Stage attribution

`cerberus_pipeline_stage_duration_seconds` carries `cerberus_ql`, so a
slow `parse` or `execute` is attributable to a language in a process that
serves all three heads. The stages (`parse` / `lower` / `optimize` / `emit` /
`execute`) do not sum to the request duration: response materialisation and
the row drain sit outside them, and on the streaming paths the drain runs
while the response is being written. Use `cerberus_queries_duration_exp_hist` for
the end-to-end number and the stage histogram for the breakdown within
the engine.

### SDK error reporting

The OTel SDK reports its own failures — a batch it could not export, a
collect or shutdown error — through a process-global error handler
instead of returning them. Cerberus routes that handler into the
structured logger at `WARN` with `component=otel` and the native error
under `err`:

```json
{"level":"WARN","msg":"otel: self-telemetry pipeline error","component":"otel","err":"..."}
```

The fixed message plus the `component` field make the rate of these
expressible as an alert.

### Query failure log line

`internal/engine` logs one `WARN`-level, structured line at every
seam where a dispatched query's outcome resolves to something other
than a clean finish — the eager `Query` / `QueryPlan` path (every PromQL
instant query, every LogQL query, every eager TraceQL query), the
streaming `QueryCursor` / `QueryPlanCursor` path's open-time and
mid-drain failures (the Prom `/query_range` matrix path), and the
dormant sharded-pushdown route-B path. It never fires for a query that
completes cleanly — logging every successful query would be a
production log-volume problem, not an observability improvement — and
it fires unconditionally, independent of whether the corpus reconciler
is even registered, so the gap does not silently reopen on a deployment
that never turned the corpus on.

```json
{"level":"WARN","msg":"cerberus query failed","cerberus_ql":"promql","shape_id":"cerb:project;agg=1;rw","decision_reason":"","exit_class":"timeout","duration_ms":30012,"query_id":"a1b2c3-...-4","err":"engine: execute: query execution timeout exceeded"}
```

The classification (`exit_class`) reuses the exact same predicates the
corpus already uses to classify a cerberus-side outcome
(`sample_budget` / `byte_budget` / `breaker` / `oom`), widened with the
two classes only a synchronous caller can see (`timeout` / `canceled`)
and the honest `error` floor for a ClickHouse exception that matches
none of the above — so the log line and the corpus row for the SAME
failure can never name a different cause. `shape_id` and
`decision_reason` are empty rather than fabricated at the one call site
with no plan/decision in scope (a mid-drain failure reported back from
the handler, which owns the drain and has no plan reference).
`duration_ms` and `query_id` are logged because they are already known
at every call site at zero extra cost; the per-query ClickHouse memory
usage the corpus's `system.query_log` join eventually learns is
deliberately NOT fetched here — doing so would mean an extra ClickHouse
round trip on every failure, which this line exists to avoid, not add.

Deliberately `WARN`, not `ERROR`: most non-`ok` exits here are not
cerberus defects (a caller cancelling, a query that ran into its own
configured budget) rather than something "worth a page" the way this
page's `ERROR` vocabulary above describes. A genuine cerberus defect
still reaches `ERROR` through its own existing site (the recovered-panic
log in `telemetry.QueryMiddleware`); this line's job is visibility, not
alerting.

### Resource attributes

Every exported span and metric carries:

| Attribute             | Source                                                                                                  |
| --------------------- | ------------------------------------------------------------------------------------------------------- |
| `service.name`        | Hard-coded to `cerberus`.                                                                               |
| `service.version`     | The `Version` var in `cmd/cerberus` (set to `dev` by default, injected at release time via `-ldflags`). |
| `service.instance.id` | `os.Hostname()`, falling back to a random 16-byte hex string.                                           |

### Shutdown

On SIGINT / SIGTERM, cerberus:

1. Stops accepting new HTTP connections (`http.Server.Shutdown`).
2. Flushes pending OTLP batches and tears the providers down
   (`Providers.Shutdown`) inside the same 10s shutdown context.

If the collector is unreachable during shutdown the OTLP exporter logs
the error and returns — cerberus still exits cleanly rather than
hanging.

### Disabling telemetry

Leave `CERBERUS_OTLP_ENDPOINT` unset (or set to the empty string). The
process installs no-op providers; otelhttp middleware still wraps the
mux but every span is silently dropped.

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [observability.background.md](observability.background.md).
