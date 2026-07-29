# Operations

Cerberus runs as a single stateless HTTP server backed by ClickHouse. This
page describes the runtime contract: configuration, dependencies, process
model, signals, and scaling.

## Configuration

> **Full configuration reference → [`configuration.md`](configuration.md).**
> Every `CERBERUS_*` variable, its type, default, and per-area grouping lives
> there. This page covers how the key knobs interact with the running service.

Every runtime knob is read at startup by `internal/config/config.go` (the solver
knobs by `internal/solver`), from a `cerberus.yaml` or from the matching
`CERBERUS_*` environment variable — a file is exactly equivalent to exporting
the variables it names, and the environment wins where both speak (precedence:
env > file > built-in default). See the
[configuration-file section in `configuration.md`](configuration.md#configuration-file).
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

### Connection teardown contract

A pooled ClickHouse connection survives a query only when that query reaches
its terminal state with its context still live. clickhouse-go has exactly two
terminal branches: drain to end-of-stream and the connection is released back
to the idle pool; cancel and the driver writes `ClientCancel` and destroys the
socket, because a cancelled query leaves undrained bytes on the wire. There is
no third branch — a clean release requires reading the remainder.

So every cursor cerberus opens is torn down **close first, cancel second**, and
that ordering is structural rather than a property of some caller's defer
order: `chclient.CloseCursor` owns both halves. It closes the cursor on its own
goroutine while the query context is still live, races that drain against a
fixed budget, and on expiry cancels — which unblocks the driver — and joins the
drain before returning. Each cursor's ClickHouse query therefore runs on its
OWN cancellable child of the request context, so teardown can cancel that query
without touching anything else the request still needs.

The budget is the deliberate trade. A caller that walked away must not be able
to pin a pooled connection for the length of a result set nobody will read, so
past the budget cerberus takes the destroyed socket and frees the pool slot
immediately. `cerberus_ch_cursor_teardown_total{outcome="abandoned"}` counts
exactly those, and a sustained non-zero rate is the signal that queries are
returning far more rows than their callers consume — a query-shape problem, not
a pool-sizing one. A teardown that begins on an ALREADY-dead context is a
different event and is counted separately as `outcome="cancelled"`: the socket
was destroyed by the client hanging up or the request deadline expiring, and no
budget cerberus could choose would have saved it. See
[`observability.md`](observability.md) for how the three outcomes decompose
overall connection churn.

The routed (multi-shard) path applies the same contract one level up: the
composed cursor signals its producers to STOP STREAMING, each producer tears
down its own cursor on its own live query context, and cancelling the shared
group context is the abort signal and bounded fallback — never the routine
teardown path. Because the cancel `CloseCursor` holds there is an ANCESTOR of
every per-shard query context, a composed cursor reports its own longer budget
through `chclient.ComposedCursor`: nesting its teardown inside a single
connection's drain budget would fire the ancestor cancel at exactly the moment
the K shard sockets were being released cleanly, destroying all of them. It is
also why the composed teardown itself is not counted — its children each count
their own. See [`solver.md`](solver.md).

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
[`test-strategy.md`](test-strategy.md) Layer 13 for the full
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

**Failure-driven route memo (`CERBERUS_SOLVER_ROUTE_MEMO_ENABLED`, default
`false`).** The Planner's cost thresholds are static and can misclassify a
plan whose real, data-dependent cost only shows up at execution time. When
enabled, `internal/routememo` (see
[`solver.md`](solver.md#failure-driven-route-memo)) retries a route-A
dispatch that fails on ClickHouse resource exhaustion once on route B, and
remembers the outcome against a literal-free fingerprint of the plan's cost
shape so a later cost-equivalent request routes directly instead of paying
the same failure again. It is opt-in — like `CERBERUS_CH_OPT_CORPUS_ENABLED`
and `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` — so upgrading cerberus never
silently changes ClickHouse dispatch/resource behavior. Two more knobs tune
it once enabled:

- **`CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL`** (duration, default: the
  `internal/routememo` package default of 30 minutes) — how long a recorded
  verdict is trusted before it ages out. Unset or non-positive leaves the
  package default in effect rather than disabling the memo.
- **`CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION`** (int, default: the
  package default of 2) — the divisor that places re-validation at the TTL
  midpoint. Same unset/non-positive-is-a-no-op contract as the TTL knob.

### Native rate (`timeSeriesRateToGrid`) — auto-enabled on 25.9+

The `ts_grid_range` optimization opts the eligible
`rate(<counter>[<range>])` query_range shape into ClickHouse's compiled
`timeSeriesRateToGrid` aggregate instead of the arrayJoin fan-out. Its maturity
label stays experimental, but it is **auto-selected** under the default
`CERBERUS_CH_OPTIMIZATIONS=auto` on any server `>= 25.9`. The deprecated
`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` boolean (**default `false`**) still works
as an override under `auto` — `true` force-enables, `false` force-disables (the
escape hatch back to the fan-out) — but new deployments should rely on `auto` or
list `ts_grid_range` explicitly in `CERBERUS_CH_OPTIMIZATIONS`. The
native operator computes the same Prometheus `extrapolatedRate` *inside the
engine* — CH ported the calculation verbatim — closing the execution-layer gap
the SQL array machinery leaves at high cardinality. See
[`performance.md`](performance.md#the-durable-answer) for the why.

**Requirements and hard constraints:**

- **ClickHouse ≥ 25.9.** The `timeSeriesRateToGrid` / `…ResampleToGrid…`
  aggregates first shipped in CH v25.6.0, but they used a **closed**
  `[anchor-window, anchor]` membership window until **v25.9** (PR #86588 made it
  left-open / right-closed to match PromQL's half-open range selector). On a
  grid-aligned corpus (scrape interval == range) the closed left edge admits the
  sample sitting exactly on `anchor-window`, so 25.6–25.8 emit a rate at grid
  points where reference Prometheus emits nothing — a systematic divergence, not
  a measure-zero edge. So the auto floor for the whole family is **25.9**. The
  compose / e2e / compatibility deployment and the chDB test substrate now run
  **26.5**, ABOVE that floor, so the native path is genuinely exercised there.
  The auto-picker gates on this floor automatically — it enables
  `ts_grid_range` only when the probed server is ≥ 25.9, so a connected older
  server keeps the fan-out and never diverges. (Force-enabling via the legacy
  `=true` flag against a < 25.9 server is still rejected at startup per mode.)
  The experimental ClickHouse setting
  `allow_experimental_time_series_aggregate_functions=1` is sent **only on the
  queries that actually use the native node** (cerberus detects a
  `RangeWindowNative` in the emitted plan and stamps the setting per-query), so
  enabling the flag never adds an unknown setting to unrelated queries.
- **The server must permit that experimental setting.** Meeting the 25.9 floor
  is necessary but not sufficient: a hardened ClickHouse profile that
  constrains/pins `allow_experimental_time_series_aggregate_functions`, or a
  readonly user, will reject the per-query stamp with
  `SETTING_CONSTRAINT_VIOLATION` / `READONLY`. cerberus **probes this at boot**
  (a one-shot capability canary alongside the version probe) and gates the
  native family on the verdict: under `auto` a forbidden server silently falls
  back to the fan-out with a boot `WARN`; an explicit `ts_grid_*` (or the legacy
  force-enable) on a forbidden server is FATAL under `enforcing` and WARN+skip
  under `permissive` — exactly the version-floor semantics. See
  [`clickhouse-optimizations.md`](clickhouse-optimizations.md#boot-capability-probe-experimental-ts_grid-setting).
- **Scope: `rate` only.** `increase` / `delta` / `deriv` / `predict_linear`
  stay on the fan-out — there is no `timeSeriesIncreaseToGrid`, and the
  `timeSeriesDeltaToGrid` mapping is not yet differentially proven against
  Prometheus. Those functions, instant queries, and every non-PromQL head are
  unaffected by the flag.
- **The fan-out remains byte-for-byte available.** Pinning `ts_grid_range` off
  (an explicit list omitting it, or the legacy `=false`) restores the
  established fan-out exactly; on a < 25.9 server it is the only path. Every
  existing golden, the compat 737/737 corpus, and the compose / e2e lanes are
  structurally the fan-out shape.

**Parity.** Validated on the chDB substrate (26.5) by a dual-emit test
(`internal/chsql/range_window_native_chdb_test.go`) that runs the fan-out and
the native path on the same seed and compares decoded float64 grids. The 26.5
substrate is above the 25.9 auto floor, so it already carries the left-open
window fix and the native path exercised here uses the same half-open membership
as PromQL — there is no closed-vs-left-open boundary difference to work around.
The seed keeps samples away from the window edges as belt-and-suspenders; the
test pins the emit shape and the extrapolation arithmetic, while the 25.9 floor
is what guarantees the boundary correctness in production. On the pinned
12-sample ramp 8 of 9 grid cells are bit-identical and
1 diverges by exactly 1 ULP (the native value is the next double up from the
correctly-rounded fan-out value — a sub-observable float-order difference, both
render `0.12`).
The test enforces a tight bound rather than the raw fixture count: **at most two
cells may diverge, each by no more than 1 ULP** (`maxDualEmitUlpDivergentCells
= 2`); any cell off by more than 1 ULP, or a third divergent cell, fails the
test as an arithmetic regression. The maturity label stays experimental because
the path rides ClickHouse's experimental setting, but it has since been
validated against a real (non-chDB) server with that setting enforced — found
result-correct at flat memory — which is why `auto` now selects it on ≥ 25.9
rather than leaving it opt-in.

### Recursive-CTE parallelism — recommend ClickHouse ≥ 26.6 for trace structure

The TraceQL structural operators (`>>`, `&>>`, the Explore-Traces structure tab)
lower to a `WITH RECURSIVE` nested-set numbering over the per-trace span forest
(`internal/chsql/nested_set_annotate.go`). Through 26.5, a `GROUP BY` over a
large recursive-CTE result was single-threaded server-side; **ClickHouse 26.6
parallelizes it**. This is automatic — no setting, no cerberus knob, no version
gate — so any server on 26.6+ runs the structural-join arm across cores with no
config change. cerberus's per-query memory bound on this path (the top-N
`BoundedTraceScope` leaf gate that caps how many traces feed the recursion)
holds on every floor; 26.6 only makes the bounded recursion *faster*. There is
no correctness floor here (the SQL is 24.8-safe), so it stays a **recommendation,
not a requirement**: trace-heavy deployments leaning on the structure tab should
prefer 26.6+, everyone else is unaffected.

### ClickHouse 26.5 — known-defective line

**Do not run cerberus on ClickHouse 26.5.x.** The 26.5 line (reproduced on
26.5.1 through 26.5.6, and on the chDB build of the same line) carries an
upstream query-execution regression in the top-K prefilter that backs
`ORDER BY <sorting-key column> … LIMIT n`: when the same attribute map appears
in both the `WHERE` predicate and the projection, the prefilter is handed the
map column where it expects the sort key and the server aborts the query with

```text
Code: 53. Type mismatch in IN or VALUES section. Expected: DateTime64(9).
Got: Map: while executing 'FUNCTION __topKFilter(Timestamp) …'
```

26.4 and earlier are unaffected (the optimisation does not exist), and
**26.6 fixes it**. The defect is gated on `LIMIT n` with
`n <= query_plan_max_limit_for_top_k_optimization` (default 1000).

For cerberus the blast radius is the Loki **`/loki/api/v1/detected_fields`**
endpoint — its peek projects `Body` + both attribute maps while the stream
selector filters on `ResourceAttributes`, and its default `line_limit` is
exactly 1000 — so Grafana's Logs Drilldown renders "Fields: 0" and the endpoint
returns 502 on every request. Sibling surfaces are clear: `/patterns` projects
the timestamp alongside the body (which sidesteps the prefilter), and the log
query path filters inside a subquery, so both keep working. Nothing is silently
wrong — the defect always aborts the query rather than returning bad rows.

Cerberus does not work around this in emitted SQL: the shapes that avoid it do
so incidentally, so pinning a golden to one would encode an upstream bug as a
cerberus invariant. The boot-time requirements check instead **warns** when it
sees a 26.5 server (it does not refuse to start — the rest of the gateway is
healthy), and the k3d e2e substrate is pinned to 26.6.

### Prometheus resource-attribute labels

The Prometheus head projects each metric row's OTel `ResourceAttributes` map as
Prometheus labels alongside the per-datapoint `Attributes` map — **on by
default**. Fleet metrics carry their resource-level keys (`k8s.namespace.name`,
`deployment.environment.name`, …) as ordinary labels you can filter and group
on. The projection runs in lock-step across every read surface: the bare
selector, `sum`/`avg by(...)`/`without(...)` aggregations, the matcher `WHERE`,
`/api/v1/series`, `/api/v1/labels`, and `/api/v1/label/<name>/values`.

Keys are sanitized dot→underscore on the wire for Prometheus legality
(`k8s.namespace.name` → `k8s_namespace_name`); a matcher like
`{k8s_namespace_name="prod"}` reverses the sanitized name through the
dot↔underscore candidate chain to filter the stored dotted key. On a key
collision the per-datapoint `Attributes` value **wins** over the
`ResourceAttributes` value (the Prometheus convention that a datapoint label
overrides a resource label); the dedicated `service.name` → `ServiceName`-column
handling is preserved.

**Cardinality.** Promote-all is **unbounded by design**: high-churn resource
keys (`k8s.pod.name`, `k8s.pod.uid`, `host.id`) become labels and multiply
active-series cardinality. To bound it, set `CERBERUS_PROM_RESOURCE_LABELS` to a
comma-separated allowlist of resource keys in their **original dotted** form —
opt-IN narrowing, empty/unset promotes every key. List only the resource keys
you actually query on at scale. See
[`configuration.md`](configuration.md#schema-overrides-and-prometheus-resource-labels).

**Memory.** Promoting resource attributes is not free: the merge
(`mapUpdate(sanitize(ResourceAttributes), sanitize(Attributes))`) runs
per-scanned-row at the scan leaf — before the staleness filter and the
range/aggregate reduction — so ClickHouse materialises a merged label map for
every row a query touches, and cerberus decodes the larger map for every result
row it buffers. The per-query heap cost grows roughly proportional to
*(rows scanned × promoted-resource-key count)*. A chDB-backed handler benchmark
(`BenchmarkResourceAttr_Range*` in `internal/api/prom`) measured **~+65% heap
per query** with the merge ON vs OFF on a 7-resource-key dataset — a genuine,
GC-recoverable per-query cost, not a leak (each query's cursor + buffered
matrix is released once the response is written). Size cerberus's memory limit
(and `GOMEMLIMIT`, which Go's GC needs since it does not read cgroup limits)
for the heavier per-query footprint, **or** trim the promoted set with
`CERBERUS_PROM_RESOURCE_LABELS` so only the keys you query on carry the cost.
The e2e manifest (`test/e2e/k3s/cerberus-values.yaml`) sizes the pod at 1536Mi /
`GOMEMLIMIT=1228MiB` for the promote-all default under the full dashboard
sweep; a tighter allowlist lets you run leaner.

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
up` exposes `8080:8080`, `test/e2e/k3s/cerberus-values.yaml` declares a
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
per-call). Each accepts an explicit integer cap or a boolean alias
(`true` = the default cap, `false`/`0` = that head unlimited), so a plain
chart bool and a precise operator cap both work. Requests above the cap
are rejected with HTTP 503 +
`Retry-After: 1` so well-behaved clients back off and ClickHouse stays
out of overload.

`CERBERUS_ADMIT_DISABLED=true` removes admission control entirely —
useful for local development where artificial caps mask real
concurrency bugs.

### Kubernetes HorizontalPodAutoscaler

The chart's `autoscaling` block ships a working HPA: the e2e values
(`test/e2e/k3s/cerberus-values.yaml`) enable it at 2–4 replicas on 70 %
CPU utilisation with a fast scale-up / slow scale-down `behavior`
policy. Because cerberus is stateless, CPU is a faithful proxy for
query load; `autoscaling.extraMetrics` can add a custom in-flight-request
signal where a metrics adapter is available.

### Helm: production HA against Replicated ClickHouse

The chart at `deploy/helm/cerberus` (published to
`oci://ghcr.io/tsouza/cerberus/charts/cerberus`) ships first-class typed
values for a multi-replica deployment. A representative production HA
`values.yaml`:

```yaml
replicaCount: 3
clickhouse:
  addr: ["clickhouse.clickhouse.svc.cluster.local:9000"]
  database: otel
  existingSecret: cerberus-ch-credentials   # password via Secret, never inline
requirementsCheck: true                     # boot-time ClickHouse preflight
schema:
  ttl: "2w"
  replicated:
    enabled: true                           # Replicated DB + ReplicatedMergeTree
    zookeeperPath: "/clickhouse/databases/otel/{shard}/{replica}"
prom:
  resourceLabels:                           # bounded allowlist — see below
    - service.name
    - k8s.namespace.name
    - k8s.pod.name
affinityPresets:
  colocateWithClickHouse:
    enabled: true
    mode: preferred
    topologyKey: kubernetes.io/hostname
```

Each typed block lowers to the canonical env:

- `schema.replicated.enabled` → `CERBERUS_SCHEMA_DATABASE_REPLICATED=true`
  and `schema.replicated.zookeeperPath` →
  `CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH`, driving the bare
  `ReplicatedMergeTree` emission documented under
  [Auto-create schema](#auto-create-schema-single-node-vs-clustered). The
  path **must** carry the `{shard}` / `{replica}` macros.
- `requirementsCheck` → `CERBERUS_REQUIREMENTS_CHECK=true` (see
  [Startup requirements preflight](#startup-requirements-preflight)).
- `prom.resourceLabels` → comma-joined `CERBERUS_PROM_RESOURCE_LABELS`. This
  is a **bounded allowlist**: leave it empty and cerberus promotes *every*
  OTel resource attribute to a Prometheus label — unbounded cardinality (see
  [Prometheus resource-attribute labels](#prometheus-resource-attribute-labels)).
  List only the attributes you actually query or group on.

Any `CERBERUS_SCHEMA_*` knob without a typed key still passes through as
`schema.<KEY>` (e.g. `schema: { CLUSTER: main }` → `CERBERUS_SCHEMA_CLUSTER`);
the typed keys win on conflict.

**Co-location is probabilistic, not node-local routing.**
`affinityPresets.colocateWithClickHouse` only influences *where cerberus pods
schedule* — it appends a podAffinity term (soft `preferred` by default, hard
`required` opt-in) onto whatever `affinity` the operator already set. Query
traffic still targets `clickhouse.addr`, which is the ClickHouse **Service**;
that Service round-robins across all `N` replicas, so a co-located cerberus pod
reaches the node-local replica only ~`1/N` of the time. The preset is worth
enabling to cut cross-AZ hops (set `topologyKey:
topology.kubernetes.io/zone`, or pair it with `Service.spec.trafficDistribution:
PreferClose` / `internalTrafficPolicy: Local` on the ClickHouse Service), but it
does **not** guarantee a node-local query path. True node-local CH preference —
a headless Service or per-pod endpoint with client-side replica locality — is a
deferred, app-side concern, not something the scheduling preset can deliver.

### Helm: bundled ClickHouse on object storage (bwc data tier)

`clickhouse.bundled.enabled` renders a self-contained ClickHouse StatefulSet
(plus its Services, plus a Keeper ensemble once `replicas > 1`) backed by an
object store (S3 / GCS / Azure) and defaults cerberus to point at it. This is
the **data tier** and is orthogonal to `mode` (monolith / split) — the gateway
topology is unchanged. With the default `enabled: false` the chart renders
byte-for-byte as if the block did not exist.

**Support / validation matrix.** Backends differ in how far they have been
exercised. Treat anything below "runtime-proven" as needing a real-cloud
validation pass against your own bucket/credentials before production use:

| Configuration                                | Status                         | How it is validated                                                         |
| -------------------------------------------- | ------------------------------ | --------------------------------------------------------------------------- |
| S3 / MinIO, single-node                      | **Runtime-proven**             | k3d e2e (`bwc-minio` lane): live MinIO, real read/write, placement asserted |
| S3 on real AWS (virtual-hosted + IRSA)       | Render / kubeconform-validated | `ci/bwc-aws-values.yaml` renders; no live-AWS run                           |
| GCS (S3-compat HMAC endpoint)                | Render / kubeconform-validated | `ci/bwc-gcs-values.yaml` renders; no live-GCS run                           |
| Azure Blob (account key or managed identity) | Render / kubeconform-validated | `ci/bwc-azure-values.yaml` renders; no live-Azure run                       |
| IRSA / GKE / AKS workload identity           | Render / kubeconform-validated | env / SA annotations render; no live cloud-identity run                     |
| Multi-replica + Keeper (ReplicatedMergeTree) | Render / kubeconform-validated | `ci/bwc-replicated-values.yaml` renders; no live multi-node run             |

Only **S3/MinIO single-node** is proven end to end on the CI substrate (the k3d
e2e brings up real MinIO and writes/reads through the object disk). Every other
row is rendered and schema-validated only; the XML wiring is correct by
construction but the cloud round-trip has not been exercised in CI.

**Pre-requisites that the chart does NOT handle for you:**

- **The bucket / container MUST be pre-created.** ClickHouse object disks (S3
  *and* Azure) do not create the bucket/container — point `objectStorage.bucket`
  (or `azure.container`) at one that already exists, or the disk fails on first
  write.
- **GCS** is reached over its S3-compatible (interop) endpoint with HMAC keys; a
  region/location hint on the bucket that matches your workload's region avoids
  cross-region egress. GCS rejects multi-object delete, so the chart already
  emits `<support_batch_delete>false</support_batch_delete>`.
- **S3 addressing** follows `s3.forcePathStyle`: a custom `endpoint`
  (MinIO/localstack) is always path-style; on real AWS, `false` (default) builds
  a virtual-hosted endpoint (`https://<bucket>.s3.<region>.amazonaws.com/`) and
  `true` builds the legacy path-style form.

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

### Auto-create schema: single-node vs clustered

When `CERBERUS_AUTO_CREATE_SCHEMA=true`, the `CERBERUS_SCHEMA_*` knobs shape
the DDL cerberus emits (all are no-ops when auto-create is off). The DDL is
built through the typed `internal/chsql` builder — cerberus never
hand-concatenates SQL — and the table column bodies still come verbatim from
the upstream OTel ClickHouse exporter templates; only the database engine,
`ON CLUSTER`, table engine and TTL clauses are cerberus-parameterised.

- **Single-node (default).** No cluster, no TTL, an Atomic database, plain
  `MergeTree` tables. Nothing to set.
- **Replicated database (recommended for a cluster).** Set
  `CERBERUS_SCHEMA_DATABASE_REPLICATED=true` and
  `CERBERUS_SCHEMA_DATABASE_REPLICATED_PATH=/clickhouse/databases/otel`. The
  database is created with `ENGINE = Replicated(<path>, {shard}, {replica})`,
  which **auto-replicates all DDL** across replicas — so you leave
  `CERBERUS_SCHEMA_CLUSTER` unset (no `ON CLUSTER` inside a Replicated
  database). A Replicated database does **not**, however, auto-convert
  `MergeTree` tables to `ReplicatedMergeTree`: replicated *DDL* gives each
  replica an independent table, but only a `ReplicatedMergeTree` engine
  replicates the *DATA*. So cerberus emits **bare `ReplicatedMergeTree`**
  tables (no engine arguments) by default whenever the database is Replicated,
  and you leave `CERBERUS_SCHEMA_TABLE_ENGINE` unset. The args are omitted on
  purpose: inside a Replicated database the engine's Keeper path and replica
  are supplied automatically (from the database's own `Replicated(...)`
  coordinates plus the server's `default_replica_path` /
  `default_replica_name`), and ClickHouse 24.8+ **rejects** an explicit
  `ReplicatedMergeTree('/path', '{replica}')` there with `code 36`
  (`database_replicated_allow_replicated_engine_arguments` defaults to `0`).
  Verify the data is genuinely replicated after deploy with
  `SELECT count() FROM system.replicas WHERE database = '<db>'` — it must be
  `> 0`.
- **Classic `ON CLUSTER` cluster.** Set `CERBERUS_SCHEMA_CLUSTER=<name>` and,
  if the engine isn't replicated by the cluster default, an explicit
  `CERBERUS_SCHEMA_TABLE_ENGINE=ReplicatedMergeTree('/clickhouse/tables/{uuid}/{shard}', '{replica}')`.
  `ON CLUSTER` and the Replicated database engine are mutually exclusive —
  pick one.
- **Externally-managed database.** When the database is provisioned by your
  cluster tooling (common for a Replicated database, whose Keeper path and
  macros are an infra concern), set `CERBERUS_AUTO_CREATE_DATABASE=false`:
  cerberus then creates only the **tables** inside it and never issues
  `CREATE DATABASE`. Leave it unset and it follows `CERBERUS_AUTO_CREATE_SCHEMA`
  — the hook creates the database too.

> **Why the database create needs a bootstrap connection.** ClickHouse rejects
> *every* statement (even `CREATE DATABASE`) on a session whose default database
> doesn't exist — and the configured database (`CERBERUS_CH_DATABASE`) is the
> session default, which is exactly the one that may be missing on a cold
> cluster. So when cerberus creates the database it does so over a one-time
> connection bound to ClickHouse's always-present `default` database; the
> fully-qualified `<db>.<table>` table creates run from there too.

**Retention is per signal.** `CERBERUS_SCHEMA_TTL` sets a global default;
`CERBERUS_SCHEMA_TTL_{METRICS,LOGS,TRACES}` override one signal each (a zero
override inherits the global). Retention keys on the signal — the five
metrics tables share one TTL, the spans + `trace_id_ts` lookup share another
— because that is how observability retention is actually managed (logs
short, metrics long). A deployment that needs genuinely per-table retention
runs the DDL itself rather than via the auto-create hook.

The TTL knobs accept the **Prometheus/Grafana duration syntax** operators
already use for retention windows — `90d`, `2w`, `1y`, or the Go `2160h`
form. `d`/`w`/`y` are fixed (24h / 7d / 365d), so a whole number of weeks
renders as `toIntervalWeek(N)` and everything else as the coarsest exact
ClickHouse interval (`toIntervalDay`/`Hour`/…). Calendar months and
calendar-aware years are intentionally not supported: they are
variable-length and a `1y` TTL is exactly 365 days, not a leap-aware
calendar year.

Auto-create also reuses the **same** table names the query heads read
(`CERBERUS_SCHEMA_*_TABLE`), so a renamed table is created and queried
consistently rather than silently diverging onto the upstream defaults.

### Metadata-enumeration projections (curated registry)

Auto-create installs a small **curated registry** of aggregating projections
on the gauge / sum / histogram fact tables. The registry lives in
`internal/schema/ddl` as a `(projectionName, body)` slice; each entry is
emitted as an idempotent `ADD PROJECTION IF NOT EXISTS` against every catalog
table at boot. Two projections ship:

```sql
-- proj_series: serves every windowless metadata-enumeration shape
ALTER TABLE <db>.<table> ADD PROJECTION IF NOT EXISTS proj_series
  (SELECT MetricName, Attributes, max(TimeUnix) GROUP BY MetricName, Attributes)

-- proj_metric_metadata: serves the windowless /api/v1/metadata listing
ALTER TABLE <db>.<table> ADD PROJECTION IF NOT EXISTS proj_metric_metadata
  (SELECT MetricName, any(MetricDescription), any(MetricUnit), max(TimeUnix)
   GROUP BY MetricName)
```

`proj_series` is the workhorse. The four windowless metadata shapes a Grafana
datasource sends on dashboard load — by far the heaviest metadata calls on a
busy backend — all route onto it:

- `label_values(__name__)` — distinct metric names. ClickHouse re-aggregates
  the finer `(MetricName, Attributes)` projection up to the coarser
  `GROUP BY MetricName` via **max-of-maxes**, so one projection serves both
  the per-name enumeration and the per-series shapes below;
- `label_values(<label>)` — distinct values of a label
  (`DISTINCT Attributes['k']` over the grouped form);
- label names (`/api/v1/labels`) —
  `arrayJoin(mapKeys(Attributes))` over the grouped form;
- series cardinality (`count()` over the grouped form).

`proj_series` also covers per-name access: `__name__` routes onto it via
re-aggregation with byte-identical results, so no dedicated per-name projection
is needed. The projection omits any
`Value` aggregate — the histogram catalog table has no top-level `Value`
column (it lives only inside the `Exemplars` Nested block) and none of the
routed shapes read a value, so a uniform `(MetricName, Attributes,
max(TimeUnix))` body stays valid across all three catalog tables. Note the
**Attributes** map is wide, so `proj_series` is the larger of the two
projections (measured storage overhead ~0.4 % of the catalog table at
realistic per-series row counts); `proj_metric_metadata` is tiny.

**Ongoing ingest cost (the honest part).** An aggregating projection is not
free at write time: ClickHouse re-sorts and writes a projection part for every
insert, so the projections levy a per-insert CPU + write-amplification tax for
as long as they exist — distinct from the one-time `MATERIALIZE` back-fill
below and from the (negligible) storage overhead above. A measured 3-way A/B on
a representative scrape workload:

| Configuration                          | Insert throughput | p50 insert latency | Storage |
| -------------------------------------- | ----------------- | ------------------ | ------- |
| no projection (baseline)               | —                 | —                  | —       |
| `proj_metric_metadata` only            | ~ −18 %           | ~ +33 %            | tiny    |
| `proj_series` + `proj_metric_metadata` | ~ −36 %           | ~ +70 %            | +~0.4 % |

The `proj_series` tax is roughly double the metric-name-only case because each
scrape batch's distinct `(MetricName, Attributes)` series collapse very little
under the grouping key, so the projection re-sorts and writes nearly as many
rows as the batch carries. Background **merge** cost is flat — the tax is paid
at insert, not at merge.

**Why this is acceptable here, with the number that makes it so.** This is a
real per-insert tax, but it is operationally immaterial at current production
scale: sustained ingest runs ~**2,824 rows/s**, against a measured
~**178k rows/s** sustained write ceiling on 4 cores — about **60× headroom**.
A 36 % throughput haircut consumes a sliver of that margin. Treat it as: real
tax, negligible given the headroom, revisit only if ingest headroom tightens
(an instance under genuine write pressure can install `proj_metric_metadata`
alone — it still covers the `__name__` enumeration at roughly half the tax, at
the cost of the per-series shapes `proj_series` adds). No caching or buffering
is involved; this is purely the write-path cost of maintaining the projections.

The handler emits each enumeration as the grouped
`… GROUP BY MetricName[, Attributes] HAVING max(TimeUnix) >= <lookback>`
shape (an aggregate-only predicate — a raw `WHERE TimeUnix >= …` column
filter cannot use a projection), which ClickHouse 26.x routes to the
matching projection, reading a sub-megabyte pre-aggregated part instead of
the fact table. Without the projection these enumerations full-scan the
metrics tables — on a real deployment ~4 billion rows / ~140 GiB / ~10 s for
a single refresh. The result set is identical: because samples are never
future-dated, `max(TimeUnix) >= lookback` is true for exactly the
names / values with a sample in `[lookback, now]`. A routing regression on a
ClickHouse upgrade is caught by the EXPLAIN + `read_rows` guard in
`internal/api/prom/metadata_scan_bound_explain_chdb_test.go` (the routed read
must stay orders below the unprojected baseline), so a silent fall-back to
full scans fails CI rather than degrading prod.

`ADD PROJECTION IF NOT EXISTS` is metadata-only and idempotent, so the
auto-create hook (re)applies the whole registry safely on every boot,
covering both freshly-created and pre-existing tables. **New parts written
after the ALTER carry the projections automatically; existing parts are not
back-filled by `ADD PROJECTION` alone.** Until existing parts roll over
(under the metrics TTL) or are back-filled, ClickHouse transparently serves
those parts from the base table — results stay correct, the prune ratio
ramps in as projected parts replace un-projected ones.

#### One-time `MATERIALIZE PROJECTION` back-fill runbook

To back-fill existing parts immediately on a deployment that predates the
projections, run the one-time materialize **per projection, per catalog
table** (a background mutation, non-blocking for reads):

```sql
-- proj_series
ALTER TABLE <db>.otel_metrics_gauge      MATERIALIZE PROJECTION proj_series;
ALTER TABLE <db>.otel_metrics_sum        MATERIALIZE PROJECTION proj_series;
ALTER TABLE <db>.otel_metrics_histogram  MATERIALIZE PROJECTION proj_series;
-- proj_metric_metadata
ALTER TABLE <db>.otel_metrics_gauge      MATERIALIZE PROJECTION proj_metric_metadata;
ALTER TABLE <db>.otel_metrics_sum        MATERIALIZE PROJECTION proj_metric_metadata;
ALTER TABLE <db>.otel_metrics_histogram  MATERIALIZE PROJECTION proj_metric_metadata;
```

`MATERIALIZE` is intentionally **not** issued by the auto-create hook — it
rewrites every existing part and belongs in a deliberate maintenance window,
not the boot path. Track progress in `system.mutations` (the
`is_done` / `parts_to_do` columns).

**Cost / caveat.** Each `MATERIALIZE` reads only the projection's source
columns (`MetricName`, `Attributes`, `TimeUnix` for `proj_series`) and writes
a small aggregated part per source part. On a production gauge table
(~2.9 billion rows / ~108 parts) the `proj_series` source columns are on the
order of **~9 GiB compressed**; the mutation is I/O-bound on that read.
Single-stream throughput measured ~1.4 M rows/s, but the background pool
parallelises across parts, so realistic wall time is **~3–8 minutes**. On a
ClickHouse cluster backed by object storage (S3 / GCS) the mutation **reads
and rewrites those parts through the object store**, so budget for the column
read + the projection-part write against your bucket's throughput and request
costs — on a wide-`Attributes` table the read side dominates. Materialize one
projection at a time and watch `system.mutations` before starting the next so
a maintenance window isn't saturated by both at once.

### Startup requirements preflight

`CERBERUS_REQUIREMENTS_CHECK` (**on by default**) runs a boot-time
requirements check immediately **after** the schema-create step. It
converts two classes of misconfiguration that would otherwise surface as
opaque query-time errors into a precise, fail-fast boot error:

- **ClickHouse too old.** The connected server's `version()` is compared
  against `max(base, applicable-feature-floors)` — base **24.8**, raised to
  **25.9** by the native-rate floor when `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` is
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
- **Absent (not-yet-created) database.** A step earlier than an absent table:
  the configured **database** itself does not exist yet. Because the connection
  carries the database as its session default, even the version probe's
  `SELECT version()` fails with `UNKNOWN_DATABASE` (ClickHouse code 81,
  `Database <name> does not exist`). This is the same cold-cluster race as an
  absent schema — the database is created moments later by the collector or by
  `CERBERUS_AUTO_CREATE_SCHEMA` — so it is **not** fatal: cerberus boots,
  reports **NOT READY** with a precise reason
  (`database "otel" not yet provisioned: …`), and re-probes until the database
  (and its tables) appear, with no restart. Treating it as fatal would
  crash-loop a gateway pointed at a database its collector hasn't created yet.

The ordering is deliberate: running the preflight **after** auto-create
means a fresh database where cerberus just created the tables passes the
schema gate (it would fail against tables that don't exist yet if the order
were reversed). When a **fatal** gate (too-old version, wrong-shape table)
fails, the process exits non-zero with an **aggregated** message listing
every unmet requirement at once, so an operator fixes the deployment in a
single pass rather than one error per restart. The **transient** findings —
an absent schema, an absent database, and an **unreachable** server — are the
ones that are *not* fatal: each takes the wait-and-reprobe path above, booting
**NOT READY** and flipping ready once the dependency appears. Set
`CERBERUS_REQUIREMENTS_CHECK=false` to skip both gates (logged as one line) —
useful when pointing cerberus at a deliberately non-default ClickHouse layout
that the shape gate doesn't model. The preflight needs ClickHouse reachable to
read the version and column metadata, but a server that is unreachable at the
preflight point is itself classified transient (a dial / connection-refused
error boots unready and re-probes, exactly like the connectivity ping above) —
**not** a fatal exit. What stays fatal is a *reachable* server that fails the
contract: a too-old / unparseable version, a wrong-shape table, or an
introspection *error* (as opposed to a clean zero-row absence, or the
`UNKNOWN_DATABASE` not-yet-created-database case).

### Schema divergence: MetricName-first metrics sort key

Cerberus auto-creates the OTel-CH schema from upstream's own DDL
templates (the `sqltemplates` API exposed by the
[`cerberus-ddl` fork](upstream-forks.md)), so the tables cerberus writes
match a stock OTel ClickHouse Exporter deployment — with **one
deliberate exception**. The five metrics tables (`otel_metrics_gauge`,
`otel_metrics_sum`, `otel_metrics_histogram`,
`otel_metrics_exponential_histogram`, `otel_metrics_summary`) carry a
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

### Metric name → table resolution

OTel-CH stores metrics in five tables by instrument type
(`otel_metrics_gauge`, `_sum`, `_histogram`, `_exp_histogram`,
`_summary`), but a PromQL `__name__` carries no type. Cerberus resolves a
metric name to the right table(s) and **unions across every physical
layout the name could live in**, so a query never returns 0 series just
because the upstream emitter dropped the rows in a table the Prom naming
convention didn't predict. The candidate set per name shape:

| `__name__` shape            | tables scanned (UNION ALL)                                   |
| --------------------------- | ------------------------------------------------------------ |
| unsuffixed (`foo`)          | gauge, sum                                                   |
| `foo_total`                 | sum                                                          |
| `foo_bucket`                | histogram (classic-bucket fan-out)                           |
| **`foo_count` / `foo_sum`** | **histogram (bare `foo`), sum (suffixed), gauge (suffixed)** |

The `_count`/`_sum` row is the subtle one: the name can be a classic
**histogram companion** (the OTel-CH exporter writes `Count`/`Sum` columns
on the bare-`foo` histogram row), a **cumulative sum** under the suffixed
name (OTel-hostmetrics: `system_cpu_logical_count`, …), **or a standalone
gauge literally named `foo_sum`** — e.g.
[`yace`](https://github.com/nerdswords/yet-another-cloudwatch-exporter)
emits each CloudWatch statistic as a name suffix
(`aws_applicationelb_request_count_sum`, `*_average`, `*_p99`), all plain
gauges. All three are scanned; empty arms are cost-free under the
per-arm `MetricName` primary-key prune, so a genuine histogram companion
pays nothing for the gauge/sum arms it doesn't use. This is why a gauge
named `*_sum`/`*_count` is queryable as its literal name rather than
silently resolving to a non-existent histogram base and returning empty.

The table above assumes a `__name__` **pinned to a literal** — the shape a
suffix heuristic can dispatch on. A classic-histogram row is stored under
its bare base name and exposed on the wire only as the synthetic companion
series `foo_bucket` / `foo_count` / `foo_sum`, so a matcher that pins one
of those names is resolved by stripping the suffix. A **regex** (or negated)
`__name__` matcher has no suffix to strip: applied to the stored
`MetricName` it would be tested against the base alphabet, which is not the
alphabet the client is asking about, and no histogram-derived series could
ever match. The metadata surfaces (`/api/v1/series`, `/api/v1/labels`,
`/api/v1/label/<name>/values`) therefore evaluate an unpinned `__name__`
matcher against the **synthetic name set** — the histogram base names in the
request window, crossed with the companion suffixes the selector lowering
serves — and re-issue each accepted name as a literal-pinned arm. The
enumeration is the same one `/api/v1/label/__name__/values` answers from, so
the names a client can discover and the names a matcher can select are one
set; and because each arm carries a `MetricName` equality it prunes on the
primary-key prefix rather than scanning the window's whole name space.

A second axis of resolution is the **separator**. A PromQL `__name__`
carries only `[a-zA-Z0-9_:]`, but the OTel-CH `MetricName` it must match can
hold the raw instrument name with **dots** (`k8s.pod.cpu.usage`) or
**slashes** — notably GCP Cloud Monitoring metric types, whose name is
`domain.parts/path/parts/leaf_name`, e.g.
`cloudsql.googleapis.com/database/up`. Cerberus reverse-maps the queried
underscored name to a bounded candidate set scanned via the same
PK-pruned `MetricName IN (…)`: the `2^k` dot powerset (each internal `_`
may have been a `.`), unioned with the **zone variants** that model the
GCP shape — contiguous dots, then slashes, then underscores. So
`cloudsql_googleapis_com_database_up` resolves to the slashed raw name
without any write-side renaming. The candidate set stays bounded (a
typical histogram chip ≈ 90 variants), so the `/series` metadata fan-out
stays one round-trip. Arbitrary interleaved separators (`a/b.c/d`) are out
of scope — real OTel/GCP names don't use them.

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
  `cmd/cerberus/main.go` reflects the tag. A single `cerberus` binary
  (`./cmd/cerberus`) ships per release as a `tar.gz` archive
  (`cerberus_<ver>_<os>_<arch>.tar.gz`) and is baked into the container
  image under `/usr/local/bin/`. Every CLI — the server plus the offline
  migration preview (`cerberus migrate …`) and the doc/analysis generators
  — is a subcommand of that one cobra-based binary.
- **Release** — the build output is combined with the deployment
  configuration. In Kubernetes that means a specific image tag/SHA in the
  Helm values (`test/e2e/k3s/cerberus-values.yaml` for the e2e stack) plus
  the chart-rendered env ConfigMap. The release is immutable: rolling back
  means redeploying the previous tag, not editing files in place.
- **Run** — the container is started; the process reads its
  configuration from the environment and binds its HTTP listener. No
  build-time work happens at run time; no `go run`, no `make` in the
  final image.

The distroless image enforces this separation by construction: it
ships only the compiled binary and root CA bundle.

### Release ritual (the ordered cycle)

Publishing is machinery; deciding *what* ships — and in which order — is the
release ritual. Every cycle runs these six steps top to bottom. The ordering
carries as much weight as the steps themselves:

1. **Drive everything merged first.** A cycle opens by draining the board: no
   open PR and no dangling branch-without-a-PR is left behind. A release ships
   the whole delta since the previous one, never a subset.
2. **Backport everything to every line that stays supported.** Every fix that
   landed on `main` since the previous release goes onto every
   `release/<major>.<minor>.x` line that is still supported once this cycle
   lands — step 3 settles which those are. "Everything" is the default rather
   than a per-fix judgement call. A change is left out of a line only when the
   backport is genuinely infeasible on that line, and a line that keeps
   rejecting backports is a retirement candidate, not a standing exception. A
   cycle that cuts a new minor also *adds* a line: the minor `main` is leaving
   needs its own maintenance branch, created from the peeled tag of its last
   release (`git push origin v<tag>^{}:refs/heads/release/<major>.<minor>.x` —
   the ruleset carries no `creation` rule, so this needs no bypass). That branch
   must exist before step 5 can publish a patch on it. See
   [maintenance lines](#maintenance-lines-hotfix-backports) for the mechanics.
3. **Settle the retirement set before any backport is cut.** When the cycle
   cuts a new minor — which a breaking change is by itself enough to force, see
   below — the oldest supported line falls out of the window defined in
   [release support window / EOL policy](#release-support-window--eol-policy).
   Work out which line that is up front, because it takes **no backport and no
   patch release** this cycle — spending a release on a line about to be retired
   is wasted work. This step is a decision, not an action: the retirement itself
   is automatic, with the `eol-retire` job deleting the out-of-window branch
   after the new minor publishes. A patch-only cycle retires nothing and passes
   straight through.
4. **Audit the delta.** One last pass over the complete diff since the previous
   release: code against comments against docs, DRY, KISS, soundness. This is
   the final gate — findings are fixed and merged here, before any tag exists.
5. **Publish the backport PATCH releases first.** Every still-supported older
   line gets its patch tag before the new head release exists.
6. **Publish the new MINOR (or patch) release last.** `main`'s release is always
   the final publish of the cycle.

**Breaking changes are accepted in a new minor.** On the cerberus version line
(`appVersion` / the `v<major>.<minor>.<patch>` tags) a breaking change does
**not** require a major bump — the minor is its vehicle. That makes "does this
delta break anything?" a step-3 input: a breaking change is on its own
sufficient reason for the cycle to cut a minor rather than a patch, and cutting
a minor is what pushes the oldest line out of the support window and calls for
a maintenance branch on the minor `main` is leaving. A cycle carrying neither a
breaking change nor a new feature is a patch cycle: it retires nothing, creates
no line, and passes step 3 straight through.

Two properties follow from that order. Publishing the older lines first means
the newest tag is never the one users find while the older lines are still
mid-flight: by the time the head release appears, every supported line already
sits at its final version. And placing the audit immediately before the first
publish means nothing merges between the audit and the tags it cleared.

Each individual publish in steps 5 and 6 runs through the machinery below — a
backport by pushing its `release/*.x` branch, the head release by merging its
release PR.

### Release pipeline (publish-on-merge)

Cerberus publishes when a **validated release PR is merged to main**, not when
a raw tag is pushed (release-please-style). The flow:

1. **Open a release PR.** Apply a `release:*` label to any issue (or run the
   `prepare-release` workflow manually). `prepare-release.yml` bumps the chart
   `version:` and/or `appVersion:`, rewrites the CHANGELOG, regenerates the
   chart README, and opens a PR from a `release/v<app>-chart-<chart>` branch.
2. **The PR runs the full matrix.** Because the head branch starts with
   `release/`, the PR runs not only the standard required checks and the e2e
   `split` + `crawl` legs, but also the four parity lanes —
   `coverage`, `mutation`, `perf-profile`, `property` — which on an ordinary PR
   short-circuit to a green no-op and only do their real (chDB-heavy) work on a
   release PR. So a release PR's green status reflects the *complete* matrix.
3. **Merge when green.** The maintainer merges once every required check is
   green on a tree up to date with main. That merge-when-green gate is the only
   thing standing between a release PR and publication — the commit on main is
   releasable by construction (its checks ran against the exact merged tree).
4. **`release.yml` publishes on the push to main.** It runs two per-line
   version gates:
   - **app** (`release-version-gate.mjs`): is Chart.yaml `appVersion` newer
     than the latest `v*` git tag? If so, create + push `v<appVersion>` at the
     merge commit and run goreleaser (binaries + multi-arch images to GHCR and
     Docker Hub + SLSA provenance).
   - **chart** (`chart-publish.mjs version-gate`): is the chart `version:`
     absent from the OCI registry? If so, create + push `chart-v<version>` at
     the merge commit and publish the chart (helm push + cosign + attest +
     Artifact Hub).
   A merge that bumped **neither** line publishes nothing — both gates return
   `publish=false`, so an ordinary code/docs merge is a complete no-op.

The job graph on a publishing merge is:

```text
gate ─▶ preflight ─▶ goreleaser ─▶ release-artifact-migration ─▶ publish ─┬▶ brew-smoke
                  │                                                       └▶ eol-retire
                  └▶ chart-release
```

Each edge is a gate, not a sequence:

- **`preflight`** runs on **both** paths (main and maintenance) whenever either
  version gate says something would publish. Branch protection alone is not
  enough even on main: it can only require lanes eligible to *be* a
  branch-protection check, and the heavyweight push-triggered lanes — notably
  `migration-e2e` (Layer 14) — are not. So the preflight carries its own
  **expected set** (`RELEASE_REQUIRED_CHECKS`) and refuses to publish when a
  required lane posted *no* check-run on the commit. A lane that did not run has
  not passed.
- **`goreleaser`** builds and uploads, but leaves the GitHub release a **draft**.
- **`release-artifact-migration`** re-runs the migration lane
  (`uses: ./.github/workflows/migration-e2e.yml`) against the image that was just
  built, asserting the running server reports the released version. The
  push-triggered run the preflight required proves the *source tree*; this proves
  the *artifact* — a bad ldflag or Dockerfile drift lives only in the latter.
- **`publish`** performs the draft→published flip. It is a separate job precisely
  so everything that validates the built artifact sits before the point of no
  return. The flip states `--latest` explicitly: GitHub defaults `make_latest`
  to true, so leaving it implicit hands the repo's `Latest` pointer to whichever
  release published most recently — which is how backporting v1.11.3 after
  v1.13.0 pointed `/releases/latest` at the oldest supported line.
- **`brew-smoke`** installs from the tap and smokes the published binary (see
  below); **`eol-retire`** retires the line that just fell out of the support
  window.

The two version lines are independent: a chart-only fix (template change, new
toggle) ships by bumping `version:` alone, and an app-only release bumps
`appVersion:` (plus a patch to `version:` for the new default image). The
publish gates handle either or both.

`RELEASE_IS_LATEST` — computed in `release.yml` right after the tag is pushed,
by comparing it against the highest stable `v*` tag — is the single answer to
"is this the newest release line?", and every resource that only one line can
hold at a time is gated on it: the rolling `:latest` image tags, the tap's
single Homebrew cask, and the GitHub `Latest` release pointer. A prerelease
or a stable backport never drags any of the three backwards.

#### De-gated lanes on the publish path

The preflight's expected set (`RELEASE_REQUIRED_CHECKS`) covers every
branch-protection context except the two below, which are listed in
`RELEASE_INFORMATIONAL_CHECKS` instead: they run, they report, and their
verdict does not hold a publish. Each one is a deliberate trade, so each one
carries its reason here — `TestReleasePreflightCoversEveryBranchProtectionContext`
and `TestDeGatedLanesAreDocumentedWithAReason` (both in
`test/regression/release_required_checks_test.go`) assert that this table and
`RELEASE_INFORMATIONAL_CHECKS` name exactly the same lanes, so a lane cannot be
de-gated without the reason landing here.

Note the direction of travel: de-gating here is the exception. The substrate
lanes `compose-smoke`, `dashboard` and `profile` went the OTHER way — they
stopped gating pull requests and became release-required, so this preflight is
now the only thing standing between them and a publish.

| Lane                       | Why it does not gate a publish                                                                                                                                                                                                                                                                                                                                                     |
| -------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `compose-smoke-shard-info` | A matrix child of `compose-smoke`, which is required. The aggregate deliberately does not `needs:` the crawl info shard, so the shard posts its own check-run; treating that run as required would let a flake in an explicitly non-blocking shard hold a release.                                                                                                                 |
| `mutation`                 | A test-QUALITY ratchet, not a property of the artifact. It ran green under branch protection on the release PR, and requiring it here would put its ~11-leg matrix on the critical path of every publish. On a maintenance line — where there is no PR at all — this means a hotfix publishes without a mutation verdict; that is the accepted cost of shipping hotfixes promptly. |

#### Homebrew tap

Stable releases publish a Homebrew cask to the
[`tsouza/homebrew-tap`](https://github.com/tsouza/homebrew-tap) tap, so operators
can install the single `cerberus` binary with:

```sh
brew install tsouza/tap/cerberus
```

That is also the shortest way to get the `cerberus migrate` CLI onto the
machine that holds an operator's rules and dashboards, so the migration
playbook points at it as the default install path — see
[getting the `cerberus` binary](migration.md#step-1-install-the-binary).

This is wired via the goreleaser `homebrew_casks:` block. A *cask* is the right
vehicle for a pre-built binary — a formula describes something Homebrew builds
from source — and it is not a macOS-only choice: a cask that declares no
`depends_on macos` reports `supports_linux?`, and Homebrew's cask installer gates
on the cask's declared OS rather than on the host being a Mac, so one cask
installs under Linuxbrew and macOS alike. Because the release binaries are
neither Apple-signed nor notarised, the cask carries a post-install hook that
strips the `com.apple.quarantine` xattr, without which the first run on macOS
dies with "cerberus is damaged and can't be opened".

The tap holds a SINGLE `cerberus` cask, so `skip_upload` is templated on
`RELEASE_IS_LATEST` — the highest-stable-tag signal `release.yml` computes after
pushing the tag — and only a stable release on the newest line ever writes it.
Neither an `rc.*` nor a maintenance backport touches the tap: a backport is not a
prerelease, so the bare `skip_upload: auto` that predated this let v1.12.1
overwrite v1.13.0's formula and downgrade every `brew install`. The block states
`directory: Casks`, which is both goreleaser's default and where Homebrew
resolves a tap's casks from; it is written out rather than inherited because
`brew-smoke.mjs` pins the same path, and a silent drift between the two would
leave the smoke reading a file no release writes.

Two prerequisites make the push work, and both are one-time:

1. The tap repo `tsouza/homebrew-tap` exists (public).
2. A PAT with push (`contents: write`) access to that tap is stored as the
   `HOMEBREW_TAP_GITHUB_TOKEN` repository secret on `tsouza/cerberus`. The
   default workflow `GITHUB_TOKEN` **cannot** push to another repo, so this
   secret is mandatory — without it the cask push on the next stable release
   fails.

The `brew-smoke` job closes the loop on both of those prerequisites. It reads the
tap's `Casks/cerberus.rb` through the API *before* touching `brew`, because a
deleted `homebrew_casks:` block or an expired `HOMEBREW_TAP_GITHUB_TOKEN` leaves
a stale cask that an install would happily consume. A stable release must find
the cask declaring exactly the version that just shipped; it then installs it and
asserts `cerberus --version` equals that version and that two offline verbs
(`migrate schema`, `config-docs -check`) work from the installed binary. The
install names `--cask` explicitly, so if the tap ever serves a same-named formula
again the job fails loudly instead of installing that one and calling the cask
healthy. The two shapes that write no cask are **not** skipped — each takes the
opposite assertion. An `rc.*` must have written none, so a cask declaring the
prerelease version is a reported regression; a release that is not the highest
stable tag must have left a strictly NEWER cask in place, so a tap that has
fallen back to the backport's own version is one too. The job runs after
`publish` because
`brew install` downloads the release tarball, which 404s while the release is
still a draft, and it runs on `macos-latest` because the Ubuntu runner image
ships no Homebrew at all. That the cask also installs under Linuxbrew is a
property of the artifact, not something CI exercises.

`brew-smoke` fires exactly once, inside the release run, which leaves one thing
uncovered: it cannot re-check an *already-published* release. `rerun-failed-jobs`
replays the workflow as it existed at the release commit, so a correction to the
job — a different runner, a different tap path — can never be exercised against
the release that needed it, and nothing here would notice a cask rotting
after publish (someone edits the tap, an asset is deleted, a checksum stops
matching). The `brew-verify` workflow covers that: same `brew-smoke.mjs`, same
assertions, but `workflow_dispatch` (with an optional bare `version`, defaulting
to the HIGHEST stable release rather than the most recently published one — a
backport publishes after the line above it) plus a weekly cron. It checks out the
*tag* it is verifying, because `config-docs -check` compares the installed
binary's config registry against the working tree's `docs/configuration.md` —
against `main` it would red on unreleased doc drift that is not a defect.

#### Maintenance lines (hotfix backports)

Beyond the main line, a hotfix can be cut on a **maintenance line** —
`release/<major>.<minor>.x` (e.g. `release/1.4.x`, `release/1.3.x`). The
maintainer cherry-picks the fix straight onto the branch and pushes; `release.yml`
also triggers on `release/*.x` pushes (the `.x` glob deliberately excludes the
main release PR branch shape `release/v1.5.0-chart-0.6.4`). The same per-line
version gates decide what to publish, and because the gates are
absence-keyed (tag-absent / OCI-absent, not newest-wins) a hotfix older than the
latest tag — `v1.4.1` cut after `v1.5.0` — still publishes. A maintenance push
has no PR gate at all, so the `preflight` job (`release-preflight.mjs`) adds two
rules on this path that the main path does not need: the pushed commit must be
the **branch tip**, and the line must still be **inside the support window**. The
required-set check is the same on both paths — every lane in
`RELEASE_REQUIRED_CHECKS` must have posted a green check-run on the commit. That
is why `migration-e2e.yml` push-triggers on `release/*.x` as well as `main`: a
required lane that never runs on the branch would block every hotfix release.

### Release support window / EOL policy

Cerberus maintains the **latest 3 minor release lines**: the current minor plus
the two prior. When a new minor ships, the line that becomes **3 minors behind**
the current minor reaches **end-of-life (EOL)**. An EOL line:

- gets **no further hotfixes**;
- has its `release/<major>.<minor>.x` maintenance branch **deleted
  automatically** when the new minor ships (the `eol-retire` job — see below);
- has its maintenance-line **publish/CI disabled** (the `preflight` gate refuses
  to publish a push on an out-of-window line — see below).

What stays: the **version tags and GitHub Releases** for EOL versions **remain
available** — only the future-hotfix branch is removed. Already-published images,
charts, and binaries are never unpublished.

**Worked example.** At **v1.5.x** current, the supported lines are
`release/1.5.x`, `release/1.4.x`, and `release/1.3.x`. Shipping v1.5.0 retired
`1.2.x` and older: `release/1.2.x` was deleted, `v1.2.*` tags and Releases stay
up. `1.4.x` and `1.3.x` remain supported and keep taking backports.

**Enforcement.** The support window is enforced on both halves of the EOL
policy, sharing one piece of window math
(`.github/scripts/release-preflight.mjs`, `SUPPORTED_MINOR_LINES` — single
source of truth):

- **Passive (publish refusal).** The maintenance-release `preflight`
  (`supportWindowProblem`) refuses a push to a `release/<major>.<minor>.x` line
  that is 3+ minors behind the current minor (derived from the stable `v*` tag
  set) — **before** any artifact publishes, independent of how green the commit
  is. An out-of-window line takes no further hotfixes.
- **Active (branch retirement).** When a NEW minor actually ships, the
  `eol-retire` job in `release.yml` deletes the maintenance branch that just
  fell out of the window **automatically** — no manual maintainer step. It runs
  only after a successful new-version publish, computes the line via
  `retireLineForPublish` (the same `SUPPORTED_MINOR_LINES` window: publishing
  `1.6.0` retires `release/1.3.x`), and deletes that `release/X.W.x` branch iff
  it exists. Guards: it retires **at most one** line and **only on a minor open**
  (`X.Y.0`, `Y>0`) — patches, major bumps, stable backports, and prereleases
  retire nothing; it deletes **only** a provably out-of-window branch that
  exists (idempotent — an already-absent branch is a clean no-op), with a
  `supportWindowProblem` cross-check before the destructive call; and it is
  **fail-open** — the release has already published, so any deletion failure
  (token, protection, network) logs loudly and the step still succeeds,
  leaving a one-line manual `git push origin --delete release/X.W.x` as the
  fallback. The job needs `RELEASE_PAT` — see the ruleset note below for why
  `contents:write` alone is not enough.

The maintenance branches are **not** unprotected. The repository ruleset
*"release maintenance lines"* targets `refs/heads/release/*.x` and is `active`:

- `deletion` — the branch cannot be deleted.
- `non_fast_forward` — no force-push; history is append-only.
- `required_status_checks` — 12 contexts must pass before a backport merges:
  `check`, `lint`, `forbid-skip`, `probe`, `roundtrip (promql)`,
  `roundtrip (logql)`, `roundtrip (traceql)`, `chart-validate`, `coverage`,
  `mutation`, `profile`, and
  `property (PromQL + LogQL + TraceQL, rapid N=500)`.

There is no `creation` rule, so cutting a **new** line (`git push origin
v1.11.1^{}:refs/heads/release/1.11.x`) needs no bypass. The single bypass actor
is the `admin` RepositoryRole in `always` mode, which is why `eol-retire` needs
`RELEASE_PAT`: an admin-owned PAT inherits the admin role and bypasses the
`deletion` rule (GitHub records this as `Bypassed rule violations`), whereas the
default `GITHUB_TOKEN` acts as `github-actions[bot]` — write, never admin — and
is refused. The required-check set is deliberately lighter than `main`'s: the
`compatibility/*` lanes are not gated on maintenance lines, because a backport
must stay cheap enough to ship. The substrate lanes (`compose-smoke`,
`dashboard`) gate no pull request anywhere — they are release gates, enforced
by release.yml's preflight on the commit being published.

EOL retirement never unpublishes anything: the `v<major>.<minor>.*` git tags and
their GitHub Releases — and the already-pushed images, charts, and binaries —
stay available; only the future-hotfix branch is removed.

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

## query_log mining

Every data-plane query cerberus runs stamps the ClickHouse `query_id` with a
per-dispatch id of the form `<trace id>-<span id>-<counter>` (always on, no
flag). The cerberus trace id is the leading **prefix**, so each row in
`system.query_log` still joins back to the cerberus trace — while the span id
and a process-global counter keep the id **unique per CH dispatch**, so the
many concurrent queries a single trace fans out (a Grafana dashboard loading
panels, a vector-join / fan-out PromQL) never collide on the same `query_id`
(which ClickHouse would reject with code 216, "Query with id = X is already
running"). With the optional DARK flags from
[`configuration.md`](configuration.md#clickhouse-optimizations), operators also
get the join keys to cluster and rank cerberus's SQL by cost. The async
performance-corpus reconciler (`CERBERUS_CH_OPT_CORPUS_ENABLED`) automates
exactly this join — it records the same per-dispatch `query_id` cerberus stamps
and matches it back against `system.query_log`; see
[`clickhouse-optimizations.md`](clickhouse-optimizations.md#the-systemquery_log-performance-corpus-reconciler).

Join a cerberus trace to its ClickHouse execution (match on the trace-id
prefix — one trace maps to many per-dispatch `query_id`s):

```sql
SELECT query_id, query_duration_ms, memory_usage, read_rows, read_bytes, query
FROM system.query_log
WHERE type = 'QueryFinish'
  AND query_id LIKE '<cerberus trace id>-%'
```

Top query shapes by p99 latency (cluster by ClickHouse's normalized hash):

```sql
SELECT
    normalized_query_hash,
    count() AS runs,
    quantile(0.99)(query_duration_ms) AS p99_ms,
    any(query) AS sample
FROM system.query_log
WHERE type = 'QueryFinish' AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY p99_ms DESC
LIMIT 20
```

Top shapes by peak memory:

```sql
SELECT
    normalized_query_hash,
    count() AS runs,
    max(memory_usage) AS peak_bytes,
    any(query) AS sample
FROM system.query_log
WHERE type = 'QueryFinish' AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY peak_bytes DESC
LIMIT 20
```

With `CERBERUS_LOG_COMMENT_SHAPE=true`, every query also carries a compact,
literal-free cerberus shape id in `log_comment` (`cerb:<root>[;mod...]`), so you
can pivot on the cerberus-assigned shape rather than ClickHouse's literal-
sensitive hash — and filter to cerberus traffic with `log_comment LIKE 'cerb:%'`:

```sql
SELECT
    log_comment AS shape,
    count() AS runs,
    quantile(0.99)(query_duration_ms) AS p99_ms,
    max(memory_usage) AS peak_bytes
FROM system.query_log
WHERE type = 'QueryFinish'
  AND log_comment LIKE 'cerb:%'
  AND event_time > now() - INTERVAL 1 DAY
GROUP BY log_comment
ORDER BY p99_ms DESC
LIMIT 20
```

Condition-cache effectiveness (once `condition_cache` is enabled — `auto` turns
it on for servers >= 25.3):

```sql
SELECT
    any(log_comment) AS shape,
    sum(ProfileEvents['QueryConditionCacheHits']) AS cache_hits,
    count() AS runs
FROM system.query_log
WHERE type = 'QueryFinish'
  AND log_comment LIKE 'cerb:%'
  AND event_time > now() - INTERVAL 1 DAY
GROUP BY normalized_query_hash
ORDER BY cache_hits DESC
LIMIT 20
```

The async performance-corpus reconciler (`CERBERUS_CH_OPT_CORPUS_ENABLED`)
persists exactly this `(shape-id, opts, timings)` join to a durable JSONL sink
so the corpus survives `query_log` TTL eviction and is minable offline. See
[`clickhouse-optimizations.md`](clickhouse-optimizations.md#the-systemquery_log-performance-corpus-reconciler)
for its config and row shape.

## Admin commands

Cerberus has no embedded admin REPL. Schema operations are owned by
ClickHouse directly (run `clickhouse-client` against the cluster);
config changes happen by env-var update + process restart. The `gh pr
merge --auto --squash --delete-branch` flow is the source of truth
for operator-driven changes to the binary.
