# Observability

Cerberus is its own first-class observability customer: it queries OTel-CH
metrics + logs + traces, and it ships the same shape of telemetry back into
that store. A self-dashboard then works against any running cluster — the
dogfood loop.

## What cerberus instruments

| Signal                  | Source                                                                                     |
| ----------------------- | ------------------------------------------------------------------------------------------ |
| Structured logs         | `log/slog` (stdlib structured logger, JSON / text via env)                                 |
| HTTP server spans       | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` around the API mux         |
| Pipeline spans          | manual spans around parse / lower / optimize / emit / execute                              |
| Self-metrics            | OTel SDK metrics: query count + latency, per-stage latency, CH rows/bytes, in-flight gauge |
| OTLP export             | `go.opentelemetry.io/otel/exporters/otlp/{otlpmetricgrpc,otlptracegrpc}`                   |
| Collector + dashboard   | `deploy/k3s/otel-collector.yaml` + `deploy/grafana/dashboards/cerberus-self.json`          |

## Structured logging

Cerberus uses the standard library's [`log/slog`](https://pkg.go.dev/log/slog)
for structured logging. Two env vars steer the handler:

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

### Level vocabulary

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

The codebase follows a small set of consistent keys so a query against
`otel_logs` can filter without guessing:

| Key                            | Type   | Notes                                                                                |
| ------------------------------ | ------ | ------------------------------------------------------------------------------------ |
| `api`                          | string | `prom` / `loki` / `tempo`, set on the per-handler logger via `.With("api", ...)`     |
| `promql` / `logql` / `traceql` | string | The query text as received                                                           |
| `sql`                          | string | The emitted ClickHouse SQL                                                           |
| `args`                         | []any  | Parameterised SQL args                                                               |
| `err`                          | error  | Native `error` value — slog encodes via `.Error()` for json + `%v` for text          |
| `trace_id`                     | string | Tempo `traceByID` handler only                                                       |
| `tag`                          | string | Tempo tag-values handler                                                             |

Always pass the native `error` as `"err", err` rather than `err.Error()` so
a future `slog.Handler` middleware can branch on `errors.As` / `errors.Is`.

### Examples

```text
# CERBERUS_LOG_FORMAT=text CERBERUS_LOG_LEVEL=info (defaults)
time=2026-05-13T10:14:01.000Z level=INFO msg="cerberus starting" version=v1.0.0 http_addr=:8080 ch_addr=clickhouse:9000 ch_db=otel log_format=text log_level=INFO

# CERBERUS_LOG_FORMAT=json
{"time":"2026-05-13T10:14:01Z","level":"INFO","msg":"cerberus starting","version":"v1.0.0","http_addr":":8080","ch_addr":"clickhouse:9000","ch_db":"otel","log_format":"json","log_level":"INFO"}
```

## Traces

Cerberus emits two layers of spans for every query:

- **HTTP server spans.** The API mux is wrapped with `otelhttp.NewHandler`, so
  every Prom / Loki / Tempo request becomes a server span whose name is the
  matched `http.ServeMux` pattern (e.g. `GET /api/v1/query`). Unmatched
  requests fall back to `HTTP <METHOD>` to keep span cardinality bounded by
  the route set rather than the URL space. The `/healthz` and `/readyz`
  probes bypass `otelhttp` so multi-Hz Kubernetes probing doesn't flood the
  trace backend with no-op spans.
- **Pipeline spans.** Manual spans wrap each pipeline stage —
  `parse` / `lower` / `optimize` / `emit` / `execute` — as a child of the
  HTTP request span. The result is a clean stage breakdown in any trace
  viewer: parse cost, lowering cost, optimizer cost, SQL emission cost, and
  ClickHouse roundtrip cost, all attributable to one user query.

Incoming `traceparent` / `baggage` headers are honored: the W3C TraceContext
and Baggage propagators are installed as process globals so a Grafana span
becomes the parent of cerberus's HTTP span, which becomes the parent of the
pipeline spans.

## Self-metrics

The OTel SDK metrics path emits a small set of cerberus-specific counters,
histograms, and gauges. They are HPA-consumable: the in-flight gauge is the
recommended autoscaler signal.

| Instrument                                   | Kind      | Labels                                | Notes                                                       |
| -------------------------------------------- | --------- | ------------------------------------- | ----------------------------------------------------------- |
| `cerberus_queries_total`                     | counter   | `api`, `result` (`ok` / `error`)      | One increment per Prom / Loki / Tempo query handled         |
| `cerberus_queries_duration_seconds`          | histogram | `api`, `result`                       | Wall-clock per query, end-to-end                            |
| `cerberus_pipeline_stage_duration_seconds`   | histogram | `api`, `stage`                        | Per-stage timing: parse / lower / optimize / emit / execute |
| `cerberus_ch_rows_read_total`                | counter   | `api`                                 | Rows read from ClickHouse, summed over the query            |
| `cerberus_ch_bytes_read_total`               | counter   | `api`                                 | Bytes read from ClickHouse, summed over the query           |
| `cerberus_http_requests_in_flight`           | gauge     | `route`                               | Currently-executing requests (HPA target)                   |

## OTLP exporters

Cerberus exports its own logs + metrics + traces via OTLP/gRPC. Configuration
is env-driven (see `internal/config/`):

| Variable                     | Default                        | Effect                                                                             |
| ---------------------------- | ------------------------------ | ---------------------------------------------------------------------------------- |
| `CERBERUS_OTEL_ENDPOINT`     | empty (export disabled)        | OTLP/gRPC endpoint (host:port). Empty makes the process a zero-collector binary.   |
| `CERBERUS_OTEL_INSECURE`     | `false`                        | Set `true` for in-cluster plaintext OTLP. The k3s manifests default to `true`.     |
| `CERBERUS_OTEL_SERVICE_NAME` | `cerberus`                     | Becomes the OTel-CH `ServiceName` column. The Grafana dashboard filters on it.     |
| `CERBERUS_OTEL_SAMPLER`      | `parentbased_traceidratio:0.1` | Tracing sampler spec. `always_on` / `always_off` / `parentbased_traceidratio:<r>`. |

Setting `CERBERUS_OTEL_ENDPOINT=""` reduces cerberus to a
zero-collector-dependency binary: no OTLP exporters are constructed, the
metric SDK uses the no-op meter provider, and pipeline spans are recorded
against a no-op tracer. HTTP handlers continue to serve PromQL / LogQL /
TraceQL exactly as before. The self-dashboard simply shows no data — it is
not an error. This is the supported posture for users who want cerberus's
query gateway behaviour but not its self-observability stack.

When the endpoint is set but unreachable at startup, the exporters install
in a queued state rather than failing the process. Cerberus keeps serving
queries; the OTel SDK retries delivery in the background. Telemetry loss is
a soft failure by design.

## Dogfood loop

Cerberus ships its own telemetry into the **same** `otel_metrics_*`,
`otel_logs`, and `otel_traces*` tables it serves over the Prometheus /
Loki / Tempo HTTP APIs. The result is a closed dogfood loop: cerberus
observes itself by querying its own metrics out of ClickHouse — and
Grafana, sitting on top, never sees anything but cerberus's three upstream
APIs.

```text
            ┌─────────────────────────────────────────────────────┐
            │                                                     │
            ▼                                                     │
     ┌────────────┐   PromQL / LogQL / TraceQL              ┌─────┴────┐
     │  Grafana   │ ──────────────────────────────────────▶ │ cerberus │
     └────────────┘                                          └─────┬────┘
                                                                   │
                                                                   │ ClickHouse SQL
                                                                   ▼
                                                            ┌────────────┐
                                                            │ ClickHouse │
                                                            └─────┬──────┘
                                                                  ▲
                                                                  │ clickhouseexporter
                                                                  │
                                                            ┌─────┴──────┐
                                            OTLP/gRPC       │ OTel       │
                            ┌──────────────────────────────▶│ Collector  │
                            │                                │ (gateway)  │
                            │                                └────────────┘
                      ┌─────┴────┐
                      │ cerberus │   (OTLP exporter)
                      └──────────┘
```

Cerberus emits OTLP to the in-cluster OTel Collector gateway. The gateway
writes to ClickHouse via the OTel-Contrib `clickhouseexporter` — the same
exporter whose DDL templates cerberus's `internal/schema/` package imports
as the schema source-of-truth, so the schema cannot drift between the
writer and the reader. Grafana, provisioned with the `Cerberus-Prometheus`
datasource, then queries cerberus's own metrics back through cerberus.

## OTel Collector — `deploy/k3s/otel-collector.yaml`

Two collector roles run in-cluster (one image, two configs):

- **Gateway** (Deployment). Accepts OTLP from cerberus and the agent
  DaemonSet, runs `k8s_cluster` + `k8s_events` for cluster-wide signals,
  writes everything to ClickHouse via `clickhouseexporter`
  (`create_schema: true`).
- **Agent** (DaemonSet). Runs `kubeletstats` (node/pod/container metrics)
  and `filelog` (container stdout/stderr from `/var/log/pods/*`), forwards
  OTLP to the gateway.

Both use `otel/opentelemetry-collector-contrib:0.116.1` — the Contrib
distribution, **not** the upstream Apache 2 core image, because we need
`clickhouseexporter`, `kubeletstats`, `filelog`, `k8s_cluster`, and
`k8s_events`. The image version is kept in lockstep with the OTel-CH
schema fork pinned in `go.mod`
(`tsouza/opentelemetry-collector-contrib:cerberus-ddl`) so the schema
templates the gateway renders match what cerberus reads.

### Deploy

`just e2e-up` applies the whole `deploy/k3s/kustomization.yaml` into a
fresh k3d cluster. The flow:

1. `clickhouse.yaml` brings up single-node ClickHouse.
2. `cerberus.yaml` deploys cerberus, with `CERBERUS_OTEL_ENDPOINT`
   pointing at `otel-collector-gateway.cerberus.svc:4317`.
3. `otel-collector.yaml` brings up the gateway Deployment + agent
   DaemonSet, RBAC, and the two ConfigMaps.
4. `grafana.yaml` + `grafana-dashboards.yaml` provision the three
   cerberus datasources and the `Cerberus self-observability` dashboard.
5. `sample-app.yaml` runs three `telemetrygen` workloads (traces /
   metrics / logs) targeting the gateway so the tables never run empty.

## Cerberus self-dashboard

`deploy/grafana/dashboards/cerberus-self.json` ships as a provisioned
JSON model. It is auto-loaded under the **Cerberus** folder in Grafana
by the file provider declared in `deploy/k3s/grafana-dashboards.yaml`.

| Panel | What it shows                                                                             | Metric                                                         |
| ----- | ----------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| 1     | Per-second query rate, broken down by upstream language (`promql` / `logql` / `traceql`). | `cerberus_queries_total`                                       |
| 2     | P50 / P95 / P99 query latency across all three heads.                                     | `cerberus_queries_duration_seconds_bucket`                     |
| 3     | Per-stage latency (parse / lower / optimize / emit / execute), stacked.                   | `cerberus_pipeline_stage_duration_seconds_bucket`              |
| 4     | ClickHouse rows + bytes read per second, summed across all queries.                       | `cerberus_ch_rows_read_total` / `cerberus_ch_bytes_read_total` |
| 5     | Error rate by language (`result="error"` / total), with 1% / 5% threshold lines.          | `cerberus_queries_total{result="error"}` / total               |

All panels target the `cerberus-prometheus` datasource and filter on
`service_name="cerberus"` so a multi-tenant gateway (one collector fed
by many cerberus replicas) still attributes data correctly.

### Import outside the k3s stack

If you run cerberus + Grafana some other way (standalone Docker,
docker-compose, or a separate cluster):

1. Make sure Grafana has a Prometheus-type datasource pointing at
   cerberus's `:8080`.
2. Either:
   - Drop `deploy/grafana/dashboards/cerberus-self.json` into a
     provisioned dashboards folder, or
   - In the Grafana UI, **Dashboards → New → Import**, upload the JSON,
     and pick the cerberus Prometheus datasource at the prompt.
3. If your datasource UID differs from `cerberus-prometheus`, edit the
   JSON's `datasource.uid` fields (or use the per-panel datasource
   picker after import).
