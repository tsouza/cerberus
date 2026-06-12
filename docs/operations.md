# Operations

Cerberus runs as a single stateless HTTP server backed by ClickHouse. This
page describes the runtime contract: configuration, dependencies, process
model, signals, and scaling.

## Configuration

Every runtime knob is an environment variable read at startup by
`internal/config/config.go` — no YAML, INI, or TOML files are loaded.

| Variable                        | Default          | Meaning                                                                                                                                                                                                                           |
| ------------------------------- | ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_HTTP_ADDR`            | `:8080`          | HTTP listen address for the Prom/Loki/Tempo APIs and health probes.                                                                                                                                                               |
| `CERBERUS_CH_ADDR`              | `localhost:9000` | ClickHouse native-protocol endpoint.                                                                                                                                                                                              |
| `CERBERUS_CH_DATABASE`          | `otel`           | ClickHouse database name.                                                                                                                                                                                                         |
| `CERBERUS_CH_USERNAME`          | `default`        | ClickHouse user.                                                                                                                                                                                                                  |
| `CERBERUS_CH_PASSWORD`          | (empty)          | ClickHouse password.                                                                                                                                                                                                              |
| `CERBERUS_CH_DIAL_TIMEOUT`      | `5s`             | ClickHouse dial timeout (`time.ParseDuration` syntax).                                                                                                                                                                            |
| `CERBERUS_CH_QUERY_MAX_MEMORY`  | `1073741824`     | Per-query ClickHouse memory cap in bytes (`max_memory_usage` on every data-plane query; DDL exempt). `0` = don't set. Queries over the cap get a resource-exhausted rejection (Prom 422 / Loki 400 / Tempo 422), breaker-neutral. |
| `CERBERUS_QUERY_MAX_SAMPLES`    | `50000000`       | Per-query sample budget (Prometheus `--query.max-samples` parity); bounds cerberus-process memory. `0` disables.                                                                                                                  |
| `CERBERUS_AUTO_CREATE_SCHEMA`   | `false`          | When `true`, apply the OTel-CH DDL at startup before serving.                                                                                                                                                                     |
| `CERBERUS_LOG_FORMAT`           | `text`           | slog handler kind (`text` or `json`).                                                                                                                                                                                             |
| `CERBERUS_LOG_LEVEL`            | `info`           | Minimum slog level (`debug` / `info` / `warn` / `error`).                                                                                                                                                                         |
| `CERBERUS_OTLP_ENDPOINT`        | (empty)          | gRPC OTLP target for self-telemetry. Empty disables exporters.                                                                                                                                                                    |
| `CERBERUS_OTLP_INSECURE`        | `false`          | Dial OTLP endpoint without TLS.                                                                                                                                                                                                   |
| `CERBERUS_OTLP_HEADERS`         | (empty)          | Comma-separated `key=value` gRPC metadata (e.g. auth tokens).                                                                                                                                                                     |
| `CERBERUS_OTLP_TIMEOUT`         | `10s`            | Per-request OTLP roundtrip timeout.                                                                                                                                                                                               |
| `CERBERUS_OTLP_EXPORT_INTERVAL` | `10s`            | Metric `PeriodicReader` flush interval for self-telemetry. The quickstart default is tuned for time-to-first-panel; deployments running at scale should raise it (e.g. `60s`) to cut collector load.                              |
| `CERBERUS_ADMIT_DISABLED`       | `false`          | Disable per-handler concurrency caps.                                                                                                                                                                                             |
| `CERBERUS_ADMIT_PROM`           | `64`             | Max simultaneous in-flight Prom API requests.                                                                                                                                                                                     |
| `CERBERUS_ADMIT_LOKI`           | `64`             | Max simultaneous in-flight Loki API requests.                                                                                                                                                                                     |
| `CERBERUS_ADMIT_TEMPO`          | `32`             | Max simultaneous in-flight Tempo API requests.                                                                                                                                                                                    |

Schema-shape overrides (table names, when the CH layout deviates from
the OTel-CH exporter defaults) are listed in
[`observability.md`](observability.md#schema-shape-overrides).

Misconfigured values fail fast: an unparseable duration, an unknown log
level, or a malformed OTLP header list aborts startup with a clear error
rather than silently downgrading behaviour. Secrets (CH password, OTLP
bearer tokens) live in the same env-var namespace and are sourced from
Kubernetes `Secret` / Docker `secrets:` / a vault-injecting init
container — never committed.

## Backing services

**ClickHouse** is the only mandatory backing service, reached
exclusively through the `CERBERUS_CH_*` connection inputs. Swapping a
local single-node CH for a managed ClickHouse Cloud cluster is a matter
of flipping the env vars and restarting the process — there is no code
path that knows or cares whether the resource is local, in-cluster, or
remote.

**OTLP collector** (optional) for self-telemetry is treated the same
way: `CERBERUS_OTLP_ENDPOINT` may point at a sidecar, a cluster-local
collector, or a SaaS ingest URL. When unset, cerberus installs no-op
trace, meter, and logger providers and runs as a zero-collector-dependency
binary.

## Process model

Cerberus holds no operational state. There is no query cache, plan
cache, result cache, or session store — every HTTP request goes through
parse → lower → optimize → emit → execute against ClickHouse from a
clean slate. The only in-process memory that survives a request is:

- The ClickHouse driver connection pool (`internal/chclient`).
- The schema configuration (`internal/schema`, immutable after startup).
- A short-TTL cache inside the readiness probe handler
  (`internal/api/health`) so probe traffic does not amplify into
  ClickHouse pings.

None of these survive a process restart, and none are shared across
replicas. ClickHouse is the durable store; cerberus is a stateless
translation layer in front of it.

## Port binding

Cerberus binds a single HTTP listener on `CERBERUS_HTTP_ADDR` (default
`:8080`). All three upstream APIs (Prometheus, Loki, Tempo) plus the
`/healthz` and `/readyz` probes are mounted on that one listener —
there is no separate admin port, no Unix socket, no embedded TLS
terminator. A reverse proxy or a Kubernetes `Service` publishes the
port to the outside world; cerberus itself only knows how to bind and
serve.

The same binding semantics apply in every environment: `docker compose
up` exposes `8080:8080`, `test/e2e/k3s/cerberus.yaml` declares a
`NodePort` on `30080 → 8080`, and a local `./cerberus` run from source
listens on `:8080`. No env-var translation is needed between deployment
targets.

### HTTP/2 (h2c) + gRPC on the same port

The same `:8080` socket accepts three protocol shapes:

- **HTTP/1.1** — the Prometheus, Loki, and Tempo HTTP datasources, plus
  `/healthz` and `/readyz` probes, plus Loki's WebSocket tail
  (`/loki/api/v1/tail`).
- **HTTP/2 cleartext (h2c)** — `application/grpc` content-type traffic
  flows into the embedded gRPC server. Cerberus serves the Tempo
  `StreamingQuerier` gRPC surface that Grafana's Tempo datasource opens
  when the user enables the "Streaming" toggle in datasource settings.
- **HTTP/2 prior-knowledge** — Go gRPC clients (Grafana's backend
  client included) connect directly without an upgrade dance.

The dispatch happens at the handler layer: an `h2c.NewHandler` wraps a
content-type-aware dispatcher that routes `application/grpc` requests
into the `*grpc.Server` and everything else into the existing HTTP
mux. This keeps deployment topology unchanged — one container port,
one `Service` port, one ingress rule.

Behind a TLS-terminating proxy (ingress-nginx, Envoy, Cloud Run): the
proxy negotiates HTTP/2 with the client and forwards h2c upstream to
cerberus. This is the standard pattern for in-cluster gRPC services
and needs no cerberus-side configuration.

For direct internet exposure you would need a `tls.Config` on the
listener (`CERBERUS_TLS_CERT`/`_KEY`) — not currently implemented;
deploy behind a TLS-terminating proxy or sidecar.

## Scaling

Cerberus scales horizontally by adding replicas. Because the process is
stateless, an N-replica deployment behind a round-robin load balancer
(Kubernetes `Service`, an external L4/L7 LB, or HAProxy) distributes
load without any coordination between cerberus instances. ClickHouse
handles the actual heavy lifting — parallel query execution,
distributed table sharding, result merging — so cerberus horizontal
scaling is bounded only by ClickHouse capacity, not by cerberus's own
CPU.

A single cerberus process is itself concurrent: the standard `net/http`
server multiplexes goroutines per request, and the ClickHouse driver
pool serves them from a shared connection set.

### Per-handler concurrency caps (admission control)

Cerberus's `internal/api/admit` package fronts each of the three API
heads with a counted semaphore that caps simultaneous in-flight
requests. Caps are env-driven via `CERBERUS_ADMIT_PROM` /
`CERBERUS_ADMIT_LOKI` / `CERBERUS_ADMIT_TEMPO` (defaults: 64 / 64 / 32
— Tempo is half because trace queries are typically the heaviest
per-call). Requests above the cap are rejected with HTTP 503 +
`Retry-After: 1` so well-behaved clients back off and ClickHouse stays
out of overload.

`CERBERUS_ADMIT_DISABLED=true` removes admission control entirely —
useful for local development where artificial caps mask real
concurrency bugs.

### Kubernetes HorizontalPodAutoscaler

The e2e manifests at `test/e2e/k3s/cerberus-hpa.yaml` ship a working
HPA reference: it scales replicas on CPU + in-flight request count
(via the `cerberus_query_inflight` gauge exported through OTLP). The
file is also a runnable example for production deployments.

## Lifecycle

### Startup

`main` parses the environment, opens the ClickHouse connection, builds
the OTel providers (no-op when `CERBERUS_OTLP_ENDPOINT` is empty), and
mounts the three API heads on a single mux wrapped with `otelhttp` so
every request becomes a server span. Optionally, when
`CERBERUS_AUTO_CREATE_SCHEMA=true`, the OTel-CH DDL is applied before
serving begins so the readiness probe doesn't gate on missing tables.

An **unreachable ClickHouse at boot is not fatal**: construction of the
connection pool is lazy (no dial), the startup connectivity ping is a
best-effort WARN, and a failed first DDL apply falls back to a
background retry loop. The replica comes up "started but unready" —
`/healthz` 200, `/readyz` 503 — and flips ready as soon as ClickHouse
answers, which is exactly the contract Kubernetes readiness gating
expects (a scale-up replica booting into a saturated CH must not
crash-loop; see [`health.md`](health.md)). Fail-fast is reserved for
misconfiguration that can never succeed — a bad env value or invalid
connection options abort startup with a clear error.

### Shutdown

On `SIGINT` or `SIGTERM`, cerberus:

1. Stops accepting new HTTP connections (`http.Server.Shutdown`).
2. Drains in-flight requests up to a bounded grace period (default
   10 s; the shutdown deadline doubles as the OTLP flush deadline).
3. Flushes pending OTLP batches and tears the providers down.
4. Closes the ClickHouse connection pool.

If the collector is unreachable during shutdown the OTLP exporter logs
the error and returns — cerberus exits cleanly rather than hanging.

The disposable model means a deployment can be rolled, scaled to zero,
or replaced with a new tag without coordinating with cerberus itself:
the process owes nothing to the prior generation beyond the ClickHouse
data already persisted.

## Build, release, run

- **Build** — `goreleaser` produces release artefacts (binaries +
  container images) from a Git tag. Source code is compiled, the binary
  is statically linked (`CGO_ENABLED=0` in release builds), and the
  version string is injected via `-ldflags` so `Version` in
  `cmd/cerberus/main.go` reflects the tag.
- **Release** — the build output is combined with the deployment
  configuration. In Kubernetes that means a specific image SHA in
  `test/e2e/k3s/cerberus.yaml` (or the operator's chart) plus the
  `cerberus-config` ConfigMap. The release is immutable: rolling back
  means redeploying the previous tag, not editing files in place.
- **Run** — the container is started; the process reads its
  configuration from the environment and binds its HTTP listener. No
  build-time work happens at run time; no `go run`, no `make` in the
  final image.

The distroless image enforces this separation by construction: it
ships only the compiled binary and root CA bundle.

## Dev / prod parity

Local development reads the same env vars and connects to the same
ClickHouse / OTel collector shapes as production. `docker compose up`
or `just e2e-up` (k3d) spin up the full stack — ClickHouse, the OTel
collector, and Grafana — so the development feedback loop exercises
the same code
paths the production deployment will. The compatibility harnesses
(`compatibility/prometheus/`, `compatibility/loki/`,
`compatibility/tempo/`) run the same docker-compose stacks against
reference Prom / Loki / Tempo for differential parity.

Time, locale, and clock sources are not mocked in cerberus's own code
path — `time.Now()` calls are real, and date formatting always uses
UTC. A production deployment that puts cerberus in a non-UTC timezone
container does not change behaviour because every CH-touching path
emits explicit `toDateTime64(...)` literals with explicit precision.

## Logs

Logs are written as an event stream — see
[`observability.md`](observability.md#logging) for the full contract
(stderr stream shape, OTLP bridge, slog attribute conventions).

## Admin commands

Cerberus has no embedded admin REPL. Schema operations are owned by
ClickHouse directly (run `clickhouse-client` against the cluster);
config changes happen by env-var update + process restart. The `gh pr
merge --auto --squash --delete-branch` flow is the source of truth
for operator-driven changes to the binary.
