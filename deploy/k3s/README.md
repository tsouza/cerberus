# `deploy/k3s/` — self-contained E2E stack

This directory ships a one-shot k3d/k3s manifest set that brings up
cerberus together with ClickHouse, Grafana, an OpenTelemetry Collector
pipeline, and a synthetic OTLP workload. `just e2e-up` applies the
whole `kustomization.yaml` via `kubectl apply -k`.

## Components

| Manifest                | What it deploys                                                                      |
| ----------------------- | ------------------------------------------------------------------------------------ |
| `namespace.yaml`        | The `cerberus` namespace everything lives in.                                        |
| `clickhouse.yaml`       | Single-node ClickHouse (Deployment + Service) backing the `otel` database.           |
| `cerberus.yaml`         | Cerberus Deployment + NodePort Service (host `:8080`).                               |
| `grafana.yaml`          | Grafana 11 with provisioned Cerberus-{Prometheus,Loki,Tempo} datasources.            |
| `grafana-dashboards.yaml` | Dashboard provider config + `Cerberus self-observability` dashboard ConfigMap.     |
| `otel-collector.yaml`   | Gateway Deployment + per-node DaemonSet, RBAC, ServiceAccount, two ConfigMaps.       |
| `sample-app.yaml`       | Three `telemetrygen` Deployments (traces / metrics / logs) targeting the gateway.    |

## Dual-data-source model

Cerberus's E2E stack populates ClickHouse from **two independent
sources** that write to the same `otel.*` tables:

1. **Synthetic seed (`just e2e-seed`).** Runs the Go program at
   `test/e2e/seed/cmd/seed/`. It applies the upstream OTel-CH DDL via
   `internal/schema/ddl.Apply` and inserts a small set of deterministic
   rows (the canonical `up` metric, a couple of log lines, a span pair,
   etc.). This is what spec-style E2E tests assert on — they need
   known values at known timestamps with known labels.

2. **Real OTel pipeline (`deploy/k3s/otel-collector.yaml`).** Boots
   alongside cerberus and continuously writes real telemetry:
   - The **agent DaemonSet** runs `kubeletstats` (node/pod/container
     metrics from the local kubelet) and `filelog` (container stdout/
     stderr tailed from `/var/log/pods/`) and forwards everything to
     the gateway over OTLP/gRPC.
   - The **gateway Deployment** runs `k8s_cluster` (cluster-wide
     metrics like node conditions, pod phases) and `k8s_events` (the
     k8s events stream as logs), accepts OTLP from the agent and from
     the sample-app, and writes through the `clickhouseexporter` to
     the `otel` database.
   - The **sample-app trio** (`telemetrygen`) emits a steady stream of
     traces, metrics, and logs against the gateway's OTLP endpoint so
     the trace + log signal tables are never empty even on a quiet
     cluster.

This is what Grafana dashboards and Playwright smokes target — they
need realistic shape, not exact values.

### Schema cannot drift

Both writers go through the **same** upstream `sqltemplates` package:

- `internal/schema/ddl` (used by `e2e-seed` and the cerberus startup
  hook) renders the upstream `CREATE TABLE` templates and executes
  them itself.
- The OTel collector's `clickhouseexporter` (`create_schema: true`)
  renders the same templates from its end of the pipeline.

If the upstream fork bumps a column, both sides pick it up in
lockstep. There is no parallel hand-maintained schema.

### Order of operations on `just e2e-up`

1. ClickHouse starts.
2. Cerberus starts. If `CERBERUS_AUTO_CREATE_SCHEMA=1` (default off
   in the k3s manifests; toggle via `cerberus.yaml`'s ConfigMap), it
   creates the OTel schema. Otherwise the collector creates it on
   first write.
3. The OTel collector gateway starts, applies the schema if it does
   not exist yet, accepts connections.
4. The collector agents and sample-app come up and start writing.

`just e2e-seed` is independent — it runs the Go seeder against
ClickHouse directly. Idempotent over an existing schema.

`just e2e-wait-otel` polls ClickHouse for non-zero row counts in
`otel_logs`, `otel_traces`, and one of the metrics tables so CI can
gate test execution on the pipeline being live (it usually takes
30–60s after `e2e-up` for the first batch to flush through).

## Versions

- `otel/opentelemetry-collector-contrib:0.116.1` — must match (or be
  compatible with) the version of the upstream OTel-CH schema fork
  pinned in `go.mod`. Bump in lockstep when the fork moves.
- `ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0`
  — same family, published as a separate image.

## Cleaning up

`just e2e-down` deletes the k3d cluster wholesale. There is no
in-cluster cleanup recipe — the whole stack is ephemeral.
