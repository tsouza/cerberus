# Observability

Cerberus is its own first-class observability customer: it queries OTel-CH
metrics + logs + traces, and it ships the same shape of telemetry back into
that store so a self-dashboard works against a running cluster.

The full self-observability stack lands across **RC4**:

| Item                                                                                                  | Status  | Notes                                    |
| ----------------------------------------------------------------------------------------------------- | ------- | ---------------------------------------- |
| R4.1 — slog quality pass + env-configurable format / level                                          | landed  | this doc                                 |
| R4.2 — `otelhttp.NewHandler` wraps the Prom / Loki / Tempo handlers                                 | planned | adds one span per HTTP request           |
| R4.3 — Custom spans around parse / lower / optimize / emit / execute                                | planned | pipeline stage timings                   |
| R4.4 — Self-metrics: request count + latency + CH roundtrip + in-flight gauge                       | planned | HPA-consumable                           |
| R4.5 — OTLP exporters wired (`CERBERUS_OTEL_ENDPOINT` / `_INSECURE` / `_SAMPLER` / `_SERVICE_NAME`) | planned | graceful no-op when endpoint unreachable |
| R4.6 — `deploy/k3s/otel-collector.yaml` + `deploy/grafana/dashboards/cerberus-self.json`            | planned | wires the export path end-to-end         |

This page only covers what has already shipped. The remaining rows are
expanded as the milestones land.

## Logging (R4.1, shipped)

Cerberus uses the standard library's [`log/slog`](https://pkg.go.dev/log/slog)
for structured logging. Two env vars steer the handler:

| Env var               | Default | Allowed values                                       | Effect                                   |
| --------------------- | ------- | ---------------------------------------------------- | ---------------------------------------- |
| `CERBERUS_LOG_FORMAT` | `text`  | `text`, `json` (case-insensitive)                    | slog handler kind                        |
| `CERBERUS_LOG_LEVEL`  | `info`  | `debug`, `info`, `warn`, `error` (+ `warning` alias) | Minimum level retained by the handler    |

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
| `err`                          | error  | Native `error` value — slog encodes via `.Error()` for json + `%v` for text                              |
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
