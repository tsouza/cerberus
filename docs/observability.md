# Observability

Cerberus is its own first-class observability customer: it queries OTel-CH
metrics + logs + traces, and it ships the same shape of telemetry back into
that store so a self-dashboard works against a running cluster.

The full self-observability stack lands across **RC4**:

| Item                                                                                                  | Status  | Notes                                    |
| ----------------------------------------------------------------------------------------------------- | ------- | ---------------------------------------- |
| R4.1 — slog quality pass + env-configurable format / level                                            | landed  | this doc                                 |
| R4.2 — `otelhttp.NewHandler` wraps the Prom / Loki / Tempo handlers                                   | planned | adds one span per HTTP request           |
| R4.3 — Custom spans around parse / lower / optimize / emit / execute                                  | planned | pipeline stage timings                   |
| R4.4 — Self-metrics: request count + latency + CH roundtrip + in-flight gauge                         | planned | HPA-consumable                           |
| R4.5 — OTLP exporters wired (`CERBERUS_OTLP_ENDPOINT` / `_INSECURE` / `_HEADERS` / `_TIMEOUT`)        | landed  | graceful no-op when endpoint unreachable |
| R4.6 — `deploy/k3s/otel-collector.yaml` + `deploy/grafana/dashboards/cerberus-self.json`              | planned | wires the export path end-to-end         |

This page only covers what has already shipped. The remaining rows are
expanded as the milestones land.

## Logging (R4.1, shipped)

Cerberus uses the standard library's [`log/slog`](https://pkg.go.dev/log/slog)
for structured logging. Logs are written as an event stream to `stderr`
(factor XI of the [12-factor methodology](12factor.md#factor-xi--logs))
— cerberus never owns the sink. Two env vars steer the handler:

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
time=2026-05-13T10:14:01.000Z level=INFO msg="cerberus starting" version=v1.0.0-RC4 http_addr=:8080 ch_addr=clickhouse:9000 ch_db=otel log_format=text log_level=INFO

# CERBERUS_LOG_FORMAT=json
{"time":"2026-05-13T10:14:01Z","level":"INFO","msg":"cerberus starting","version":"v1.0.0-RC4","http_addr":":8080","ch_addr":"clickhouse:9000","ch_db":"otel","log_format":"json","log_level":"INFO"}
```

## Tracing + metrics export (R4.5, shipped)

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
