# Cerberus health probes

Cerberus exposes two HTTP endpoints intended for orchestrator probes
(Kubernetes, Nomad, Docker healthchecks, …). They follow the standard
12-factor distinction between *liveness* (is the process alive?) and
*readiness* (is this instance ready to serve traffic?), and they back
the graceful-shutdown contract described in factor IX of the
[12-factor methodology](https://12factor.net/disposability).

Alongside them sits a third, cerberus-native endpoint —
[`/info`](#info--metadata-fingerprint) — which returns a single JSON
fingerprint of the build, the enabled heads, the resolved ClickHouse
optimizations, and the live connection state. It is for humans and
dashboards, not orchestrator probes.

All three endpoints live on the same HTTP listener as the Prom/Loki/Tempo
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

{"clickhouse":"ok","schema":"ready","heads":{"prom":"closed","loki":"closed","tempo":"closed"}}
```

On failure:

```text
GET /readyz
503 Service Unavailable
Content-Type: application/json

{"clickhouse":"error: dial tcp clickhouse:9000: connect: connection refused","schema":"unknown","heads":{"prom":"open","loki":"open","tempo":"open"}}
```

- Pings ClickHouse via the configured `chclient.Client` connection
  pool. The ping is capped at 1 second.
- When `CERBERUS_AUTO_CREATE_SCHEMA=true`, also waits for the startup
  hook that bootstraps the OTel ClickHouse tables to have completed at
  least once.
- Reports the live circuit-breaker phase of every head this process serves,
  and goes unready once **all** of them are open (see
  [Per-head readiness](#per-head-readiness)).
- Results are memoised behind a **2-second TTL cache** so the typical
  3-second Kubernetes probe period coalesces into roughly one
  ClickHouse ping per probe.
- A failure removes the pod from the Service endpoints but does **not**
  cause a restart.

### Response shape

| Field        | Type    | Values                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------ | ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `clickhouse` | string  | `"ok"` on success, `"error: <reason>"` on a failed ping.                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `schema`     | string  | `"ready"` when the schema is provisioned and the auto-create hook is done (or disabled); `"absent: <reason>"` when the boot-time requirements check found the configured schema not yet provisioned — either the tables are absent or the **database** itself does not exist yet (`database "otel" not yet provisioned: …`), both the cerberus + collector startup race where cerberus waits and re-probes, no restart; `"pending"` while the auto-create hook is still running; `"unknown"` when the CH ping itself failed. |
| `heads`      | object  | Circuit-breaker phase per **enabled** head (`CERBERUS_ENABLED_HEADS`), keyed `prom` / `loki` / `tempo`: `"closed"`, `"open"`, or `"half-open"`. Present on every response, success or failure.                                                                                                                                                                                                                                                                                                                               |

### HTTP status codes

| Status | Meaning                                                                            |
| ------ | ---------------------------------------------------------------------------------- |
| 200    | Both ClickHouse and the schema invariant report healthy.                           |
| 503    | At least one dependency is not yet ready, or every enabled head's breaker is open. |

### Per-head readiness

Each query head fronts ClickHouse through its **own** circuit breaker, so
"can this pod serve" is a per-head question and `/readyz` answers it per
head. The `heads` object names the live phase of every head this process
serves — a tripped head is visible on the probe itself, not only in the
`cerberus_ch_breaker_state` gauge.

The status code turns on **head exhaustion**: the pod goes unready once
every enabled head's breaker is open, and stays ready while any head can
still answer.

- Under the chart's **split mode** (one head per Deployment) a process serves
  exactly one head, so this *is* "this head's breaker is open": a tripped
  tempo Deployment leaves its Service while the prom and loki Deployments,
  whose own heads are healthy, keep serving.
- In **combined mode** one tripped head leaves two working ones behind.
  Evicting the pod there would take those two down for a fault that is
  already contained — the whole point of per-head breakers — and, since the
  breakers trip on a shared ClickHouse, would tend to evict every replica at
  once. So the phases are reported and the pod stays in its Service.

A head whose breaker is `half-open` is admitting a recovery probe rather
than failing, and does not count toward exhaustion.

Heads this process does not serve are never reported and never counted: a
head that was never built has no requests to fail, and counting it would
evict pods for a breaker nothing can reach.

## `/info` — metadata fingerprint

```text
GET /info
200 OK
Content-Type: application/json

{
  "service": "cerberus",
  "version": "1.6.1",
  "revision": "78f6a5720c…",
  "goVersion": "go1.23.0",
  "uptimeSeconds": 3725,
  "heads": ["prom", "loki", "tempo"],
  "clickhouse": {
    "address": "clickhouse:9000",
    "database": "otel",
    "serverVersion": "25.8",
    "serverVersionSource": "probe",
    "reachable": true,
    "breaker": "closed",
    "schemaReady": true
  },
  "optimizations": {
    "selection": "auto,columnar_result_decode",
    "mode": "enforcing",
    "resolvedAgainstVersion": "25.8",
    "enabled": ["aggregation_in_order", "columnar_result_decode", "condition_cache"]
  },
  "ready": true
}
```

`/info` is cerberus's own, unauthenticated build/config/connection
fingerprint — **not** an upstream-compat surface. The Prometheus and Loki
`buildinfo` endpoints (`/api/v1/status/buildinfo`,
`/loki/api/v1/status/buildinfo`) mirror their reference backends
byte-for-byte and stay faithful; cerberus's own metadata lives here at the
top level instead.

Unlike `/readyz`, `/info` **always returns `200 OK`** — it is a metadata
surface, not a probe. Readiness is reported *in the body* (`ready`, plus
the live `clickhouse` sub-object), so a scrape can read the fingerprint of
an unready process. Every field that describes something the process can
change while it runs is read on each request — the connection state through
the same dedicated `probe` breaker `/readyz` uses, and the ClickHouse
capability fields from the resolution currently in force. Only the build
identity and the operator's configured inputs are captured at boot.

### Response shape

Static fields, captured once at boot:

- `service` — always `"cerberus"`.
- `version` — build version (the goreleaser ldflag; `"dev"` in dev builds).
- `revision` — VCS commit (`runtime/debug` `vcs.revision`), or `"unknown"`
  when the build carries no VCS stamp.
- `goVersion` — `runtime.Version()`.
- `heads` — the **enabled** query heads (`CERBERUS_ENABLED_HEADS`), in
  `prom`, `loki`, `tempo` order.
- `clickhouse.address` / `clickhouse.database` — configured ClickHouse
  endpoint and database.
- `optimizations.selection` — raw `CERBERUS_CH_OPTIMIZATIONS` selection.
- `optimizations.mode` — `"enforcing"` or `"permissive"`.

Live fields, re-read on every request:

- `uptimeSeconds` — seconds since process start.
- `clickhouse.reachable` — a ClickHouse ping succeeds right now.
- `clickhouse.breaker` — circuit-breaker phase: `"closed"`, `"open"`, or
  `"half-open"`.
- `clickhouse.schemaReady` — schema provisioned and the auto-create hook
  complete (or disabled).
- `clickhouse.serverVersion` — resolved server version `<major>.<minor>`.
- `clickhouse.serverVersionSource` — `"probe"` when read live from the
  server, or `"fallback"` when the probe failed and the supported floor
  (`24.8`) was assumed.
- `optimizations.resolvedAgainstVersion` — the version the auto-picker
  resolved the selection against (equals `serverVersion`).
- `optimizations.enabled` — **the headline field**: the effectively enabled
  optimization feature ids. Makes plain whether cerberus is running the
  optimizations it should.
- `ready` — the same condition `/readyz` uses (CH reachable AND schema
  present AND schema ready).

The capability fields track the ClickHouse
[re-probe](clickhouse-optimizations.md#re-probe): a scrape taken after a
rolling ClickHouse upgrade reports what cerberus is emitting against now, not
what it booted with. Watching `optimizations.enabled` is therefore how an
operator confirms an upgrade actually reached the query path.

## Kubernetes probe configuration

The shipped `test/e2e/k3s/cerberus-values.yaml` wires the probes as follows:

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
- **Startup** — none needed; `initialDelaySeconds: 2` on the
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

The benchmark is build-tagged (`startup_bench`), so regular `just test`
skips it (the file isn't compiled without the tag). CI runs it as an
informational job in `.github/workflows/e2e.yml` (`startup-bench` job)
on push-to-main, nightly, and manual dispatch — it is **not** a required
PR gate, so a slow VM doesn't block merges, but a real regression (e.g.
a new synchronous startup hook that blocks the listener bind) shows up
on the very next merge.

When `CERBERUS_AUTO_CREATE_SCHEMA=true`, the startup hook that applies
the OTel ClickHouse DDL runs synchronously **before** the listener
binds, so both probes wait for the schema to be ready; in that mode the
< 2 s budget no longer applies (DDL apply time dominates). If that
first apply fails (typically: ClickHouse not up yet), cerberus does
**not** exit — the listener binds anyway and the apply is retried in
the background every 5 s; `/readyz` reports `"schema":"pending"` until
the first success.

## ClickHouse down at boot

With the requirements preflight off (`CERBERUS_REQUIREMENTS_CHECK=false`),
an unreachable ClickHouse never prevents startup. The connection pool
is constructed lazily (no dial), the startup connectivity ping is
demoted to a WARN log, and the process serves immediately:

- `/healthz` → `200` (the process is alive),
- `/readyz` → `503` (the CH ping fails),

flipping `/readyz` to `200` as soon as ClickHouse answers — no restart
needed. This is the readiness-gating contract Kubernetes expects: a
replica scaled up while ClickHouse is saturated waits out the outage out
of the Service endpoints instead of converting it into a
CrashLoopBackOff. Fail-fast remains for misconfiguration that can never
succeed (bad env values, invalid connection options).

The preflight is a deliberately **stricter** contract, and it is on by
default. `CERBERUS_REQUIREMENTS_CHECK` (the boot-time CH-version + schema
gate — see [`operations.md`](operations.md#startup-requirements-preflight))
needs ClickHouse reachable to read `version()` and `system.columns`. It still
boots into unready (never exits) for the **transient** cases — an unreachable
server, a not-yet-created database (`UNKNOWN_DATABASE`, surfaced even by the
`version()` probe because the connection carries the database as its session
default), or an absent schema — re-probing until the dependency appears. What
it converts into a fail-fast boot **error** is a *reachable* server that fails
the contract: a too-old / unparseable version, or a wrong-shape table. Set
`CERBERUS_REQUIREMENTS_CHECK=false` to skip the gate entirely.

## Implementation pointers

- Endpoint code: `internal/api/health/health.go` (`/healthz` + `/readyz`),
  `internal/api/info/info.go` (`/info`).
- Wire-up: `cmd/cerberus/main.go` (separate sub-mux so probes bypass
  the otelhttp wrapper; `infoOptions` builds the `/info` snapshot from
  config + chclient and injects the live closures, `enabledHeadBreakers`
  scopes the `/readyz` head report to `CERBERUS_ENABLED_HEADS`).
- ClickHouse ping: `internal/chclient/client.go` — `(*Client).Ping`.
- Breaker phase: `internal/chclient/client.go` — `(*Client).PeekBreakerState`
  for the connection-level view, `(*Client).HeadBreakerStates` for the
  per-head report `/readyz` renders.
- Capability re-probe behind the live `/info` optimization fields:
  `cmd/cerberus/chopt_reprobe.go`.
- Startup benchmark: `test/e2e/startup_bench_test.go`.
