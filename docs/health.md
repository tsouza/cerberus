# Cerberus health probes

Cerberus exposes two HTTP endpoints intended for orchestrator probes
(Kubernetes, Nomad, Docker healthchecks, …). They follow the standard
12-factor distinction between *liveness* (is the process alive?) and
*readiness* (is this instance ready to serve traffic?), and they back
the graceful-shutdown contract described in factor IX of the
[12-factor methodology](12factor.md#factor-ix--disposability).

Both endpoints live on the same HTTP listener as the Prom/Loki/Tempo
APIs (`CERBERUS_HTTP_ADDR`, default `:8080`) and are deliberately served
**outside** the OpenTelemetry middleware so high-frequency probe traffic
does not flood the trace backend.

## `/healthz` — liveness

```text
GET /healthz
200 OK
Content-Type: text/plain; charset=utf-8

ok
```

- Returns `200 OK` as long as the process is alive and the HTTP listener
  is accepting connections.
- Does **not** touch ClickHouse, the schema layer, or any other
  downstream dependency.
- A failure means the process is wedged and the orchestrator should
  restart the container.

## `/readyz` — readiness

```text
GET /readyz
200 OK
Content-Type: application/json

{"clickhouse":"ok","schema":"ready"}
```

On failure:

```text
GET /readyz
503 Service Unavailable
Content-Type: application/json

{"clickhouse":"error: dial tcp clickhouse:9000: connect: connection refused","schema":"unknown"}
```

- Pings ClickHouse via the configured `chclient.Client` connection
  pool. The ping is capped at 1 second.
- When `CERBERUS_AUTO_CREATE_SCHEMA=true`, also waits for the startup
  hook that bootstraps the OTel ClickHouse tables to have completed at
  least once.
- Results are memoised behind a **2-second TTL cache** so the typical
  3-second Kubernetes probe period coalesces into roughly one
  ClickHouse ping per probe.
- A failure removes the pod from the Service endpoints but does **not**
  cause a restart.

### Response shape

| Field        | Type    | Values                                                                                                                                        |
| ------------ | ------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `clickhouse` | string  | `"ok"` on success, `"error: <reason>"` on a failed ping.                                                                                      |
| `schema`     | string  | `"ready"` when the auto-create hook is done (or disabled), `"pending"` while it is still running, `"unknown"` when the CH ping itself failed. |

### HTTP status codes

| Status | Meaning                                                       |
| ------ | ------------------------------------------------------------- |
| 200    | Both ClickHouse and the schema invariant report healthy.      |
| 503    | At least one dependency is not yet ready.                     |

## Kubernetes probe configuration

The shipped `deploy/k3s/cerberus.yaml` wires the probes as follows:

```yaml
readinessProbe:
  httpGet:
    path: /readyz
    port: http
  initialDelaySeconds: 2
  periodSeconds: 3
  timeoutSeconds: 2
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 10
  periodSeconds: 10
```

### Recommended defaults

- **Readiness** — `periodSeconds: 3`, `timeoutSeconds: 2`. The TTL cache
  bounds the actual CH ping rate to ~1 per 2 seconds regardless of
  probe frequency.
- **Liveness** — `periodSeconds: 10`. Liveness probes are cheap (no CH
  call), so the period is set by container-restart sensitivity rather
  than by CH load.
- **Startup** — none needed today; `initialDelaySeconds: 2` on the
  readiness probe is enough for cerberus to bind its listener.

## Startup latency

Cerberus binds its HTTP listener fast: with
`CERBERUS_AUTO_CREATE_SCHEMA=false` and a reachable ClickHouse, the
gap from process spawn to first `200 OK` on `/healthz` is well under
2 seconds. The benchmark in `test/e2e/startup_bench_test.go` enforces
this with a 2500 ms ceiling (target < 2000 ms, plus a 500 ms safety
margin to absorb CI scheduler jitter).

Run it locally with:

```sh
# Requires a warm ClickHouse at $CH_ADDR (default 127.0.0.1:9000).
just startup-bench
```

The benchmark is build-tagged (`startup_bench`) and `RUN_STARTUP_BENCH`-
gated, so regular `just test` skips it. CI runs it as an informational
job in `.github/workflows/e2e.yml` (`startup-bench` job) on push-to-main,
nightly, and manual dispatch — it is **not** a required PR gate, so a
slow VM doesn't block merges, but a real regression (e.g. a new
synchronous startup hook that blocks the listener bind) shows up on the
very next merge.

When `CERBERUS_AUTO_CREATE_SCHEMA=true`, the startup hook that applies
the OTel ClickHouse DDL runs synchronously **before** the listener
binds, so both probes wait for the schema to be ready; in that mode the
< 2 s budget no longer applies (DDL apply time dominates).

## Implementation pointers

- Endpoint code: `internal/api/health/health.go`.
- Wire-up: `cmd/cerberus/main.go` (separate sub-mux so probes bypass
  the otelhttp wrapper).
- ClickHouse ping: `internal/chclient/client.go` — `(*Client).Ping`.
- Startup benchmark: `test/e2e/startup_bench_test.go`.
