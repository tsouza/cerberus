# `test/e2e/k3s/` — self-contained E2E stack

This directory ships a one-shot k3d/k3s stack that brings up cerberus
together with ClickHouse, Grafana, an OpenTelemetry Collector pipeline,
and a synthetic OTLP workload. `just e2e-up`:

1. applies the surrounding fixtures (`kustomization.yaml`) via
   `kubectl apply -k`, then
2. **deploys cerberus itself via its published Helm chart**
   (`deploy/helm/cerberus`, values in `cerberus-values.yaml`) with
   `helm upgrade --install`.

This **dogfoods the chart**: the e2e cluster is the only place the chart
is exercised in a *live* cluster (the `chart-validate` job only
lints / templates / kubeconforms it statically). A chart regression —
a bad env mapping, a broken probe, a wrong port — now fails the
dashboard + chaos e2e lanes, not just review.

## Components

| Source                      | What it deploys                                                                                                                                                           |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cerberus-values.yaml`      | **Cerberus** — installed via `deploy/helm/cerberus` Helm chart: Deployment, NodePort Service (host `:8080`), HPA, PDB, ServiceAccount, env ConfigMap, CH-password Secret. |
| `namespace.yaml`            | The `cerberus` namespace everything lives in.                                                                                                                             |
| `clickhouse.yaml`           | Single-node ClickHouse (Deployment + Service + PVC-backed data dir) for `otel`.                                                                                           |
| `grafana.yaml`              | Grafana 11 with provisioned Cerberus-{Prometheus,Loki,Tempo} datasources.                                                                                                 |
| `grafana-dashboards.yaml`   | Dashboard provider config + `Cerberus self-observability` dashboard ConfigMap.                                                                                            |
| `otel-collector.yaml`       | Gateway Deployment + per-node DaemonSet, RBAC, ServiceAccount, two ConfigMaps.                                                                                            |
| `sample-app.yaml`           | Three `telemetrygen` Deployments (traces / metrics / logs) targeting the gateway.                                                                                         |

The chart features exercised by `cerberus-values.yaml` (typed
clickhouse / otlp / autoCreate blocks, the chart-managed Secret, the
`config` passthrough, `extraEnv`, probe + resource overrides, NodePort
Service, HPA, PDB, ServiceAccount) are listed at the top of that file.
The two 0.2.0 HA features that need a real multi-node / multi-replica
cluster (`schema.replicated`, `affinityPresets.colocateWithClickHouse`)
cannot run on single-node k3d and are covered by `chart-validate`
against `deploy/helm/cerberus/ci/ha-values.yaml` instead.

## Dual-data-source model

Cerberus's E2E stack populates ClickHouse from **two independent
sources** that write to the same `otel.*` tables:

1. **Synthetic seed (`just e2e-seed`).** Runs the Go program at
   `test/e2e/seed/cmd/seed/`. It applies the upstream OTel-CH DDL via
   `internal/schema/ddl.Apply` and inserts a small set of deterministic
   rows (the canonical `up` metric, a couple of log lines, a span pair,
   etc.). This is what spec-style E2E tests assert on — they need
   known values at known timestamps with known labels.

2. **Real OTel pipeline (`test/e2e/k3s/otel-collector.yaml`).** Boots
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

1. ClickHouse starts (from the kustomize fixtures).
2. Cerberus is installed via Helm and starts. `cerberus-values.yaml`
   sets `autoCreate.schema: true` (→ `CERBERUS_AUTO_CREATE_SCHEMA`), so
   it creates the OTel schema at boot. Otherwise the collector creates
   it on first write.
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

## Horizontal scale-out (HPA)

The chart's `autoscaling` block (enabled in `cerberus-values.yaml`)
renders a `HorizontalPodAutoscaler` targeting the `cerberus` Deployment,
brought up by the `helm upgrade --install` in `just e2e-up`.

Defaults:

- `minReplicas: 2`, `maxReplicas: 4`. The ceiling is a **CI** bound:
  the 16GB/4vCPU GitHub runner cannot back 10 × 1Gi of cerberus burst
  next to ClickHouse + collector + Grafana + Playwright (run
  27272406583 saturated CH that way). Production ceilings belong in
  production overlays.
- Scales on **CPU utilisation** at 70% of the pod's CPU request. The
  standard `metrics-server` is the only dependency (already present
  in k3d/k3s). No prometheus-adapter, no prometheus-operator.
- Scale-up: up to 3 pods per 60s, 30s stabilisation window.
- Scale-down: 1 pod per 300s, 5-minute stabilisation window.

The chart omits the Deployment's `replicas:` field whenever
`autoscaling.enabled` is true, so the HPA owns the count. Setting both
would cause the controllers to fight on every reconcile.

### Verifying

```sh
kubectl -n cerberus get hpa cerberus
kubectl -n cerberus describe hpa cerberus
```

The output should show `TARGETS  70%/<current>%` and
`MINPODS  2`, `MAXPODS  4`. If `TARGETS` is `<unknown>/70%`,
metrics-server has not reported yet — give it 60 seconds after the
pods become Ready.

### Tuning

- **Bigger ceiling** — raise `maxReplicas` in a production overlay
  once the upstream ClickHouse cluster can absorb the parallel query
  load; this manifest's value is sized for the CI runner.
- **Quieter scale-down** — increase `scaleDown.stabilizationWindowSeconds`
  for workloads with predictable diurnal dips.
- **Load-aware scaling** — set `autoscaling.extraMetrics` in the chart
  values for a prometheus-adapter recipe that scales on
  `rate(cerberus_queries_total[1m])` instead of CPU.

### Pairing with admission caps

When `CERBERUS_MAX_INFLIGHT_PROM` / `_LOKI` / `_TEMPO` / `_TAIL` are
set, a saturated pod returns `503 Retry-After` immediately rather
than queuing work that ClickHouse cannot keep up with. Average CPU
utilisation across the remaining pods stays high, the HPA spawns the
next replica, and the new pod takes its share of traffic on the
following load-balancer hash. The admission caps protect ClickHouse
while the HPA grows the pool.

## Cleaning up

`just e2e-down` deletes the k3d cluster wholesale. There is no
in-cluster cleanup recipe — the whole stack is ephemeral.
