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

The OTLP sink is enabled whenever `CERBERUS_OTLP_ENDPOINT` is set;
unset means no-op bridge (stderr-only fallback). Two env vars steer the
stderr-side handler:

| Env var               | Default | Allowed values                                       | Effect                                |
| --------------------- | ------- | ---------------------------------------------------- | ------------------------------------- |
| `CERBERUS_LOG_FORMAT` | `text`  | `text`, `json` (case-insensitive)                    | slog handler kind                     |
| `CERBERUS_LOG_LEVEL`  | `info`  | `debug`, `info`, `warn`, `error` (+ `warning` alias) | Minimum level retained by the handler |

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
  selectable.
- **`Error`** — handler-level failures that produce a 5xx (CH connection
  reset, plan emission internal error). The bridge to alerting.

### Attribute conventions

The codebase follows a small set of consistent keys so a future query
across `otel_logs` can filter without guessing:

| Key                            | Type   | Notes                                                                                                      |
| ------------------------------ | ------ | ---------------------------------------------------------------------------------------------------------- |
| `api`                          | string | `prom` / `loki` / `tempo`, set on the per-handler logger via `.With("api", ...)` in `cmd/cerberus/main.go` |
| `promql` / `logql` / `traceql` | string | The query text as received                                                                                 |
| `sql`                          | string | The emitted ClickHouse SQL                                                                                 |
| `args`                         | []any  | Parameterised SQL args                                                                                     |
| `err`                          | error  | Native `error` value — slog encodes via `.Error()` for json + `%v` for text                                |
| `trace_id`                     | string | Tempo `traceByID` handler only                                                                             |
| `tag`                          | string | Tempo tag-values handler                                                                                   |

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

One related opt-in knob is a boolean rather than a table-name override:

| Variable                           | Default | Effect                                                                                                                                                                                                                                                                          |
| ---------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_SCHEMA_TRACES_TS_LOOKUP` | `false` | When truthy, the Tempo trace-by-ID path window-prunes the spans scan through the OTel-CH `<spans>_trace_id_ts` lookup MV. Enable only after confirming that MV is populated; the lookup-table name derives from the (possibly overridden) spans table as `<spans>_trace_id_ts`. |

The active ClickHouse **database** is set by `CERBERUS_CH_DATABASE`
(default `otel`) — that single knob covers both the connection's
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

| Variable                 | Default | Meaning                                                                                          |
| ------------------------ | ------- | ------------------------------------------------------------------------------------------------ |
| `CERBERUS_OTLP_ENDPOINT` | `""`    | gRPC target, e.g. `otel-collector.observability.svc:4317`. Empty disables both exporters.        |
| `CERBERUS_OTLP_INSECURE` | `false` | When `true`, dial the endpoint without TLS. Use for local dev / k3d only.                        |
| `CERBERUS_OTLP_HEADERS`  | `""`    | Comma-separated `key=value` list attached as gRPC metadata (e.g. `authorization=Bearer abc...`). |
| `CERBERUS_OTLP_TIMEOUT`  | `10s`   | Per-request OTLP roundtrip timeout.                                                              |

Standard OTel SDK env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_RESOURCE_ATTRIBUTES`, …) are read by
the SDK on top of the cerberus-specific knobs above. When both are set,
the `CERBERUS_OTLP_*` value wins for that field because cerberus passes
it explicitly to the exporter constructor.

### Self-metrics

The instrument set lives in `internal/telemetry`. Names, units and
attribute keys are a public contract — dashboards and alert rules
reference them verbatim, and `internal/telemetry/contract_test.go` pins
each one so a rename cannot ship silently.

| Metric                                     | Type      | Attributes                                                                                  |
| ------------------------------------------ | --------- | ------------------------------------------------------------------------------------------- |
| `cerberus_queries_total`                   | counter   | `cerberus_ql`, `cerberus_route`, `result`, `cerberus_error_reason`, `cerberus_status_class` |
| `cerberus_queries_duration_seconds`        | histogram | `cerberus_ql`, `cerberus_route`, `result`                                                   |
| `cerberus_pipeline_stage_duration_seconds` | histogram | `stage`, `cerberus_ql`                                                                      |
| `cerberus_optimizer_rules_applied`         | histogram | —                                                                                           |
| `cerberus_clickhouse_rows_read`            | histogram | `cerberus_ql`                                                                               |
| `cerberus_clickhouse_bytes_read`           | histogram | `cerberus_ql`                                                                               |
| `cerberus_query_inflight`                  | gauge     | `cerberus_ql`                                                                               |

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

- `drained` — the cursor reached end-of-stream inside its budget while
  the query context was still live. This is the only outcome that
  returns the connection to the pool; everything else costs a redial.
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
pool, startup opens short-lived bootstrap pools for the version probe,
the ts-grid capability probe and the schema apply. The gauges are
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

#### Failure classification

`result` alone answers "did the query fail", never "whose fault was
it" — and those demand opposite responses. A 4xx means the caller sent
something cerberus cannot answer; a 5xx means cerberus could not answer
something valid. `cerberus_error_reason` and `cerberus_status_class`
carry that distinction on the counter. Both are closed enums derived
from the response's status family, never from an error string or a raw
status code, so the label cardinality is fixed:

| `cerberus_error_reason` | Meaning                                                                                                       |
| ----------------------- | ------------------------------------------------------------------------------------------------------------- |
| `none`                  | The query succeeded. Carried so ok and error series share one label set.                                      |
| `bad_request`           | Not answerable as written — unparseable, unsupported, or over a per-query budget. Caller has to change it.    |
| `backend_unavailable`   | ClickHouse could not be reached or refused the work.                                                          |
| `resource_exhausted`    | The server refused for capacity reasons: rate limited, out of storage.                                        |
| `timeout`               | The request ran out of time, on either side of the gateway.                                                   |
| `internal`              | A defect in cerberus — a recovered panic or an unclassified 5xx. Worth a page.                                |

Admission-control rejections are not in this counter at all: the
limiter middleware sits OUTSIDE `telemetry.QueryMiddleware`, so a
saturation rejection never enters the query pipeline and never lands in
`cerberus_queries_total`.

`cerberus_status_class` is the HTTP family (`1xx` … `5xx`, or `unknown`
for a code outside them). Alert on
`cerberus_queries_total{cerberus_status_class="5xx"}` for "cerberus is
broken" and leave `4xx` to a separate, lower-urgency rule.

#### Duration buckets

The duration ladders are explicit (the SDK default is
millisecond-shaped, and these instruments record seconds). Both reach
the minute scale on purpose: a gateway fronting an analytical database
can serve a request slower than any single-digit-second bound, and
every observation past the top FINITE bucket is unresolvable — the
`+Inf` bucket has no upper bound for `histogram_quantile` to
interpolate against, so once it holds more than 5% of the observations
p95 and p99 both collapse onto the top finite bound and the slow tail
disappears exactly where an investigation needs it. `execute` carries
the ClickHouse round trip, so the stage ladder has to reach as far up
as the query ladder.

#### Stage attribution

`cerberus_pipeline_stage_duration_seconds` carries `cerberus_ql`
because one process serves all three heads: without it, a slow `parse`
or `execute` cannot be attributed to a language, and the metric would
only be separable in a deployment that happened to split the heads into
separate processes — a property of that deployment, not of the metric.
The stages (`parse` / `lower` / `optimize` / `emit` / `execute`) do not
sum to the request duration: response materialisation and the row drain
sit outside them, and on the streaming paths the drain runs while the
response is being written. Use `cerberus_queries_duration_seconds` for
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

`WARN` rather than `INFO` because the condition is actionable and
degrading: an export failure means the gateway has lost its OWN
telemetry, which is precisely what an operator reaches for when
diagnosing a failure. The fixed message plus the `component` field make
the rate of these expressible as an alert.

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
