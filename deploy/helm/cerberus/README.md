# cerberus

<!-- markdownlint-disable MD060 MD012 -->
<!-- Tables (including the helm-docs-generated values table) are not
     pipe-aligned, and the version footer adds a trailing blank — both are
     owned by helm-docs; realigning would fight the chart-ci drift check. -->

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 1.0.0](https://img.shields.io/badge/AppVersion-1.0.0-informational?style=flat-square)

Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse — a single stateless gateway that speaks three upstream query wire formats and lowers each to parameterised ClickHouse SQL.

cerberus is a single, stateless gateway that speaks the Prometheus, Loki, and
Tempo HTTP wire formats and lowers every query into parameterised ClickHouse
SQL. Point three Grafana datasources at one cerberus endpoint.

**Homepage:** <https://github.com/tsouza/cerberus>

## TL;DR

```console
helm install my-cerberus oci://ghcr.io/tsouza/cerberus/charts/cerberus \
  --set clickhouse.addr='{clickhouse:9000}' \
  --set clickhouse.existingSecret=ch-creds
```

## Configuration strategy

cerberus is configured **100% via environment variables** — there is no config
file. This chart lowers three layers into env:

1. **Typed blocks** (`clickhouse` / `otlp` / `autoCreate` / `admit` / `schema` /
   `http`) → canonical `CERBERUS_*` env in a ConfigMap; the ClickHouse password
   flows through a Secret (`existingSecret` preferred).
2. **`config: {}`** — a free-form map rendered verbatim as `KEY: value` env. Use
   it for any `CERBERUS_*` knob not covered by a typed block (the long tail; see
   [docs/configuration.md](https://github.com/tsouza/cerberus/blob/main/docs/configuration.md)).
3. **`extraEnv` / `extraEnvFrom`** — raw container env (supports `valueFrom`).

**Precedence (last wins):** typed blocks < `config` < `extraEnv`.

## Observability

cerberus exposes **no Prometheus `/metrics` endpoint** — self-telemetry is
**pushed via OTLP**. There is intentionally no `ServiceMonitor` in this chart.
Set `otlp.endpoint` to enable export; leave it empty to disable.

## Health endpoints

| Probe     | Path       | Behaviour                                                        |
| --------- | ---------- | --------------------------------------------------------------- |
| Liveness  | `/healthz` | Dependency-free; a failure restarts the pod.                    |
| Readiness | `/readyz`  | Pings ClickHouse; a failure ejects the pod from Service endpoints (no restart). |

## Memory sizing

`resources.limits.memory` defaults to `1536Mi` with no CPU limit. cerberus's Go
heap doesn't see the cgroup limit; if you change the memory limit, set
`GOMEMLIMIT` (~80%) via `extraEnv`:

```yaml
extraEnv:
  - name: GOMEMLIMIT
    value: "1228MiB"
```

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| tsouza |  | <https://github.com/tsouza> |

## Requirements

Kubernetes: `>=1.23.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| admit.disabled | bool | `false` | Disable admission entirely (CERBERUS_ADMIT_DISABLED). |
| admit.loki | bool | `true` | Admit LogQL queries (CERBERUS_ADMIT_LOKI). |
| admit.prom | bool | `true` | Admit PromQL queries (CERBERUS_ADMIT_PROM). |
| admit.tempo | bool | `true` | Admit TraceQL queries (CERBERUS_ADMIT_TEMPO). |
| affinity | object | `{}` | Affinity. |
| args | list | `[]` | Full override of the container args. |
| autoCreate | object | `{"database":false,"schema":true}` | Auto-create toggles (lowered to CERBERUS_AUTO_CREATE_* env). See fields below. |
| autoCreate.database | bool | `false` | Create the target database if absent (CERBERUS_AUTO_CREATE_DATABASE). |
| autoCreate.schema | bool | `true` | Apply the OTel-CH schema DDL at boot (CERBERUS_AUTO_CREATE_SCHEMA). |
| automountServiceAccountToken | bool | `false` | Mount the ServiceAccount token into the pod. cerberus calls no k8s API → false (defence in depth). |
| autoscaling.behavior | object | `{}` | HPA scaling behaviour. Empty uses the cluster default. The reference manifest uses scaleUp-fast / scaleDown-slow. |
| autoscaling.enabled | bool | `false` | Enable a HorizontalPodAutoscaler. When true, `replicaCount` is ignored. |
| autoscaling.extraMetrics | list | `[]` | Extra HPA metric specs appended verbatim (e.g. custom/Pods metrics). |
| autoscaling.maxReplicas | int | `4` | Maximum replicas. |
| autoscaling.minReplicas | int | `2` | Minimum replicas (survives a single-pod failure at >=2). |
| autoscaling.targetCPUUtilizationPercentage | int | `70` | Target average CPU utilisation %. cerberus's hot path is CPU-bound on the gateway side, so CPU is a faithful load proxy. Set to null to drop it. |
| autoscaling.targetMemoryUtilizationPercentage | string | `nil` | Target average memory utilisation %. OFF by default — a memory target thrashes against GOMEMLIMIT-driven heap (rc.5 OOM finding). |
| clickhouse | object | `{"addr":["clickhouse:9000"],"database":"otel","dialTimeout":"10s","existingSecret":"","password":"","passwordKey":"password","protocol":"native","tls":{"caFileKey":"ca.crt","certFileKey":"tls.crt","enabled":false,"existingSecret":"","insecureSkipVerify":false,"keyFileKey":"tls.key","serverName":""},"username":"default"}` | ClickHouse connection block (lowered to CERBERUS_CH_* env). See fields below. |
| clickhouse.addr | list | `["clickhouse:9000"]` | ClickHouse address list (`host:port`), joined with `,` into CERBERUS_CH_ADDR. Native protocol is 9000 (9440 TLS); HTTP is 8123. |
| clickhouse.database | string | `"otel"` | Target database (CERBERUS_CH_DATABASE). |
| clickhouse.dialTimeout | string | `"10s"` | Dial timeout (CERBERUS_CH_DIAL_TIMEOUT). |
| clickhouse.existingSecret | string | `""` | Name of a pre-existing Secret holding the ClickHouse password. Takes precedence over `password` (no chart Secret is rendered). |
| clickhouse.password | string | `""` | Inline password. Renders a chart-managed Secret. PREFER `existingSecret` in production so the password never lands in values / release history. |
| clickhouse.passwordKey | string | `"password"` | Key within the Secret (chart-managed or existing) holding the password. |
| clickhouse.protocol | string | `"native"` | Wire protocol: `native` or `http` (CERBERUS_CH_PROTOCOL). |
| clickhouse.tls.caFileKey | string | `"ca.crt"` | Key in the TLS Secret for the CA cert → CERBERUS_CH_TLS_CA_FILE. |
| clickhouse.tls.certFileKey | string | `"tls.crt"` | Key in the TLS Secret for the client cert → CERBERUS_CH_TLS_CERT_FILE. |
| clickhouse.tls.enabled | bool | `false` | Enable TLS to ClickHouse (CERBERUS_CH_TLS_ENABLED). |
| clickhouse.tls.existingSecret | string | `""` | Name of a Secret holding the TLS cert files. When set, it is mounted at /etc/cerberus/tls and the CERBERUS_CH_TLS_*_FILE env point at the keys below. |
| clickhouse.tls.insecureSkipVerify | bool | `false` | Skip server cert verification (CERBERUS_CH_TLS_INSECURE_SKIP_VERIFY). Insecure — for testing only. |
| clickhouse.tls.keyFileKey | string | `"tls.key"` | Key in the TLS Secret for the client key → CERBERUS_CH_TLS_KEY_FILE. |
| clickhouse.tls.serverName | string | `""` | Override the TLS server name (SNI) (CERBERUS_CH_TLS_SERVER_NAME). |
| clickhouse.username | string | `"default"` | ClickHouse username (CERBERUS_CH_USERNAME). |
| command | list | `[]` | Full override of the container command (entrypoint). |
| commonLabels | object | `{}` | Extra labels added to every rendered object (tpl-rendered). NOT added to selectors (those are immutable). |
| config | object | `{}` | Arbitrary env vars rendered verbatim into the env ConfigMap. Use for any CERBERUS_* knob not covered by a typed block above, e.g. `{CERBERUS_QUERY_MAX_SAMPLES: "5000000", CERBERUS_CH_QUERY_MAX_MEMORY: "1073741824"}`. Overrides the typed defaults. |
| deploymentAnnotations | object | `{}` | Extra annotations on the Deployment object. |
| dnsConfig | object | `{}` | DNS config. |
| dnsPolicy | string | `""` | DNS policy. |
| extraArgs | list | `[]` | Args appended to the container when `args`/`command` are unset. |
| extraContainers | list | `[]` | Extra sidecar containers (tpl-rendered). |
| extraEnv | list | `[]` | Raw container env entries (supports `valueFrom` — fieldRef / secretKeyRef / resourceFieldRef). Overrides everything (envFrom is lowest precedence). Use for GOMEMLIMIT, downward-API values, externally-managed secrets. |
| extraEnvFrom | list | `[]` | Raw `envFrom` sources (configMapRef / secretRef) merged after the chart's own env ConfigMap. |
| extraManifests | list | `[]` | Arbitrary extra manifests rendered into the release (tpl-rendered list). |
| extraVolumeMounts | list | `[]` | Extra volume mounts on the cerberus container (tpl-rendered). |
| extraVolumes | list | `[]` | Extra volumes (tpl-rendered). |
| fullnameOverride | string | `""` | Override the fully-qualified release name (`<release>-cerberus` by default). |
| hostNetwork | bool | `false` | Use the host network namespace. |
| http.addr | string | `":8080"` | HTTP listen address (CERBERUS_HTTP_ADDR). The port here must match `service.targetPort`. |
| image.digest | string | `""` | Optional image digest (`sha256:...`). When set, pins by digest and is appended as `@<digest>` after the tag. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/tsouza/cerberus"` | cerberus container image repository. |
| image.tag | string | `""` | Image tag. Defaults to the chart's appVersion (e.g. `1.0.0`) when empty. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries. |
| ingress.annotations | object | `{}` | Ingress annotations (tpl-rendered). |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Enable an Ingress for the gateway. |
| ingress.hosts | list | `[{"host":"cerberus.local","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress hosts + paths. |
| ingress.tls | list | `[]` | Ingress TLS blocks. |
| initContainers | list | `[]` | Init containers (tpl-rendered). |
| lifecycle | object | `{}` | Container lifecycle hooks. |
| livenessProbe | object | `{"failureThreshold":6,"httpGet":{"path":"/healthz","port":"http"},"initialDelaySeconds":10,"periodSeconds":10,"timeoutSeconds":5}` | Liveness probe. Dependency-free `/healthz`; a failure restarts the pod. Budgets are sized for a saturated node (5s timeout, 6 failures ≈ 60s). |
| logFormat | string | `"json"` | Log format: json or text (CERBERUS_LOG_FORMAT). |
| logLevel | string | `"info"` | Log level: one of debug, info, warn, error (CERBERUS_LOG_LEVEL). |
| nameOverride | string | `""` | Override the chart name (defaults to the chart's own name, `cerberus`). |
| networkPolicy.egress | list | `[]` | Extra egress rules appended to the auto-derived ClickHouse/DNS/OTLP set. |
| networkPolicy.enabled | bool | `false` | Create a NetworkPolicy. Egress auto-allows the ClickHouse port(s) (parsed from `clickhouse.addr`), DNS, and the OTLP endpoint port (parsed from `otlp.endpoint`). |
| networkPolicy.ingress | list | `[]` | Ingress peer selectors on the gateway port. Empty = allow from anywhere; narrow to e.g. the Grafana namespace. |
| nodeSelector | object | `{}` | Node selector. |
| otlp.endpoint | string | `""` | OTLP gRPC endpoint for cerberus self-telemetry export (CERBERUS_OTLP_ENDPOINT). EMPTY disables self-telemetry export entirely. cerberus has NO /metrics endpoint — this is the only observability path. |
| otlp.exportInterval | string | `""` | Export interval (CERBERUS_OTLP_EXPORT_INTERVAL). |
| otlp.headers | string | `""` | Comma-separated OTLP headers, e.g. `authorization=Bearer xxx` (CERBERUS_OTLP_HEADERS). |
| otlp.insecure | bool | `false` | Use an insecure (plaintext) OTLP connection (CERBERUS_OTLP_INSECURE). |
| otlp.timeout | string | `""` | Export timeout (CERBERUS_OTLP_TIMEOUT). |
| podAnnotations | object | `{}` | Extra pod annotations (tpl-rendered). Merged with the config/secret checksum annotations. |
| podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget. |
| podDisruptionBudget.maxUnavailable | string | `nil` | Maximum unavailable pods (mutually exclusive with `minAvailable`). |
| podDisruptionBudget.minAvailable | int | `1` | Minimum available pods (used when `maxUnavailable` is null). |
| podLabels | object | `{}` | Extra pod labels (tpl-rendered). |
| podSecurityContext | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context. Defaults to distroless:nonroot (uid/gid 65532). |
| priorityClassName | string | `""` | PriorityClass name. |
| readinessProbe | object | `{"failureThreshold":5,"httpGet":{"path":"/readyz","port":"http"},"initialDelaySeconds":2,"periodSeconds":3,"timeoutSeconds":5}` | Readiness probe. `/readyz` pings ClickHouse (with a small TTL cache); a failure removes the pod from the Service endpoints (backpressure, no restart). |
| replicaCount | int | `2` | Number of replicas. Ignored when `autoscaling.enabled` is true (the HPA owns the replica count then). |
| resources | object | `{"limits":{"memory":"1536Mi"},"requests":{"cpu":"250m","memory":"128Mi"}}` | Pod resource requests/limits. Mirrors the reference k3s manifest: a small request, a generous memory limit, no CPU limit (bursting is fine; probe kills under CPU starvation are the real risk). If you change limits.memory, set GOMEMLIMIT (~80%) via extraEnv. |
| schema | object | `{}` | Schema overrides. Each key is suffixed onto `CERBERUS_SCHEMA_` verbatim, e.g. `{TTL: "30d", CLUSTER: "main"}` → CERBERUS_SCHEMA_TTL / _CLUSTER. See docs/configuration.md for the full CERBERUS_SCHEMA_* surface. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"privileged":false,"readOnlyRootFilesystem":true}` | Container-level security context. |
| service.annotations | object | `{}` | Service annotations. |
| service.appProtocol | string | `"http"` | appProtocol on the Service port (helps L7-aware meshes/ingress). |
| service.loadBalancerClass | string | `""` | loadBalancerClass (only honoured when `type: LoadBalancer`). |
| service.nodePort | string | `nil` | NodePort (only honoured when `type: NodePort`). |
| service.port | int | `8080` | Service port (the cerberus wire endpoint for all three heads). |
| service.targetPort | int | `8080` | Container target port. Must match the port in `http.addr`. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount (e.g. IRSA / Workload Identity). |
| serviceAccount.automountServiceAccountToken | bool | `false` | Mount the SA token into the SA itself. cerberus calls no k8s API → false. |
| serviceAccount.create | bool | `true` | Create a dedicated ServiceAccount. |
| serviceAccount.name | string | `""` | Name of the ServiceAccount to use/create. Generated from the fullname when empty. |
| startupProbe | object | `{}` | Startup probe (off by default). |
| terminationGracePeriodSeconds | int | `30` | Termination grace period (seconds). |
| tolerations | list | `[]` | Tolerations. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints. |
| updateStrategy | object | `{"type":"RollingUpdate"}` | Deployment update strategy. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
