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
`test/e2e/grafana/dashboards/cerberus-self.json` wire the full export path
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
  by the peer in the Loki `/tail` handler).
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

| Variable                                      | Default                      | Effect                                             |
| --------------------------------------------- | ---------------------------- | -------------------------------------------------- |
| `CERBERUS_SCHEMA_METRICS_GAUGE_TABLE`         | `otel_metrics_gauge`         | Gauge-metrics table name.                          |
| `CERBERUS_SCHEMA_METRICS_SUM_TABLE`           | `otel_metrics_sum`           | Sum / counter metrics table name.                  |
| `CERBERUS_SCHEMA_METRICS_HISTOGRAM_TABLE`     | `otel_metrics_histogram`     | Classic histogram metrics table name.              |
| `CERBERUS_SCHEMA_METRICS_EXP_HISTOGRAM_TABLE` | `otel_metrics_exp_histogram` | Exponential / native histogram metrics table name. |
| `CERBERUS_SCHEMA_METRICS_SUMMARY_TABLE`       | `otel_metrics_summary`       | Summary metrics table name.                        |
| `CERBERUS_SCHEMA_LOGS_TABLE`                  | `otel_logs`                  | Logs table name read by the Loki API.              |
| `CERBERUS_SCHEMA_TRACES_TABLE`                | `otel_traces`                | Spans table name read by the Tempo API.            |

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
| `CERBERUS_OTLP_TIMEOUT`  | `10s`   | Per-request OTLP roundtrip timeout (`time.ParseDuration` syntax).                                |

Standard OTel SDK env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_RESOURCE_ATTRIBUTES`, …) are read by
the SDK on top of the cerberus-specific knobs above. When both are set,
the `CERBERUS_OTLP_*` value wins for that field because cerberus passes
it explicitly to the exporter constructor.

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
