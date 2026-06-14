# Operations

Cerberus runs as a single stateless HTTP server backed by ClickHouse. This
page describes the runtime contract: configuration, dependencies, process
model, signals, and scaling.

## Configuration

> **Full configuration reference → [`configuration.md`](configuration.md).**
> Every `CERBERUS_*` variable, its type, default, and per-area grouping lives
> there. This page covers how the key knobs interact with the running service.

Every runtime knob is an environment variable read at startup by
`internal/config/config.go` (the solver knobs by `internal/solver`). An optional
`cerberus.yaml` may supply file-level defaults, but the `CERBERUS_*` environment
contract always wins (precedence: env > file > built-in default) — see the
[configuration-file section in `configuration.md`](configuration.md#configuration-file-optional).
The most operationally significant knobs:

- **`CERBERUS_CH_ADDR` / `_DATABASE` / `_USERNAME` / `_PASSWORD`** point cerberus
  at ClickHouse; swapping a local node for a managed cluster is an env flip.
- **`CERBERUS_CH_QUERY_MAX_MEMORY`** bounds per-query ClickHouse memory so a
  single over-broad query gets a deterministic rejection instead of racing the
  server-total cap; **`CERBERUS_QUERY_MAX_SAMPLES`** bounds cerberus-process
  memory the same way.
- **`CERBERUS_CH_BREAKER_*`** tune the ClickHouse-disconnect circuit breaker
  (below); **`CERBERUS_ADMIT_*`** tune the per-handler concurrency caps
  ([Scaling](#per-handler-concurrency-caps-admission-control)).
- **`CERBERUS_EVAL_ROUTE`** + the `CERBERUS_SHARD_*` knobs tune the
  sharded-pushdown solver (below); **`CERBERUS_OTLP_ENDPOINT`** enables
  self-telemetry export.

Misconfigured values fail fast: an unparseable duration, an unknown log level,
or a malformed OTLP header list aborts startup with a clear error rather than
silently downgrading behaviour. Secrets (CH password, OTLP bearer tokens) live
in the same env-var namespace and are sourced from Kubernetes `Secret` / Docker
`secrets:` / a vault-injecting init container — never committed.

### ClickHouse circuit breaker

Every CH-touching call is guarded by a circuit breaker
(`internal/chclient/breaker.go`). After `CERBERUS_CH_BREAKER_THRESHOLD`
consecutive failures inside `CERBERUS_CH_BREAKER_WINDOW` the breaker trips
OPEN and methods return `ErrCircuitOpen` without dialling — the handler
layer maps that into `503` with `Retry-After: 5` so clients back off
instead of stacking inner-stage retries against a dead upstream. After
`CERBERUS_CH_BREAKER_OPEN_INTERVAL` the breaker admits exactly one
HALF-OPEN probe; a successful probe closes the circuit, a failed one
restarts the backoff. Pool-acquire timeouts, `MEMORY_LIMIT_EXCEEDED`
rejections, and client-cancelled requests are treated as breaker-neutral
(they prove CH is alive, or say nothing about its health) and never
advance the failure count.

The defaults (`5` / `10s` / `5s`, enabled) reproduce the pre-tunable
hardcoded values exactly, so out-of-the-box behaviour is unchanged.
Tighten the knobs for a flappier CH, loosen them to tolerate longer
hiccups, or set `CERBERUS_CH_BREAKER_ENABLED=false` to switch the breaker
off entirely — a disabled breaker is always-allow and never trips, so a
saturated or dead CH surfaces as ordinary dial/query errors (useful when
an external proxy or service mesh already owns CH fail-fast).

**Blast radius — per-head breakers over one shared pool, and a dedicated
`/readyz` probe breaker.** The single `chclient.Client` is constructed once at
startup and holds a **registry of breakers, one per head** — `prom` / `loki` /
`tempo` for the data planes plus a dedicated `probe` breaker for `/readyz` —
all fronting the **one** shared ClickHouse connection pool. Each API head is
handed its own breaker via `Client.ForHead(head)`; the readiness pinger gets
the `probe` breaker. So a query storm that trips one head's breaker OPEN
isolates the fast-fail to that head:

- **Only the storming head returns 503.** A Prom query storm that drives 5
  consecutive CH-health failures trips ONLY the `prom` breaker; Prom queries
  short-circuit to `ErrCircuitOpen` → `503` + `Retry-After: 5`, while Loki and
  Tempo keep their own CLOSED breakers and serve normally. One head's CH-path
  problem no longer 503s the other two.
- **`/readyz` stays green under a single head's storm.** The readiness probe
  pings through the dedicated `probe` breaker, which is driven ONLY by the
  low-rate, TTL-coalesced readiness pings — never by data-plane traffic. So a
  Prom-only storm 503s Prom queries while `/readyz` stays green and the pod is
  **not** evicted: it is still happily serving Loki and Tempo, and could serve
  Prom again within `CERBERUS_CH_BREAKER_OPEN_INTERVAL` once the HALF-OPEN probe
  recovers. A genuine total-CH outage still fails the readiness pings
  themselves, trips the `probe` breaker, and flips `/readyz` red → correct
  eviction. The probe breaker uses a slightly tighter default failure budget so
  a dead CH is reported red well inside the k8s `readinessProbe` eviction window
  even though it only sees the throttled probe stream.

**Bulkhead boundary (what this does NOT isolate).** Per-head breakers isolate
the **503-cascade + pod-eviction** blast radius, NOT pool or CH-server
saturation. All heads still share ONE connection pool: a fan-out that saturates
ClickHouse's server-side resources can still slow the other heads' queries
(pool-acquire timeouts are breaker-neutral by design and never trip a breaker),
and a `MEMORY_LIMIT_EXCEEDED` (code 241) storm counts as breaker SUCCESS (CH
answering with a typed cap is proof it's alive), so it does not trip the
storming head's breaker at all. The isolation earns its keep where one head's
queries time out (code 159) or hard-error CH-side at a rate tripping that
head's budget. A query whose latency — not CH health — is the problem is bounded
separately by the per-query wall-clock timeout
([`CERBERUS_QUERY_TIMEOUT` in `configuration.md`](configuration.md#query-limits-and-memory)).

Tune `CERBERUS_CH_BREAKER_*` (or disable the breaker) per the failure budget
each head should tolerate; the knobs apply to every head, and the `probe`
breaker's tighter default trip budget keeps readiness honest about a truly dead
backend. The per-head state + trip telemetry
(`cerberus_ch_breaker_state{head=…}` / `cerberus_ch_breaker_trips_total{head=…}`)
shows exactly which head tripped.

These resilience contracts — the breaker trip + recovery (and the
per-head isolation + dedicated-probe-breaker `/readyz` contract above), the
breaker-neutrality of query timeouts / admit + pool rejections, the
`/healthz`-stays-green-on-CH-outage invariant, and replica resilience
under a single-pod kill — are validated against a *real* k3d deployment
under *real* faults by the **live-stack chaos lane** (the `chaos` job in
`.github/workflows/e2e.yml`, driven by `.github/scripts/chaos-run.mjs`;
locally `just e2e-chaos`). It is informational (push-to-main + nightly +
manual only, never a PR gate) and sits above the deterministic
stubbed-querier unit chaos in the required `check` lane. See
[`docs/test-strategy.md`](test-strategy.md) Layer 13 for the full
scenario + contract map.

### Sharded-pushdown solver

The sharded-pushdown solver (`internal/solver`,
[`solver.md`](solver.md)) handles the one query
class route A cannot bound: high **anchor fan-out** (`F = Range/Step`, e.g.
`sum(rate(m[5m]))` at a fine step over a wide range), where one statement's
peak intermediate cardinality exceeds the CH memory cap. For an eligible plan
it re-anchors `K` deep copies of the **same already-optimized plan** onto
disjoint slices of the anchor grid, emits each via the existing `chsql.Emit`,
and concatenates the result streams behind the existing cursor — no new
evaluator, no new SQL template, the same compat-gated route-A SQL per shard.

**ON by default (`CERBERUS_EVAL_ROUTE=auto`).** The solver routes in
production. `auto` is fail-toward-A: only
ELIGIBLE plans that clear the cost thresholds
(`CERBERUS_SHARD_MIN_FANOUT` / `CERBERUS_SHARD_MIN_ANCHOR_PAIRS`, and
`K >= 2`) take route B; everything else — instant queries, `now64`,
un-sliceable nodes, grid mismatches, below-threshold fan-outs, and every
non-PromQL head — stays byte-identical on route A. The flip is gated on the
`compatibility/prometheus-forced-route` CI job, which forces
`CERBERUS_EVAL_ROUTE=sharded` over the whole upstream PromQL corpus and fails
on any diff vs reference Prometheus.

**Modes:**

- `auto` (default) — route eligible, above-threshold plans; fail toward A
  otherwise.
- `single` — **disable routing.** The Planner still classifies every plan (so
  the shadow header stays populated), but never routes: every request runs
  route A, byte-identical to the pre-solver pipeline. Pin this to opt out.
- `sharded` — drop the cost thresholds to the floor (`K_min = 2`) so every
  ELIGIBLE plan routes; ineligible plans still stay on route A. Used by the
  forced-route compatibility lane as the corpus-wide proof; not a production
  setting.

**Shadow header.** Every response to a PromQL `query_range` carries the
additive `X-Cerberus-Route-Decision` header reporting the per-request
classification regardless of mode: `routed` (took route B),
`below-threshold`, `instant`, `not-sliceable`, `high-D`, `now64`,
`grid-mismatch`, `incommensurate`, or `scalar-heavy`. The header is **omitted**
for non-PromQL heads and when the solver is fully off (nil). It is purely
diagnostic — observe it to see what the solver would do (under `single`) or
did (under `auto`) without changing the wire body.

**All-or-nothing.** Whether a request is solved by route A or fanned out across
`K` shards, the client sees a single response. A shard failure surfaces as one
typed error (first-error-wins, cause-threaded), never a partial body. The
solver re-emits and re-executes per request — it never caches.

The remaining `CERBERUS_SHARD_*` / `CERBERUS_SOLVER_TIMEOUT` knobs in the
table above tune the shard count, concurrency, per-request output cap, and
per-shard memory apportionment; their defaults are deliberately conservative
against over-routing (Grafana's auto-step makes `rate[5m] @ 15s` hit `F=20`,
which must NOT route at the default thresholds unless the total expansion is
spike-class).

### Experimental: native rate (`timeSeriesRateToGrid`)

`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` (**default `false`**) opts the eligible
`rate(<counter>[<range>])` query_range shape into ClickHouse's compiled
`timeSeriesRateToGrid` aggregate instead of the default arrayJoin fan-out. The
native operator computes the same Prometheus `extrapolatedRate` *inside the
engine* — CH ported the calculation verbatim — closing the execution-layer gap
the SQL array machinery leaves at high cardinality. See
[`performance.md`](performance.md#the-durable-answer) for the why.

**Requirements and hard constraints:**

- **ClickHouse ≥ 25.6.** The `timeSeries*ToGrid` family was introduced in CH
  v25.6.0. The compose / e2e / compatibility deployment runs **25.8**
  (matching the chDB test substrate), so the function exists everywhere — but
  on any older server a native-path query still errors with `UNKNOWN_FUNCTION`,
  so the flag **must stay OFF** unless the target CH is ≥ 25.6. The experimental
  ClickHouse setting
  `allow_experimental_time_series_aggregate_functions=1` is sent **only on the
  queries that actually use the native node** (cerberus detects a
  `RangeWindowNative` in the emitted plan and stamps the setting per-query), so
  enabling the flag never adds an unknown setting to unrelated queries.
- **Scope: `rate` only.** `increase` / `delta` / `deriv` / `predict_linear`
  stay on the fan-out — there is no `timeSeriesIncreaseToGrid`, and the
  `timeSeriesDeltaToGrid` mapping is not yet differentially proven against
  Prometheus. Those functions, instant queries, and every non-PromQL head are
  unaffected by the flag.
- **Default OFF is byte-for-byte the established fan-out.** Every existing
  golden, the compat 574/574 corpus, and the compose / e2e lanes are
  structurally untouched when the flag is unset.

**Parity.** Validated on the chDB substrate (25.8) by a dual-emit test
(`internal/chsql/range_window_native_chdb_test.go`) that runs the fan-out and
the native path on the same seed and compares decoded float64 grids. On the
pinned 12-sample ramp 8 of 9 grid cells are bit-identical and 1 diverges by
exactly 1 ULP (the native value is the next double up from the correctly-rounded
fan-out value — a sub-observable float-order difference, both render `0.12`).
The test enforces a tight bound rather than the raw fixture count: **at most two
cells may diverge, each by no more than 1 ULP** (`maxDualEmitUlpDivergentCells
= 2`); any cell off by more than 1 ULP, or a third divergent cell, fails the
test as an arithmetic regression. Treat this as experimental: the compose / e2e
CH substrate is 25.8 (past the ≥ 25.6 introduction point, so the flag is
exercisable there), but the path still rides the experimental setting and has not
yet been differentially swept against a real (non-chDB) server where that setting
is actually enforced.

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

### Startup requirements preflight

`CERBERUS_REQUIREMENTS_CHECK` (**on by default**) runs a boot-time
requirements check immediately **after** the schema-create step. It
converts two classes of misconfiguration that would otherwise surface as
opaque query-time errors into a precise, fail-fast boot error:

- **ClickHouse too old.** The connected server's `version()` is compared
  against `max(base, applicable-feature-floors)` — base **24.8**, raised to
  **25.6** by the native-rate floor when `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` is
  on. A version below the floor (or an unparseable one) **fails startup
  fast** — a too-old server is a hard incompatibility that never self-heals.
- **Wrong-shape schema.** A configured table that **exists** but whose shape
  is wrong — a missing essential column, or an attribute-map column
  (`Attributes` / `ResourceAttributes` / `ScopeAttributes`) typed something
  other than `Map(String, String)` — **fails startup fast.** A wrong shape is
  a genuine misconfiguration, not a race, so failing fast is the honest
  signal. The check honours every `CERBERUS_SCHEMA_*` table rename — it
  validates the *active* shape.
- **Absent (not-yet-provisioned) schema.** When the configured tables are
  **entirely absent** (`system.columns` reports zero rows for them), cerberus
  does **not** crash-loop — it **boots and waits**. This is the cerberus +
  otel-collector startup race: a drop-in gateway deployed alongside the
  ingestion pipeline that owns schema creation may legitimately start before
  any table exists. Cerberus boots, reports **NOT READY** on `/readyz` with a
  precise reason (`schema not yet provisioned: table otel_logs absent`), and
  **re-probes** on the same cadence as the auto-create retry. The moment an
  external writer (the collector, or cerberus' own `CERBERUS_AUTO_CREATE_SCHEMA`)
  provisions the schema, `/readyz` flips ready **without a restart**.
  `/healthz` (liveness) stays **200** throughout — only readiness gates.

The ordering is deliberate: running the preflight **after** auto-create
means a fresh database where cerberus just created the tables passes the
schema gate (it would fail against tables that don't exist yet if the order
were reversed). When a **fatal** gate (too-old version, wrong-shape table)
fails, the process exits non-zero with an **aggregated** message listing
every unmet requirement at once, so an operator fixes the deployment in a
single pass rather than one error per restart. An **absent** schema is the
one finding that is *not* fatal — it is the transient case the wait-and-
reprobe path above handles. Set `CERBERUS_REQUIREMENTS_CHECK=false` to skip
both gates (logged as one line) — useful when pointing cerberus at a
deliberately non-default ClickHouse layout that the shape gate doesn't
model. Note the version + wrong-shape gates are a stricter contract than the
best-effort connectivity ping above: the preflight needs ClickHouse
reachable to read the version and column metadata, so a CH that is
unreachable at the preflight point (or returns an introspection *error*, as
opposed to a clean zero-row absence) fails startup rather than booting
unready.

### Schema divergence: MetricName-first metrics sort key

Cerberus auto-creates the OTel-CH schema from upstream's own DDL
templates (the `sqltemplates` API exposed by the
[`cerberus-ddl` fork](upstream-forks.md)), so the tables cerberus writes
match a stock OTel ClickHouse Exporter deployment — with **one
deliberate exception**. The five metrics tables (`otel_metrics_gauge`,
`otel_metrics_sum`, `otel_metrics_histogram`,
`otel_metrics_exp_histogram`, `otel_metrics_summary`) carry a
**MetricName-first sort key**:

```sql
ORDER BY (MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))
```

where stock OTel-CH leads its `ORDER BY` with `ServiceName`. The traces
and logs tables are unchanged from stock.

This divergence is **correctness-neutral**. A ClickHouse `ORDER BY`
(the table sort key) governs only data-skipping and on-disk row layout —
it never changes which rows a query returns. Cerberus therefore answers
**identically** whether the metrics tables carry the stock
ServiceName-first key or the MetricName-first key.

What it buys is metric-query speed. The common metric query carries a
`MetricName` matcher but no `service.name` matcher; against a
MetricName-first key ClickHouse range-prunes the primary key, against a
ServiceName-first key it falls back to a generic-exclusion granule scan
(~17× more granules read — see
[`benchmarks.md`](benchmarks.md#metricname-first-order-by)).

The practical contract for adopters:

- **Cerberus runs against an existing stock OTel-CH deployment without
  a schema change.** Pointed at tables that were created by the stock
  exporter (ServiceName-first metrics key), cerberus returns the same
  results as it does on the optimized key — the sort key changes only
  performance, not semantics — it simply forgoes the ~17× metric-query
  granule-prune speedup until the metrics tables carry the
  MetricName-first key.
- **`CERBERUS_AUTO_CREATE_SCHEMA=true`** is what writes the
  MetricName-first key: any metrics table cerberus auto-creates (the
  table does not already exist) gets the optimized sort key. The DDL
  is `CREATE TABLE IF NOT EXISTS`, so cerberus never rewrites the sort
  key of a table that already exists — adopting the optimized key on an
  existing stock table is an operator-driven migration (create the new
  table, backfill), not something cerberus does silently.

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
