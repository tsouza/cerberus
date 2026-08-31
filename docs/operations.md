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
layer maps that into `503` with a `Retry-After` so clients back off
instead of stacking inner-stage retries against a dead upstream. After
`CERBERUS_CH_BREAKER_OPEN_INTERVAL` the breaker admits exactly one
HALF-OPEN probe; a successful probe closes the circuit, a failed one
restarts the backoff. That interval is exactly what `Retry-After`
advertises: the tripped breaker stamps its own recovery interval on the
error it returns, so widening `CERBERUS_CH_BREAKER_OPEN_INTERVAL` to protect
a fragile ClickHouse moves the header with it instead of inviting clients
back while cerberus is still fast-failing. The header is whole seconds
(RFC 9110), rounded up and floored at `1`, so a sub-second interval still
asks for a back-off rather than an immediate retry. Pool-acquire timeouts, `MEMORY_LIMIT_EXCEEDED`
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
  short-circuit to `ErrCircuitOpen` → `503` + `Retry-After`, while Loki and
  Tempo keep their own CLOSED breakers and serve normally. One head's CH-path
  problem no longer 503s the other two.
- **`/readyz` stays green under a single head's storm, and names the tripped
  head.** The readiness probe pings through the dedicated `probe` breaker, which
  is driven ONLY by the low-rate, TTL-coalesced readiness pings — never by
  data-plane traffic. So a Prom-only storm 503s Prom queries while `/readyz`
  stays green and the pod is **not** evicted: it is still happily serving Loki
  and Tempo, and could serve Prom again within
  `CERBERUS_CH_BREAKER_OPEN_INTERVAL` once the HALF-OPEN probe recovers. The
  probe body still reports the tripped head — it carries a `heads` object with
  the live phase of every enabled head — so the fault is visible on the probe
  and not only in the breaker gauge. The pod goes unready when *every* enabled
  head is OPEN, which under the chart's split mode (one head per Deployment) is
  the single head that Deployment serves. A genuine total-CH outage also fails
  the readiness pings themselves, trips the `probe` breaker, and flips `/readyz`
  red → correct eviction. The probe breaker uses a slightly tighter default
  failure budget so a dead CH is reported red well inside the k8s
  `readinessProbe` eviction window even though it only sees the throttled probe
  stream. See [`health.md`](health.md#per-head-readiness).

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
`below-threshold`, `instant`, `instant-join`, `not-sliceable`, `high-D`,
`now64`, `grid-mismatch`, `incommensurate`, `scalar-heavy`,
`routing-disabled` (the reason recorded under `single`, where no threshold is
consulted at all), or `extraction-failed` (no grid carrier was measurable, so
the reported cost grid is zero for want of data rather than because the plan is
cheap). The header is **omitted**
for non-PromQL heads and when the solver is fully off (nil). It is purely
diagnostic — observe it to see what the solver would do (under `single`) or
did (under `auto`) without changing the wire body.

**All-or-nothing.** Whether a request is solved by route A or fanned out across
`K` shards, the client sees a single response. A shard failure surfaces as one
typed error (first-error-wins, cause-threaded), never a partial body. The
solver re-emits and re-executes per request — it never caches.

The remaining `CERBERUS_SHARD_*` / `CERBERUS_SOLVER_TIMEOUT` knobs — enumerated
with their defaults in
[`configuration.md`](configuration.md#schema-overrides-and-prometheus-resource-labels) —
tune the shard count, concurrency, per-request output cap, and
per-shard memory apportionment; their defaults are deliberately conservative
against over-routing (Grafana's auto-step makes `rate[5m] @ 15s` hit `F=20`,
which must NOT route at the default thresholds unless the total expansion is
spike-class).

**Failure-driven route memo (`CERBERUS_SOLVER_ADAPTIVE_ENABLED`, default
`true`).** The Planner's cost thresholds are static and can misclassify a
plan whose real, data-dependent cost only shows up at execution time.
`internal/routememo` (see
[`solver.md`](solver.md#failure-driven-route-memo)) retries a route-A
dispatch that fails on ClickHouse resource exhaustion once on route B, and
remembers the outcome against a literal-free fingerprint of the plan's cost
shape so a later cost-equivalent request routes directly instead of paying
the same failure again. It is on by default because it only ever turns a
FAILURE into a slower answer and can never change a result — it is what makes
`auto` mean "start on route A and escalate on real evidence" rather than
"guess up front and never learn". Set it `false` to keep dispatch
byte-unchanged. `CERBERUS_SOLVER_ROUTE_MEMO_ENABLED` is the soft-deprecated
spelling: it still applies, the new name wins when both are set, and setting
it logs a deprecation notice at startup. Two more knobs tune it:

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
  compose / e2e deployment runs **26.6** and the compatibility deployment and the
  chDB test substrate run **26.5**, both ABOVE that floor, so the native path is
  genuinely exercised on every substrate.
  The auto-picker gates on this floor automatically — it enables
  `ts_grid_range` only when the probed server is ≥ 25.9, so a connected older
  server keeps the fan-out and never diverges. (Force-enabling via the legacy
  `=true` flag against a < 25.9 server is still rejected at startup per mode.)
  The experimental ClickHouse setting
  `allow_experimental_time_series_aggregate_functions=1` is sent **only on the
  queries that actually use the native node** (cerberus detects a
  `RangeWindowGridNative` in the emitted plan and stamps the setting per-query), so
  enabling the flag never adds an unknown setting to unrelated queries.
- **The server must permit that experimental setting.** Meeting the 25.9 floor
  is necessary but not sufficient: a hardened ClickHouse profile that
  constrains/pins `allow_experimental_time_series_aggregate_functions`, or a
  readonly user, will reject the per-query stamp with
  `SETTING_CONSTRAINT_VIOLATION` / `READONLY`. cerberus **probes this**
  (a capability canary alongside the version probe) and gates the
  native family on the verdict: under `auto` a forbidden server silently falls
  back to the fan-out with a `WARN`; an explicit `ts_grid_*` (or the legacy
  force-enable) on a forbidden server is FATAL under `enforcing` and WARN+skip
  under `permissive` — exactly the version-floor semantics. See
  [`clickhouse-optimizations.md`](clickhouse-optimizations.md#capability-probe-experimental-ts_grid-setting).
- **Scope: `rate` (this section) plus its later query_range siblings.**
  `increase` / `delta` / `changes` / `resets` / `deriv` / `predict_linear` /
  `irate` / `idelta` each gained their own native `ts_grid_*` feature under
  the same 25.9 floor (see `clickhouse-optimizations.md` for the full table);
  every non-PromQL head stays unaffected by any of them. **Instant queries**
  (bare `rate(m[5m])` with no `query_range`) were unaffected by the WHOLE
  family until `ts_grid_instant` (floor 26.5, opt-in — cerberus issue #2748):
  with it enabled, an eligible instant `rate` / `changes` / `resets` / `deriv`
  / `predict_linear` query ALSO rides the native aggregate, fed a degenerate
  one-point grid instead of query_range's materialised one — the same flat
  per-series memory the matrix shape gets, in place of the alerting/
  recording-rule path's unbounded `groupArray` over the lookback window.
  `increase` / `delta` stay fan-out-only in instant mode (their own instant
  coverage is a deferred follow-up, not a technical gap).
- **The fan-out remains byte-for-byte available.** Pinning `ts_grid_range` off
  (an explicit list omitting it, or the legacy `=false`) restores the
  established fan-out exactly; on a < 25.9 server it is the only path. Every
  existing golden, the compat 747/747 corpus, and the compose / e2e lanes are
  structurally the fan-out shape.

**Parity.** Validated on the chDB substrate (26.5) by a dual-emit test
(`internal/chsql/range_window_grid_native_chdb_test.go`) that runs the fan-out and
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
The e2e manifest (`test/e2e/k3s/cerberus-values.yaml`) sizes the pod at 2560Mi /
`GOMEMLIMIT=2048MiB` for the promote-all default under the full dashboard
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

- The ClickHouse driver connection pool (`internal/chclient`), and the
  per-head circuit breakers that front it.
- The schema configuration (`internal/schema`, immutable after startup).
- A short-TTL cache inside the readiness probe handler
  (`internal/api/health`) so probe traffic does not amplify into
  ClickHouse pings.
- The resolved ClickHouse capability set (`internal/chopt`), refreshed on the
  re-probe cadence so the strategies a query dispatches through describe the
  server that is actually connected.

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

## Security posture

Cerberus ships **no authentication, no authorization, and no tenant
isolation**. That is a deliberate scope decision — the same one the
listener makes about TLS — but it is load-bearing for how you deploy
the process, so it is spelled out here rather than left to be inferred
from the absence of a `CERBERUS_AUTH_*` knob. Vulnerability reporting
and the in-scope threat model live in
[`SECURITY.md`](../SECURITY.md).

**Everything on the listener is open to whoever can reach the port.**
There is no privileged route and no unprivileged one: the three query
heads, `/healthz`, `/readyz`, and `/info` all answer any caller that
completes a TCP connection. Treat `:8080` as an internal port and put
the authn boundary in front of it — a TLS terminator plus an
authenticating proxy (mTLS, `oauth2-proxy`, ingress basic-auth, a
service mesh), and a `NetworkPolicy` (or security group) that lets only
that proxy dial the port. The chart's `ingress.enabled=true` publishes
the port unauthenticated unless you supply the annotations that
authenticate it; nothing in cerberus will object.

**No tenant header is read.** `X-Scope-OrgID` is ignored on every
serving path, so a multi-tenant Grafana pointed at one cerberus gets one
undivided view of the configured ClickHouse database — every tenant's
metrics, logs, and traces, whichever tenant asked. (The
`CERBERUS_VERIFY_REF_*_ORG_ID` settings are reference-side only: they
tenant the *incumbent* Loki / Tempo that `cerberus migrate verify`
queries, not cerberus.) Per-tenant separation has to come from the
deployment: one cerberus per tenant, each with its own ClickHouse user
and database, or a proxy that enforces the split before the request
arrives.

**Give the gateway a read-only ClickHouse user.** The query path only
ever issues `SELECT`, so the credential in `CERBERUS_CH_PASSWORD` needs
nothing but read access to the telemetry database. Provision DDL rights
separately for the paths that actually create objects —
`CERBERUS_AUTO_CREATE_SCHEMA` / `CERBERUS_AUTO_CREATE_DATABASE` and
`cerberus migrate` — and prefer running those as a one-shot job under a
different user rather than granting the long-lived gateway the ability
to write. Note the interaction with the native `ts_grid_*` family
documented under [Configuration](#configuration): a `readonly` profile
that forbids the per-query experimental setting makes cerberus fall
back to the fan-out shape.

**Error bodies are verbatim.** A failed query's `{status:"error",
error:"…"}` envelope carries the underlying ClickHouse error text,
which can name tables and columns and quote the emitted SQL. That is
useful in a trusted operator context and is reconnaissance material in
an untrusted one — another reason the port belongs behind a boundary.

**The diagnostic surface is opt-in, except `/info`.**
`/debug/pprof/*` is mounted only under `CERBERUS_DEBUG_PPROF=true` (and
logs a `WARN` for as long as it is on), so enable it transiently and
turn it back off. `/info` has no switch: it always answers `200` with
the build identity plus the configured ClickHouse address and database
(never credentials) — see [`health.md`](health.md). Both are internal
surfaces; neither should be routable from outside the boundary.

**CORS and the tail WebSocket.** Cerberus emits no
`Access-Control-Allow-*` headers, so a browser refuses cross-origin XHR
against it. The exception is Loki's tail WebSocket
(`/loki/api/v1/tail`), which accepts every `Origin` — upstream Loki's
posture, and the one cerberus matches, since the endpoint is consumed
same-origin by Grafana or out-of-browser by `logcli`. WebSocket
handshakes are not subject to CORS, so a proxy that authenticates with
cookies should check `Origin` itself; one that authenticates with a
header or mTLS is unaffected.

**The passwords in the repo are fixtures.** `docker-compose.yml`,
`test/e2e/**`, and the integration tests all use `cerberus` (and
Grafana's `admin`) as literal dev credentials for throwaway local
stacks. They are not defaults the shipped binary or the chart carries:
the chart takes `clickhouse.existingSecret` (or synthesises a Secret),
and the process reads the credential from the environment. Copying a
compose fixture into a real deployment is how those strings become a
problem.

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

Cerberus's `internal/api/admit` package fronts its routes with counted
semaphores. Each accepts an explicit integer cap or a boolean alias
(`true` = the default cap, `false`/`0` = unlimited), so a plain chart
bool and a precise operator cap both work. An occupant above the cap is
rejected with HTTP 503 + `Retry-After: 1` so well-behaved clients back
off and ClickHouse stays out of overload.

There are **four** budgets, and they are not four heads — they are three
request budgets plus one for long-lived streams:

| Knob                   | Default | Bounds                              | A slot is held for                                   |
| ---------------------- | ------- | ----------------------------------- | ---------------------------------------------------- |
| `CERBERUS_ADMIT_PROM`  | 64      | every Prom route                    | the request (milliseconds)                           |
| `CERBERUS_ADMIT_LOKI`  | 64      | every Loki route **except** `/tail` | the request (milliseconds)                           |
| `CERBERUS_ADMIT_TEMPO` | 32      | every Tempo route (HTTP + gRPC)     | the request (milliseconds)                           |
| `CERBERUS_ADMIT_TAIL`  | 16      | `GET /loki/api/v1/tail` only        | **the whole session — until the client disconnects** |

Tempo's cap is half of Prom / Loki because trace queries (search +
tag-value scans + per-trace span fetches) are the heaviest per-call.

The tail budget is separate because it bounds a different quantity. The
three request budgets cap *concurrency*: slots churn in milliseconds, so
a cap of 64 sustains far more than 64 queries per second. The tail
budget caps *concurrent live-tail sessions*: a `/tail` slot is taken at
the WebSocket upgrade and returned only when the client disconnects,
which is minutes or hours later — nothing reclaims it in between, and
`CERBERUS_HTTP_WRITE_TIMEOUT` defaults to unlimited precisely so a tail
can stream indefinitely. Read the tail cap as "how many Grafana Live-tail
panels may point at one replica at once", and size it against the
per-session steady load: each tail re-queries ClickHouse about once a
second, so the cap is also tailing's background query rate per replica.

Sizing them separately is what keeps the two from interfering. Were
`/tail` drawn from the Loki request budget, `CERBERUS_ADMIT_LOKI`
concurrent Live-tail sessions would occupy every Loki slot indefinitely
and every subsequent `/query`, `/query_range`, `/labels`, `/series`,
`/patterns` and Drilldown probe on that replica would 503 until a tail
client disconnected. Occupancy would only ratchet one way, and the
symptom — "the Loki datasource is down", on a healthy pod with no
elevated CPU — points nowhere near the cause. Exhausting the tail budget
now costs ordinary Loki queries nothing: the two semaphores are
independent, and a saturated tail budget rejects only new `/tail`
upgrades.

To tell which budget rejected a request, group the rejection counter by
the `budget` label: `sum by (cerberus_ql, budget, reason)
(rate(cerberus_admit_rejected_total[5m]))` splits `budget="request"` from
`budget="tail"`. Sustained `budget="tail"` rejections mean live-tail
demand exceeds `CERBERUS_ADMIT_TAIL`; sustained `budget="request"`
rejections on `cerberus_ql="logql"` are ordinary query saturation and are
now unaffected by how many tails are open.

The tail budget is independent of `CERBERUS_ADMIT_LOKI` in both
directions, including when that knob is off: a replica running
`CERBERUS_ADMIT_LOKI=0` (unlimited Loki requests) still admits at most
`CERBERUS_ADMIT_TAIL` concurrent tail sessions, because "unlimited
requests" says nothing about how many unbounded-duration streams the
replica should carry. Set `CERBERUS_ADMIT_TAIL=0` to opt out of the tail
cap as well, or `CERBERUS_ADMIT_DISABLED=true` to opt out of both.

A tail refused by the tail cap upgrades to a WebSocket first and is then
closed with a `1013` (`Try Again Later`) close frame, the same shape
reference Loki answers its own `querier.max-concurrent-tail-requests`
with — every other admitted route refuses at the HTTP upgrade instead
(`503` + `Retry-After: 1`), but a WebSocket client has no way to read an
HTTP header once the handshake has already gone through, so `/tail`'s
rejection has to live at the WebSocket layer to be actionable
(issue [#2048](https://github.com/tsouza/cerberus/issues/2048)).

`CERBERUS_ADMIT_DISABLED=true` removes admission control entirely, on
every budget at once — useful for local development where artificial caps
mask real concurrency bugs.

### Workload scheduling (cerberus issue #2785)

Everything above this line is **client-side** admission: cerberus bounding
its own concurrency, its own per-query memory, its own per-query wall clock.
None of it can stop a heavy read burst from starving ClickHouse's OWN
background merge/mutation threads at the CPU/IO level on a node that also
takes OTel-collector ingest writes — or the reverse, an ingest spike starving
concurrent dashboard queries — because both traffic classes are, by default,
just threads competing for the same cores and the same disk with no
scheduler between them. That is a **server-side** resource-scheduling
problem, and ClickHouse's own answer is `CREATE WORKLOAD` / `CREATE
RESOURCE` (the [workload scheduling](https://clickhouse.com/docs/operations/workload-scheduling)
feature family: weighted fair-share CPU-slot and IO-byte scheduling across
named workload trees).

**Verified directly against a real ClickHouse 26.6** (not merely cited from
docs — this repo's session found more than one ClickHouse doc claim not hold
up against the actually-deployed version): `CREATE RESOURCE cpu (MASTER
THREAD, WORKER THREAD)`, `CREATE RESOURCE io_default (READ DISK default,
WRITE DISK default)`, and a `CREATE WORKLOAD` hierarchy with per-child
`weight` all worked exactly as documented, and `system.scheduler` showed the
resulting weighted fair-share nodes live and active for both resources. This
is production-quality, GA machinery on the deployed version, not an
experimental flag.

**The recipe** (run by the ClickHouse operator, against the operator's own
cluster — see "Ownership" below for why cerberus does not run this itself):

```sql
-- One resource per scheduled dimension. WORKER THREAD/MASTER THREAD cover
-- CPU slot scheduling; READ/WRITE DISK covers IO-byte scheduling for a
-- named disk (substitute the deployment's real disk name, e.g. an S3 disk).
CREATE RESOURCE cpu (MASTER THREAD, WORKER THREAD);
CREATE RESOURCE io_default (READ DISK default, WRITE DISK default);

-- A root that bounds total CPU slots, then two children under it.
CREATE WORKLOAD all SETTINGS max_concurrent_threads = 16 FOR cpu;

-- Naming this workload literally `default` is deliberate, not cosmetic:
-- ClickHouse's `merge_workload` / `mutation_workload` server settings both
-- default to the literal string "default", so every background merge and
-- mutation is automatically scheduled through a workload named `default`
-- the moment one exists — no config.xml edit, no server restart required.
CREATE WORKLOAD `default` IN `all` SETTINGS weight = 6;

-- cerberus's own read traffic. weight is RELATIVE to its siblings under the
-- same parent (fair-share, not a percentage): 6:1 here means merges/
-- mutations get roughly 6x the CPU slots and IO bytes of cerberus's own
-- queries whenever both are actually contending for capacity.
CREATE WORKLOAD cerberus_queries IN `all` SETTINGS weight = 1;
```

Then tell cerberus to tag its own query traffic with the workload it should
ride: `CERBERUS_CH_QUERY_WORKLOAD=cerberus_queries` (see
[`configuration.md`](configuration.md)). Cerberus stamps the ClickHouse
`workload` per-query setting on every dispatched query when this is set —
boot-probed against the connected server (a server too old to know the
`workload` setting, or a hardened profile that pins/forbids it, degrades to
FATAL under the default `CERBERUS_CH_OPTIMIZATIONS_MODE=enforcing`, or a
`WARN` + no-op under `permissive`; the knob's mere absence is already a full,
byte-identical-to-before no-op).

**The weighting footgun** (the issue's own risk note: "misconfigured shares
can starve merges" — this is the concrete shape of that). Stock ClickHouse,
with NO workload objects defined at all, already runs background merges
through their own dedicated thread pool (`background_pool_size`) entirely
separate from the query-concurrency-control thread pool queries draw from —
so out of the box, merges are not queued behind queries in any scheduler,
they just time-slice at the OS level. The moment an operator creates a
workload literally named `default`, merges are pulled OUT of that separate
pool and INTO the SAME weighted CPU/IO scheduler queries now use — which is
a NET REGRESSION for merges if that `default` workload is left at an
unweighted or low-weighted default alongside a heavier query workload.
Standing up workload scheduling for the query side only earns the intended
protection when the merge-side workload is weighted (or bounded) to actually
win contention against the query side — never assume enabling the machinery
alone is protective; a naive or forgotten weight is worse than not enabling
it at all.

**Measured under combined load** (real measurement against the local
`clickhouse/clickhouse-server:26.6` compose service, not an estimate): a
synthetic OTel-collector-style ingest loop — continuous small (500-row)
batched `INSERT`s into a MergeTree table seeded to 3M rows / dozens of active
parts — ran for 60s concurrently with 16 parallel heavy `GROUP BY` +
`quantiles()` read queries (`max_threads=8` each), with and without the
recipe above (`max_concurrent_threads=8` on `all`, `default` weight 6 vs
`cerberus_queries` weight 1). With isolation configured, ingest p95/p99
latency improved (~135ms → ~124ms p95, ~160ms → ~149ms p99) and insert
throughput rose slightly (~9.7/s → ~10.1/s), at the cost of ~26% fewer
completed read queries in the same window (476 → 352) — the intended
tradeoff, reads yielding CPU/IO share to protect ingest. The effect size was
modest on this single 8-core dev box with local disk: stock ClickHouse's
separate merge thread pool (above) already absorbs a fair amount of
contention before any scheduling is configured, so the gap only widens under
genuinely saturated CPU/IO — the production case this feature targets (a
shared node under real dashboard-storm + ingest-spike load, or S3-backed
merge IO competing with query S3 reads) is harder to reproduce on a
lightly-loaded local box and is where the isolation is expected to matter
most.

**Ownership: this is a documented operator recipe, not cerberus-provisioned
DDL.** `WORKLOAD` / `RESOURCE` are server objects, and in cerberus's common
deployment shape it is a stateless gateway pointed at a
bring-your-own ClickHouse cluster the operator owns and may already run
other, non-cerberus workloads against — auto-issuing `CREATE
WORKLOAD`/`CREATE RESOURCE` DDL, or `ALTER USER`/`CREATE SETTINGS PROFILE`
against that cluster, from inside cerberus would be a real ownership and
blast-radius overreach (and `merge_workload`/`mutation_workload` affect
EVERY table on the cluster, not just cerberus's own). Note also that a
ClickHouse user provisioned via `CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD`
env-var (XML `users.xml`) config — as the demo compose stack's `clickhouse`
service is — lives in a read-only access storage from SQL's own
perspective: `ALTER USER ... SETTINGS PROFILE ...` fails with
`ACCESS_STORAGE_READONLY` against it, so a settings-profile-based wiring is
NOT reliable in general. `CERBERUS_CH_QUERY_WORKLOAD` sidesteps this
entirely: it rides the SAME per-query settings-stamping path cerberus
already uses for `ts_grid_range` and the query result cache, needing no
ClickHouse RBAC privilege at all on the connection cerberus uses.

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

| Configuration                                | Status                         | How it is validated                                                                  |
| -------------------------------------------- | ------------------------------ | ------------------------------------------------------------------------------------ |
| S3 / MinIO, single-node                      | **Runtime-proven**             | k3d e2e (`bwc-minio` lane): live MinIO, real read/write, placement asserted          |
| S3 on real AWS (virtual-hosted + IRSA)       | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-aws-values.yaml` renders; no live-AWS run               |
| GCS (S3-compat HMAC endpoint)                | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-gcs-values.yaml` renders; no live-GCS run               |
| Azure Blob (account key or managed identity) | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-azure-values.yaml` renders; no live-Azure run           |
| IRSA / GKE / AKS workload identity           | Render / kubeconform-validated | env / SA annotations render; no live cloud-identity run                              |
| Multi-replica + Keeper (ReplicatedMergeTree) | Render / kubeconform-validated | `deploy/helm/cerberus/ci/bwc-replicated-values.yaml` renders; no live multi-node run |

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

Retention is written in the duration grammar every cerberus duration knob
shares — `90d`, `2w`, `1y`, or the equivalent `2160h` form. `d`/`w`/`y` are
fixed (24h / 7d / 365d), so a whole number of weeks renders as
`toIntervalWeek(N)` and everything else as the coarsest exact ClickHouse
interval (`toIntervalDay`/`Hour`/…). Calendar months and calendar-aware
years are intentionally not supported: they are variable-length and a `1y`
TTL is exactly 365 days, not a leap-aware calendar year.

Auto-create also reuses the **same** table names the query heads read
(`CERBERUS_SCHEMA_*_TABLE`), so a renamed table is created and queried
consistently rather than silently diverging onto the upstream defaults.

### Hot/cold storage tiering

`CERBERUS_SCHEMA_STORAGE_POLICY` puts a MergeTree `storage_policy` on every
auto-created table, and a policy only declares which volumes a table **may**
use. Parts are written to its first (hot) volume and stay there until
retention deletes them, so a multi-volume hot/cold policy tiers nothing on its
own — the expensive volume keeps data that should have aged onto the cheap one,
and the storage bill is the only symptom.

`CERBERUS_SCHEMA_TIER_VOLUME` plus `CERBERUS_SCHEMA_TIER_AFTER` emit the rule
that moves it. The move age is per signal exactly as retention is
(`CERBERUS_SCHEMA_TIER_AFTER_{METRICS,LOGS,TRACES}`, a zero inheriting the
global), and both actions land in the one `TTL` clause ClickHouse allows:

```sql
TTL toDateTime(Timestamp) + toIntervalDay(7)  TO VOLUME 'cold',
    toDateTime(Timestamp) + toIntervalDay(30) DELETE
```

A configuration that would be accepted while doing nothing is rejected at
startup: a move age with no `CERBERUS_SCHEMA_TIER_VOLUME`, a volume with no
move age, or a move age at or past the same signal's retention (the part would
be deleted before it ever moved). What can only be judged against the live
server — a policy the server does not define, a tiering volume the policy does
not have, or a multi-volume policy with no tiering rule at all — is reported by
the boot **requirements check** as a warning naming the exact fix. Those are
warnings rather than boot failures because storage layout is a cost property of
the tables, not a correctness property of the gateway.

### Local filesystem cache (cerberus issue #2780)

Production is explicitly a **single node with S3-backed storage** (see
"Helm: bundled ClickHouse on object storage" above), so a cold dashboard scan
is latency-dominated by object-store round trips, not CPU. ClickHouse's local
filesystem cache — a disk-backed cache of the raw S3 byte ranges a MergeTree
read touches, keyed by object path + offset — sits directly on that leverage:
a part whose ranges are already cached serves from local disk instead of
re-fetching from S3 on every scan, which matters most for cerberus's own
access pattern of the same recent partitions being rescanned by every
dashboard refresh.

**This is a SERVER-CONFIG concern, not a `chopt` stamp.** The per-query
toggle (`enable_filesystem_cache`) already defaults to `1` on every
ClickHouse version cerberus supports — confirmed live via chDB (ClickHouse
26.5.1.1) rather than assumed, see "Audited, not adopted" in
[`docs/clickhouse-optimizations.md`](clickhouse-optimizations.md) — so there
is nothing for cerberus to stamp per query. What the per-query default cannot
do is create the cache: a `cache`-type disk has to exist in the server's
`storage_configuration` before `enable_filesystem_cache=1` has anywhere to
write, exactly the gap
[ClickHouse's own "using local cache" doc](https://clickhouse.com/docs/operations/storing-data#using-local-cache)
describes. That disk definition, and the operator work below, lives entirely
in the ClickHouse server config cerberus does not template — cerberus's own
knob is `CERBERUS_SCHEMA_STORAGE_POLICY` (see "Hot/cold storage tiering"
above), which only SELECTS which already-defined server-side policy new
tables use.

**Wiring a cache disk over the S3 disk**, in the ClickHouse server's own
`config.xml`/`config.d` — the shape the doc above walks through, wrapping
whatever `<s3_disk>` the "bundled ClickHouse on object storage" section
already defines:

```xml
<storage_configuration>
  <disks>
    <s3_cache>
      <type>cache</type>
      <disk>s3_disk</disk>
      <path>/var/lib/clickhouse/disks/s3_cache/</path>
      <max_size>107374182400</max_size> <!-- 100 GiB; see sizing below -->
    </s3_cache>
  </disks>
  <policies>
    <s3_main>
      <volumes>
        <main>
          <disk>s3_cache</disk>
        </main>
      </volumes>
    </s3_main>
  </policies>
</storage_configuration>
```

Point `CERBERUS_SCHEMA_STORAGE_POLICY` at the wrapping policy name (`s3_main`
above) so cerberus's auto-created tables land on the cache-backed disk
instead of the raw S3 one directly.

**Sizing the cache disk for the single-node profile.** The cache only pays
off across the "hot" window a dashboard actually rescans — sized past that
window it just holds cold, never-revisited ranges. A first-pass estimate
uses the same production figures already measured elsewhere in this doc
("Metadata-enumeration projections", "Ongoing ingest cost"): ~2,824 rows/s
sustained ingest against a measured ~4 billion rows / ~140 GiB on-disk
(compressed) ratio is ~35 bytes/row, so:

```text
hot_window_bytes ≈ sustained_rows_per_sec × bytes_per_row × hot_window_seconds
                ≈ 2,824 × 35 × hot_window_seconds
```

A 24-hour hot window (the busiest Grafana panels' typical lookback) is
therefore ~**8.3 GiB/day**; a 7-day hot window (weekly comparisons, longer
on-call lookback) is ~**58 GiB**. Recompute with the deployment's OWN
sustained ingest rate and per-row size (`system.parts` on the live table) —
the figures above are this doc's own measured production numbers, not a
universal constant — and size the disk with headroom past the busiest
window an operator's dashboards actually reissue, not the full retention
window: cold, rarely-revisited history gains little from caching and would
just evict the hot ranges that pay for themselves.

**Warm-up for the hot recent window.** ClickHouse's cache metadata persists
across a graceful restart (`load_metadata_threads` reloads it at startup), so
a routine restart does not empty it. It DOES start genuinely cold the first
time a cache disk is wired in, and a cache freshly resized larger has empty
new capacity until something touches it. In either case the fastest path to
a warm cache is deliberately re-issuing the query shapes that would
otherwise warm it organically off real traffic: replay a representative
sample of recent dashboard queries (or, simplest, a broad `SELECT count()`
over the last `hot_window`'s worth of each fact table) once after standing
up or resizing the cache, rather than letting the first wave of real user
traffic pay the cold-scan cost.

**Observability.** `GET /info`'s `filesystemCache` object reports whether a
cache is configured at all (`configured`, `caches`) plus its aggregate
configured capacity and live occupancy (`maxSizeBytes`, `currentSizeBytes`,
`currentElements`), read live from `system.filesystem_cache_settings` on
every poll — so "is the cache actually there, and how full is it" is
answered without a ClickHouse shell. `configured: false` on a deployment that
believes it wired a cache disk is the single most actionable signal this
section can give: it means the `storage_configuration` above never took
effect (a config-reload miss, a typo'd disk name in the policy, or the
policy itself not yet applied via `CERBERUS_SCHEMA_STORAGE_POLICY`).

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

### AggregationTemporality skip index

Auto-create also installs a small **data-skipping index** on the sum and
histogram fact tables:

```sql
ALTER TABLE <db>.otel_metrics_sum       ADD INDEX IF NOT EXISTS idx_agg_temporality AggregationTemporality TYPE minmax GRANULARITY 1
ALTER TABLE <db>.otel_metrics_histogram ADD INDEX IF NOT EXISTS idx_agg_temporality AggregationTemporality TYPE minmax GRANULARITY 1
```

Unlike the metadata projections above, this is a `minmax` **skip index**, not
a stored second copy of the table — it costs a small amount of per-granule
`[min, max]` metadata, not additional table storage.

**Why it exists (issue #2458).** A range-mode `rate()`/`increase()` window
over a temporality-bearing counter can split into a native
`timeSeriesRateToGrid` arm (fed only non-DELTA rows) and a fan-out arm (fed
only DELTA rows) — see `NativeRateLowerer.LowerRate` in
`internal/promql/lower_strategy.go`. Both arms scan the SAME base table with
the SAME `MetricName`/`Attributes` predicate, differing only in a trailing
`AggregationTemporality` conjunct. That column is not part of the table's
`ORDER BY` (`MetricName, Attributes, ServiceName, TimeUnix`), so without a
skip index ClickHouse cannot prune a single granule on it and reads every
matching row from BOTH arms — a confirmed, reproducible 2.00x `read_rows`
ratio against table size, measured on real production-shaped data. Real OTel
deployments set `AggregationTemporality` once per exporter configuration, so
a given series' samples land in temporality-homogeneous runs almost always;
the minmax index lets ClickHouse recognize a homogeneous granule and skip it
entirely for whichever arm's predicate does not match, without requiring any
change to the plan shape or the table's `ORDER BY`. The same index also
prunes the ordinary single-arm case: any plain scan carrying an
`AggregationTemporality` predicate (the fan-out emitter's own per-row branch,
or a future consumer) benefits identically.

`ADD INDEX IF NOT EXISTS` is metadata-only and idempotent, so the auto-create
hook (re)applies it safely on every boot, covering both freshly-created and
pre-existing tables. **New parts written after the ALTER carry the index
automatically; existing parts are not back-filled by `ADD INDEX` alone.**

#### One-time `MATERIALIZE INDEX` back-fill runbook

To back-fill existing parts immediately on a deployment that predates the
index, run the one-time materialize per table (a background mutation,
non-blocking for reads):

```sql
ALTER TABLE <db>.otel_metrics_sum       MATERIALIZE INDEX idx_agg_temporality;
ALTER TABLE <db>.otel_metrics_histogram MATERIALIZE INDEX idx_agg_temporality;
```

`MATERIALIZE` is intentionally **not** issued by the auto-create hook — it
rewrites index metadata for every existing part and belongs in a deliberate
maintenance window, not the boot path. Track progress in `system.mutations`
(the `is_done` / `parts_to_do` columns). A `MATERIALIZE INDEX` mutation only
reads and rewrites the indexed column's marks, not the whole part, so it is
far cheaper than a `MATERIALIZE PROJECTION` over the same table.

### Column statistics (cerberus issue #2766)

Auto-create can also install ClickHouse **column statistics** — an opt-in
feature gated behind `CERBERUS_CH_OPTIMIZATIONS=column_statistics` (server
`>= 26.3`; see `docs/clickhouse-optimizations.md`'s `column_statistics`
entry). Enabled, it appends curated statements after the CREATE / projection
/ skip-index statements for each signal:

```sql
-- metrics: every table gets ServiceName + MetricName; sum/histogram add AggregationTemporality
ALTER TABLE <db>.otel_metrics_gauge      ADD STATISTICS IF NOT EXISTS ServiceName, MetricName TYPE uniq
ALTER TABLE <db>.otel_metrics_sum        ADD STATISTICS IF NOT EXISTS ServiceName, MetricName TYPE uniq
ALTER TABLE <db>.otel_metrics_histogram  ADD STATISTICS IF NOT EXISTS ServiceName, MetricName TYPE uniq
ALTER TABLE <db>.otel_metrics_exponential_histogram ADD STATISTICS IF NOT EXISTS ServiceName, MetricName TYPE uniq
ALTER TABLE <db>.otel_metrics_summary    ADD STATISTICS IF NOT EXISTS ServiceName, MetricName TYPE uniq
ALTER TABLE <db>.otel_metrics_sum        ADD STATISTICS IF NOT EXISTS AggregationTemporality TYPE minmax, uniq
ALTER TABLE <db>.otel_metrics_histogram  ADD STATISTICS IF NOT EXISTS AggregationTemporality TYPE minmax, uniq

-- logs
ALTER TABLE <db>.otel_logs ADD STATISTICS IF NOT EXISTS ServiceName, TraceId TYPE uniq
ALTER TABLE <db>.otel_logs ADD STATISTICS IF NOT EXISTS SeverityNumber TYPE minmax, uniq

-- traces
ALTER TABLE <db>.otel_traces ADD STATISTICS IF NOT EXISTS ServiceName, SpanName, TraceId TYPE uniq
ALTER TABLE <db>.otel_traces ADD STATISTICS IF NOT EXISTS Duration TYPE minmax, uniq, tdigest
```

**Why these columns, and why two ALTERs per table.** ServiceName / MetricName
/ SpanName / TraceId are all `String` or `LowCardinality(String)` in the
upstream OTel-CH schema, and ClickHouse rejects `minmax` and `tdigest`
outright on a string-typed column (`Code: 708, ILLEGAL_STATISTICS` —
verified against a live ClickHouse 26.5 server, not merely read off the
docs), so they carry `uniq` only. That is also the semantically right choice
for an equality-filtered identity column: `minmax` exists for RANGE
predicates a string equality never issues. AggregationTemporality (`Int32`)
and SeverityNumber (`UInt8`) are numeric, so they carry `minmax, uniq` in
their OWN ALTER — ClickHouse applies one TYPE list to every column in a
single ADD STATISTICS statement, so a string column and a numeric column
sharing supported types can never share one statement. Duration (`UInt64`)
additionally carries `tdigest`, since it is filtered by RANGE (a latency
threshold) far more than by equality, and only `tdigest` lets the planner
estimate a range predicate's selectivity rather than just its `[min, max]`
bounds. AggregationTemporality is scoped to the SAME sum/histogram pair the
`idx_agg_temporality` skip index above already targets — gauge never carries
the column, and exp_histogram carries it but sits outside every
temporality-aware routing path (see `renderAddTemporalityIndex`'s doc
comment), so statistics there would only tax writes for a column no read
path filters.

**ClickHouse Cloud refuses ADD STATISTICS outright** — statistics are not
supported there at all. The auto-create apply path treats that specific
refusal as skip-and-warn rather than fatal (`internal/schema/ddl`'s
`isColumnStatisticsUnsupported`), so a Cloud deployment with the feature
enabled logs a warning per signal and otherwise boots normally instead of
leaving `/readyz` stuck reporting "pending" (setupSchema's background retry
loop would otherwise retry the whole apply forever against a refusal that
can never succeed).

**Insert overhead.** ClickHouse's statistics GA (PR
[#97487](https://github.com/ClickHouse/ClickHouse/pull/97487), which landed
the 26.3 floor above) disables `materialize_statistics_on_insert` by
default: new parts do NOT compute statistics synchronously on every insert,
so the overhead lands on the existing background merge pool rather than the
synchronous write path.

`ADD STATISTICS IF NOT EXISTS` is metadata-only and idempotent, so the
auto-create hook (re)applies it safely on every boot, covering both
freshly-created and pre-existing tables. **New parts written after the
ALTER carry statistics automatically; existing parts are not back-filled by
`ADD STATISTICS` alone.**

#### One-time `MATERIALIZE STATISTICS` back-fill runbook

To back-fill existing parts immediately on a deployment that predates the
statistics, run the one-time materialize per table (a background mutation,
non-blocking for reads):

```sql
ALTER TABLE <db>.otel_metrics_gauge      MATERIALIZE STATISTICS ServiceName, MetricName;
ALTER TABLE <db>.otel_metrics_sum        MATERIALIZE STATISTICS ServiceName, MetricName, AggregationTemporality;
ALTER TABLE <db>.otel_metrics_histogram  MATERIALIZE STATISTICS ServiceName, MetricName, AggregationTemporality;
ALTER TABLE <db>.otel_metrics_exponential_histogram MATERIALIZE STATISTICS ServiceName, MetricName;
ALTER TABLE <db>.otel_metrics_summary    MATERIALIZE STATISTICS ServiceName, MetricName;
ALTER TABLE <db>.otel_logs               MATERIALIZE STATISTICS ServiceName, TraceId, SeverityNumber;
ALTER TABLE <db>.otel_traces             MATERIALIZE STATISTICS ServiceName, SpanName, TraceId, Duration;
```

(`MATERIALIZE STATISTICS` accepts every listed column regardless of which
`ADD STATISTICS` ALTER declared it, so one line per table is enough even
though the identity and numeric columns above were added in separate
ALTERs.)

`MATERIALIZE` is intentionally **not** issued by the auto-create hook — it
rewrites statistics for every existing part and belongs in a deliberate
maintenance window, not the boot path. Track progress in `system.mutations`
(the `is_done` / `parts_to_do` columns), same as the projection and index
runbooks above.

### TraceId lookup projection (cerberus issue #2767)

Trace-by-id lookups and logs<->traces correlation hops filter on `TraceId`,
but neither `otel_traces` (`ORDER BY (ServiceName, SpanName, Timestamp)`) nor
`otel_logs` (`ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName,
Timestamp)`) sorts on it — today these lookups are served only by the
`idx_trace_id` bloom_filter skip index, a probabilistic, GRANULARITY-coarse
filter, not exact row addressing.

Auto-create can install a lightweight **TraceId lookup projection** — an
opt-in feature gated behind `CERBERUS_CH_OPTIMIZATIONS=trace_id_projection`
(server `>= 25.5`, the first release accepting `_part_offset` inside a
normal projection's `SELECT` list; see `docs/clickhouse-optimizations.md`'s
`trace_id_projection` entry). Enabled, it appends one curated statement per
table after each signal's other DDL:

```sql
ALTER TABLE <db>.otel_traces ADD PROJECTION IF NOT EXISTS proj_trace_id (SELECT TraceId, _part_offset ORDER BY TraceId)
ALTER TABLE <db>.otel_logs   ADD PROJECTION IF NOT EXISTS proj_trace_id (SELECT TraceId, _part_offset ORDER BY TraceId)
```

Unlike the metrics catalog projections above, this one carries no `GROUP
BY`: it re-sorts two columns — the sort key plus ClickHouse's per-part
virtual row-offset column, `_part_offset` — rather than pre-aggregating
them, so the merge engine stores only that pair per row, not a second full
copy of the table. That is what makes it usable as a lightweight secondary
index: ClickHouse can binary-search the projection's own TraceId-ordered
part for a seek, then dereference `_part_offset` back into the base part for
the matching rows, on ClickHouse `>= 25.5`. On `>= 25.11` the optimizer can
additionally route an eligible `TraceId` predicate onto a row-precise
PREWHERE **bitmap filter** via this same projection — see the
`trace_id_bitmap_filter` feature below.

**Both tables carry it, not traces alone.** The issue's own motivation names
logs<->traces correlation as well as trace-by-id, and
`trace_id_index_probe_chdb_test.go`'s own bar already requires both sides
index-served for `Consistent() == true` — `otel_logs` has no more `TraceId`
locality than `otel_traces` does, so scoping the projection to traces alone
would leave the logs side of every correlation hop on the bloom filter.

`ADD PROJECTION IF NOT EXISTS` is metadata-only and idempotent, so the
auto-create hook (re)applies it safely on every boot. **New parts written
after the ALTER carry the projection automatically; existing parts are not
back-filled by `ADD PROJECTION` alone.**

#### One-time `MATERIALIZE PROJECTION` back-fill runbook

To back-fill existing parts immediately on a deployment that predates the
projection, run the one-time materialize per table (a background mutation,
non-blocking for reads — see the metrics catalog projections' own
write-amplification discussion above for what this costs on a large table):

```sql
ALTER TABLE <db>.otel_traces MATERIALIZE PROJECTION proj_trace_id;
ALTER TABLE <db>.otel_logs   MATERIALIZE PROJECTION proj_trace_id;
```

`MATERIALIZE` is intentionally **not** issued by the auto-create hook, same
as every other projection/index/statistics runbook above. Track progress in
`system.mutations`.

#### The `trace_id_bitmap_filter` query-time setting (server >= 25.11)

Independently of the projection's own version floor, `CERBERUS_CH_OPTIMIZATIONS`
also carries a second, auto-enabled feature: `trace_id_bitmap_filter`. On a
server `>= 25.11` (upstream PR
[#81021](https://github.com/ClickHouse/ClickHouse/pull/81021)), cerberus
stamps `min_table_rows_to_use_projection_index=0` on any query plan carrying
a `TraceId`-keyed predicate or join — a top-level equality, a flat or
subquery membership test, or a TraceQL structural join's recursive closure
(see `internal/engine.eligibleForTraceIDBitmapFilter`). ClickHouse's own
default for that setting (1,000,000 rows) is cleared trivially by any real
production `otel_traces`/`otel_logs` table, but stamping it to 0 makes the
bitmap-filter path reachable regardless of table size rather than leaving it
contingent on production scale — and, unlike the projection itself, this
setting is a pure query-time knob with no DDL and no version floor below
25.11 to opt into: it stamps automatically once the server clears that
floor, the same way `aggregation_in_order` / `condition_cache` do for their
own floors. It is harmless (a no-op) on a table that carries no
`proj_trace_id` projection at all.

### Loki label-cardinality catalog (cerberus issue #2770)

`GET /loki/api/v1/detected_labels` normally answers every request with a
server-side `GROUP BY` over the matched window's distinct
`ResourceAttributes` label sets, deriving per-key cardinality client-side —
real work re-paid on every Grafana logs-explorer open, even the plain
datasource-probe open that carries no stream selector at all.

Auto-create can install a **refreshable materialized view** that maintains a
small label-cardinality catalog, an opt-in feature gated behind
`CERBERUS_CH_OPTIMIZATIONS=loki_catalog_mv` (server `>= 24.10`, upstream PR
[#70550](https://github.com/ClickHouse/ClickHouse/pull/70550), which dropped
the `allow_experimental_refreshable_materialized_view` flag requirement and
made refreshable views GA; see `docs/clickhouse-optimizations.md`'s
`loki_catalog_mv` entry). Enabled, it appends two curated statements after
the logs table's other DDL:

```sql
CREATE TABLE <db>.loki_label_catalog (LabelKey String, CardinalityState AggregateFunction(uniq, String))
ENGINE = AggregatingMergeTree ORDER BY (LabelKey)

CREATE MATERIALIZED VIEW <db>.loki_label_catalog_mv REFRESH EVERY 5 MINUTE
TO <db>.loki_label_catalog AS
SELECT LabelKey, uniqState(LabelValue) AS CardinalityState
FROM <db>.otel_logs
ARRAY JOIN mapKeys(ResourceAttributes) AS LabelKey, mapValues(ResourceAttributes) AS LabelValue
WHERE Timestamp >= now() - toIntervalHour(24) AND LabelValue != ''
GROUP BY LabelKey
```

Every five minutes the view re-aggregates the **trailing 24 hours** of
`otel_logs` (a bounded window, so the refresh's own scan cost stays
predictable regardless of table retention) and, on success, **atomically
swaps** the result into `loki_label_catalog` — ClickHouse's own refreshable
materialized-view mechanism, not a cerberus-built one. A refresh that
errors (a transient ClickHouse restart, a schema hiccup) leaves the target
holding whatever the LAST successful swap produced; it never serves a
partial or empty result. This was verified against a real ClickHouse
server, not assumed from the upstream design doc — see
`internal/schema/ddl.TestLokiLabelCatalog_RefreshAndFailureMode`, which
renames the source table away mid-run to force a real failure and confirms
the catalog table's data is byte-for-byte unchanged afterward, then restores
the source and confirms the next refresh picks back up normally.

**Eligibility is deliberately narrow.** `/detected_labels` (and its sibling
`/detected_fields`, unchanged by this feature — see below) accept a LogQL
stream selector, and Grafana Logs Drilldown's per-service views pass one;
the catalog above is unkeyed by stream, so it can only answer a
SELECTOR-LESS request (an empty `query` param, or one that parses to zero
matchers, e.g. `{}`) — exactly the datasource-open-probe shape. Any request
carrying a real selector stays on the existing per-request `GROUP BY` path
unconditionally — that path is untouched and permanent, not a transitional
shim; nothing about it changes whether this feature is on or off. A
catalog-eligible request that finds the table not yet provisioned, or not
yet successfully refreshed since creation (an empty snapshot), degrades to
the same fallback path rather than erroring.

**`/detected_fields` is intentionally out of scope for this feature.**
Unlike `/detected_labels`' label-set shape, `/detected_fields` derives
fields by re-running the query path's own `| logfmt` / `| json` parser-stage
extractions over a row peek — replicating that inside a materialized view
would mean embedding the parser cascade in SQL and maintaining a second
declaration of it, a substantially larger and riskier change than the
label-key catalog above. See cerberus issue
[#2844](https://github.com/tsouza/cerberus/issues/2844) for tracking a
dedicated design pass on that, independent of this feature.

**Cardinality numbers diverge from the peek-based path by design.** Every
OTHER estimate `/detected_labels` and `/detected_fields` emit is
deliberately matched to upstream Loki's own peek-based HyperLogLog sketches
(same library, same sampling shape) so the compat harness diffs clean
against a reference Loki. The catalog computes a full-window server-side
`uniq` aggregate over 24h of real data instead — a different computation
over a different (larger, unsampled) window — so its numbers do NOT
reproduce the peek path's bit-for-bit, and are not expected to. The compat
harness only ever exercises selector-bearing requests (matching real Loki
usage), which always stay on the peek path regardless of this feature's
state, so the divergence never surfaces there.

**Measured before/after cost** (2M synthetic `otel_logs` rows spread across
a 24h window, `service.name`/`k8s.pod.name`/`deployment.environment.name`/
`k8s.namespace.name`/region attributes, ClickHouse 25.9 in Docker): the
existing per-request path over the full 24h window reads all 2,000,000 rows
(171 MiB) in 325ms; the SAME window's catalog read reads 5 rows (555 B, one
row per label key) in 2–3ms — roughly 400,000x fewer rows and ~130x less
wall-clock time. Even against a cheaper 1-hour window (1.6M rows, 50ms), the
catalog read is still ~17x faster.

`system.view_refreshes`' live status for this view is surfaced on `GET
/info` under `lokiCatalogViewRefresh` — `status`, `exception`,
`lastSuccessTime`, `lastRefreshTime`, `retry`, reported verbatim with no
cerberus-side healthy/unhealthy verdict layered on. Note that
`system.view_refreshes` carries no "last refresh result" enum and no
refresh-count column (verified live against a 25.9 server) — a failed
refresh reads as a non-empty `exception` plus `lastRefreshTime` having
advanced past `lastSuccessTime`, not a distinct status value.

### Tempo tag catalog (cerberus issue #2771)

`GET /api/v2/search/tags` and `GET /api/search/tag/{name}/values` normally
answer every request with a live scan of the traces table's
`SpanAttributes`/`ResourceAttributes` attribute maps — a full
`arrayJoin(mapKeys(...))` explosion per Grafana Explore keystroke, since
Tempo's tag/tag-value autocomplete fires as the user types a TraceQL query.

Auto-create can install a **refreshable materialized view**, the Tempo
sibling of the Loki label-cardinality catalog above, gated behind
`CERBERUS_CH_OPTIMIZATIONS=tempo_tag_catalog_mv` (server `>= 24.10`, the
same refreshable-materialized-view floor `loki_catalog_mv` uses; see
`docs/clickhouse-optimizations.md`'s `tempo_tag_catalog_mv` entry). Enabled,
it appends two curated statements after the traces table's other DDL:

```sql
CREATE TABLE <db>.tempo_tag_catalog (Scope String, TagKey String, TopValuesState AggregateFunction(topK(50), String))
ENGINE = AggregatingMergeTree ORDER BY (Scope, TagKey)

CREATE MATERIALIZED VIEW <db>.tempo_tag_catalog_mv REFRESH EVERY 5 MINUTE
TO <db>.tempo_tag_catalog AS
SELECT Scope, TagKey, topKState(50)(TagValue) AS TopValuesState
FROM (
    SELECT 'resource' AS Scope, k AS TagKey, v AS TagValue FROM <db>.otel_traces
    ARRAY JOIN mapKeys(ResourceAttributes) AS k, mapValues(ResourceAttributes) AS v
    WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
    UNION ALL
    SELECT 'span' AS Scope, k AS TagKey, v AS TagValue FROM <db>.otel_traces
    ARRAY JOIN mapKeys(SpanAttributes) AS k, mapValues(SpanAttributes) AS v
    WHERE Timestamp >= now() - toIntervalHour(1) AND v != ''
)
GROUP BY Scope, TagKey
```

Every five minutes the view re-aggregates the **trailing 1 hour** of
`otel_traces` — deliberately narrower than the Loki catalog's 24h window,
because it is sized to match `internal/api/tempo.DefaultSearchLookback`
exactly: the same window the live path already defaults a windowless
discovery request to. A catalog hit and a catalog-miss fallback therefore
describe the SAME trailing window rather than two different ones.

`/search/tags` and `/search/tag/{name}/values` serve from the catalog only
for a **windowless request** (no `start`/`end` at all — the true
datasource-open-probe shape, stricter than the Loki catalog's own
selector-less-only rule because this catalog answers with an actual key/value
LIST rather than a cardinality count, so a window mismatch would silently
omit real entries rather than merely reporting an approximate count), with
**no `q=<TraceQL>` narrowing filter**, and, for `/search/tags`, only for the
`resource`/`span`/unscoped `?scope=` values. Every other shape — a real
window, a `q=` filter, or `event`/`link`/`instrumentation`/`intrinsic`/`trace`
scope — stays on the existing live scan unconditionally; that path is
untouched and permanent, not a transitional shim.

Each key's top values carry a bounded top-50 sample (`topKState`/
`topKMerge`, a Space-Saving sketch) rather than the exhaustive value set the
live scan returns — the catalog trades exhaustiveness for a bounded per-key
state, the same kind of documented divergence the Loki catalog's
cardinality numbers accept.

**Verified, not assumed:**
`internal/api/tempo/search_tags_filter.go`'s `tagQueryFilter` only resolves a
non-nil filter when `q` is present AND lowers to a real span-row predicate,
so a filtered tag-values lookup provably never reaches the catalog — see
`TestSearchTagValues_WithQFilter_StaysOnLivePath`.

**Event/link scopes are intentionally out of scope for this feature.**
`Events.Attributes`/`Links.Attributes` are `Array(Map(String, String))` — one
map PER EVENT/PER LINK on a span row, not one map per row — so cataloging
them costs an extra `arrayFlatten(arrayMap(...))` fan-out on top of the
explosion the resource/span arms already pay twice, a materially costlier
shape the "if cheap" qualifier in the originating issue was not written to
cover. See cerberus issue
[#2850](https://github.com/tsouza/cerberus/issues/2850) for tracking a
dedicated design pass on that, independent of this feature.

**Measured before/after cost** (2,000,000 synthetic `otel_traces` rows
spread across a trailing 1h window, 5 resource-attribute keys + 10
span-attribute keys including two deliberately higher-cardinality tails
[`http.route`, `db.statement`], ClickHouse 25.9 in Docker via
testcontainers): the existing live scan
(`SELECT DISTINCT arrayJoin(mapKeys(ResourceAttributes))` over the same
window) reads 1,991,808 rows (294,022,617 bytes) in 302ms; the catalog read
(`SELECT TagKey FROM tempo_tag_catalog WHERE Scope = 'resource' GROUP BY
TagKey`) reads 15 rows (515 bytes) in 4.6ms — roughly 132,800x fewer rows
and ~65.6x less wall-clock time, verified via `system.query_log`
(`read_rows`/`read_bytes`), not client-side row counting. See
`internal/schema/ddl.TestTempoTagCatalog_MeasuredCost`.

`system.view_refreshes`' live status for this view is surfaced on `GET
/info` under `tempoTagCatalogViewRefresh`, the exact same shape and posture
`lokiCatalogViewRefresh` reports for its sibling.

### Curated column compression codecs (cerberus issue #2768)

Auto-create also installs two curated `MODIFY COLUMN` codec ALTERs, retuning
the upstream OTel-CH exporter template's own compression codec choice on two
columns benchmarked against real production-shaped data:

```sql
ALTER TABLE <db>.otel_logs   MODIFY COLUMN IF EXISTS Body     CODEC(ZSTD(3))
ALTER TABLE <db>.otel_traces MODIFY COLUMN IF EXISTS Duration CODEC(GCD, ZSTD(1))
```

Unlike `ADD PROJECTION` / `ADD INDEX` / `ADD STATISTICS` above, `MODIFY
COLUMN` has no `IF NOT EXISTS`-shaped guard — only `IF EXISTS`, which is a
no-op on an absent column (e.g. a signal not yet auto-created), not on an
unchanged one. Re-running the identical statement is still safe to do on
every boot: ClickHouse compares the declared codec against the column's
current one and only schedules work when they actually differ, so applying
this repeatedly never re-triggers a conversion once the codec has converged
— verified directly against a live server (both a 24.8 floor container and
the auto-create integration test's 25.9 container): re-issuing the same
`MODIFY COLUMN ... CODEC(...)` statement twice in a row produces no error
and no additional mutation.

**Only two of the issue's proposed candidates survived measurement.** Every
candidate was benchmarked — not just reasoned about — against real
production-shaped sample data (`test/perf/nightly/testdata/samples/`,
issue #2411) or, where no real sample exists (span Duration — traces are
outside that sample set's scope), representative synthetic data, via a real
MergeTree engine (chDB), comparing whole-table compressed bytes before/after
the codec swap:

- **Adopted — Logs Body:** `ZSTD(3)` measured ~1.3%-1.8% smaller than the
  upstream `ZSTD(1)` default on a representative leveled-log-line corpus,
  consistently across repeated runs. ZSTD's decode cost is level-independent,
  so this costs write-side CPU only — no read-side query latency impact.
- **Adopted — Span Duration:** `GCD, ZSTD(1)` (no Delta stage) measured a
  real ~3.5% win when real precision is coarser than the column's declared
  nanosecond resolution (the issue's own stated GCD rationale), and was a
  statistical no-op (+0.02%, measurement noise) when precision is genuinely
  fine-grained — GCD finds no common divisor when there isn't one, so it
  costs nothing on a deployment whose real Duration values don't carry the
  coarse-precision pattern this codec targets. Chaining a `Delta` stage
  ahead of ZSTD — either `Delta, ZSTD(1)` alone or the issue's own proposed
  `GCD, Delta, ZSTD(1)` pairing — measured 3%-10% LARGER instead: Duration
  values are independent per-span measurements, not a running sequence, so
  delta-encoding them adds entropy rather than removing it.
- **NOT adopted — metrics TimeUnix / span Timestamp (`DateTime64(9)`):** the
  issue proposed `DoubleDelta, ZSTD(1)`, reasoning that a near-constant
  scrape interval leaves second-order regularity DoubleDelta captures and
  Delta alone misses. Measured against three real metrics tables
  (gauge/sum/histogram) it was 43%-166% LARGER than the current
  `Delta, ZSTD(1)` — a regression: a near-constant Delta stream is already
  maximally redundant (the same interval repeated for most of a series'
  run), which `ZSTD(1)` alone already exploits about as well as physically
  possible (232x-443x compression measured); DoubleDelta's own per-value
  framing bytes break up that redundancy more than they remove. `GCD, Delta,
  ZSTD(1)` measured a small, INCONSISTENT effect across the three tables
  (-3.5% to -6.9% on sum/histogram, +9.2% on gauge) — not a safe blanket win
  across every metrics table sharing one codec declaration. Bare `GCD,
  ZSTD(1)` measured catastrophically worse (+650% to +3515%). TimeUnix and
  Timestamp keep their current `Delta, ZSTD(1)`, unchanged by this issue.
- **NOT adopted — Value (gauge/sum) / Sum (histogram/summary/exp_histogram)
  Float64:** the issue gated Gorilla/FPC adoption on beating `ZSTD(1)` on
  real gauge-shaped data. Measured against the same three tables, BOTH
  regressed — Gorilla 27%-2182% larger, FPC 84%-2662% larger — so neither is
  adopted; Value / Sum keep `ZSTD(1)`, unchanged.

See the codec-tuning PR (cerberus issue #2768) for the full benchmark
transcript and measured numbers. Cerberus issue #2822 tracks the one
candidate the issue explicitly deferred rather than benchmarked here:
experimental ALP for Value/Sum, earmarked for a future `>= 26.8` version
floor (its 26.8 Float32 arithmetic change is a live on-disk decode-compat
risk below that floor).

All codecs used above are supported at ClickHouse >= 22.9, well under this
repo's 24.8 version floor (`docs/toolchain.md`), so — like `ADD PROJECTION`
/ `ADD INDEX` above, and unlike `ADD STATISTICS`'s chopt-gated
`column_statistics` feature — this registry renders **unconditionally**: no
config flag, no chopt feature gate.

A codec change is metadata-only for **new parts only**; existing parts keep
their prior codec until a background merge (or an operator-run `OPTIMIZE
... FINAL`) rewrites them.

#### One-time `OPTIMIZE ... FINAL` back-fill

Unlike the projection / index / statistics ALTERs above, ClickHouse has no
dedicated `MATERIALIZE`-style mutation for a codec change — the general
mechanism is a full part rewrite:

```sql
OPTIMIZE TABLE <db>.otel_logs   FINAL;
OPTIMIZE TABLE <db>.otel_traces FINAL;
```

**Cost / caveat.** Unlike `MATERIALIZE PROJECTION` / `MATERIALIZE INDEX` /
`MATERIALIZE STATISTICS`, which each touch only their own narrow slice of a
part, `OPTIMIZE ... FINAL` reads and rewrites the **entire table**, not just
the recoded column — budget accordingly on a large table, and prefer letting
normal background merges converge new codecs onto old parts over time rather
than forcing an immediate rewrite unless the storage win is needed sooner.

### Text index on logs Body + LogQL line-filter prefilter (cerberus issue #2773)

The logs table's `idx_lower_body` skip index over `lower(Body)` shipped as a
`tokenbf_v1(32768, 3, 0)` bloom filter — but cerberus's LogQL line filters
(`|=` / `!=` / `|~` / `!~`) emit `position(Body, ?) > 0` / `match(Body, ?)`
against the case-sensitive `Body` column directly, a predicate shape
`tokenbf_v1` never matches. The most expensive LogQL predicate class (a
substring or regex filter over the log line itself) has always full-scanned
Body, even on a deployment that already pays to maintain `idx_lower_body`.

Two independent, opt-in `CERBERUS_CH_OPTIMIZATIONS` features fix this:

#### The `full_text_index` DDL feature (server >= 26.2)

`enable_full_text_index` — the setting gating ClickHouse's own acceptance of
`TYPE text(...)` — flips from default-OFF to default-ON at ClickHouse 26.2
(confirmed live: `SELECT value FROM system.settings WHERE name =
'enable_full_text_index'` reports `0` on a 26.1.12 server and `1` on 26.2.19),
the GA floor this feature gates on. Enabled, a **freshly created** logs table
gets `idx_lower_body` as `TYPE text(tokenizer = 'splitByNonAlpha')` instead of
`tokenbf_v1` — the upstream OTel-CH exporter template's own
`HasFullTextSearch` branch already carries this shape; the feature only
decides which branch a boot renders. An **existing** table (already carrying
the tokenbf branch from an earlier boot) instead gets an ADDITIVE,
separately-named index:

```sql
ALTER TABLE <db>.otel_logs ADD INDEX IF NOT EXISTS idx_body_text lower(Body) TYPE text(tokenizer = 'splitByNonAlpha') GRANULARITY 100000000
```

A second name, not an in-place type swap of `idx_lower_body`: ClickHouse
matches `ADD INDEX IF NOT EXISTS` on NAME, not type, so re-running it against
a table that already carries `idx_lower_body` as `tokenbf_v1` is a silent
no-op — it could never install the text index on an upgraded deployment.
Swapping the type in place needs `DROP INDEX` + `ADD INDEX`, which is
destructive (existing `MATERIALIZE`'d granules are discarded, forcing a full
re-backfill) and, since this render-time DDL layer has no live
`system.data_skipping_indexes` read, cannot tell whether `idx_lower_body` is
ALREADY the text type — repeating that drop+add on every boot would be
pure, repeated, backfill-losing churn. Installing a second, non-colliding
name is the only additive, idempotent, crash-safe option available here, the
same reasoning `ADD PROJECTION` / `ADD STATISTICS` / `ADD INDEX` above all
already follow. `GRANULARITY 100000000` reproduces ClickHouse's OWN implicit
default for a `text` index type when no `GRANULARITY` clause is given
(confirmed live via `EXPLAIN indexes=1`, and re-confirmed byte-identical when
stamped explicitly) — `chsql.AlterTableAddIndex` has no omit-GRANULARITY
mode, so this reproduces the default rather than widening that builder's
contract for one index type.

**Retiring the now-redundant legacy `idx_lower_body` tokenbf index on an
upgraded existing table is explicitly out of scope of this feature** —
dropping an index a running deployment's queries may still be planning
against is a real production-cluster risk this render-time DDL apply should
not take unilaterally. Tracked as a follow-up issue.

##### One-time `MATERIALIZE INDEX` back-fill runbook

`ADD INDEX` is metadata-only for NEW parts; existing parts need the one-time
materialize to benefit retroactively:

```sql
ALTER TABLE <db>.otel_logs MATERIALIZE INDEX idx_body_text;
```

(On a freshly created table the index is `idx_lower_body` itself, already
the text type — no separate materialize needed for those parts.) Track
progress in `system.mutations`, same as every other index/projection/
statistics runbook above.

#### The `text_index_line_filter` query-time feature (server >= 26.4)

Independently of the DDL feature's own version floor,
`CERBERUS_CH_OPTIMIZATIONS` also carries `text_index_line_filter`, gating a
chsql emission rewrite, not any DDL statement. On a server `>= 26.4` — the
release introducing `use_text_index_like_evaluation_by_dictionary_scan`
(confirmed live: 0 rows from `SELECT name FROM system.settings WHERE name
ILIKE '%text_index_like%'` on a 26.2.19 server, 3 rows on 26.4.5) — a text
index can answer a `LIKE '%needle%'` predicate by dictionary scan instead of
a row-by-row match. cerberus's LogQL line-filter emitter uses this by
prepending an ANDed per-token strict-superset prefilter ahead of the
UNCHANGED row predicate:

```sql
-- |= "connection reset by peer" becomes:
(lower(Body) LIKE '%connection%' AND lower(Body) LIKE '%reset%' AND lower(Body) LIKE '%peer%'
 AND (position(Body, 'connection reset by peer') > 0))
```

Each conjunct is a necessary (never sufficient) condition for the original
literal to be a substring of `Body` — a strict superset the granule-pruning
index can use to eliminate ranges the row predicate would have rejected
anyway, never one that admits a false positive past the always-kept row
predicate. `by` is dropped (below the 4-rune minimum useful token length —
see `internal/chsql`'s `textIndexLikeMinTokenLength`), and every token is
lowered ASCII-only (matching ClickHouse's own `lower()`, not the Unicode-aware
`lowerUTF8()` idx_lower_body does NOT use) and LIKE-escaped (`\`, `%`, `_`)
before embedding.

**Scope boundary — negated and regex filters:**

- `!=` / `!~` (negated) are passed through byte-identical: a superset
  prefilter has no sound dual for a "must NOT contain" predicate.
- `|~` (regex, non-negated) is rewritten ONLY when the pattern round-trips
  through Go's `regexp/syntax` (RE2 — the same engine ClickHouse's `match()`
  runs) as a single `OpLiteral` — a regex only in name, with no
  metacharacters. Any other regex shape (alternation, anchors, character
  classes, quantifiers) renders byte-identical to today; this package does
  not compile partial RE2 semantics into index predicates.

**Independent of `full_text_index`, but inert without it**: the floors are
strictly ordered (26.4 > 26.2), so a server can satisfy one without the
other, and the rewrite is a harmless (if pointless) no-op on any table that
carries no text index at all — every LIKE conjunct just evaluates against
the same undexed `lower(Body)` scan the row predicate already pays for.

**Unverified ClickHouse 26.6 claims — do not build on these without
re-verifying against a real 26.6+ instance.** `multiSearchAny` inside the
skip-index analyzer and a dedicated posting-list segment cache were both
raised as possible 26.6 extras. Live-probed against a real ClickHouse
26.6.3.62 server: `multiSearchAny(Body, [...])` produced NO skip-index entry
in `EXPLAIN indexes=1` at all (full granule scan, unlike `hasAnyTokens` /
`hasAllTokens` / `hasToken`, which all pruned correctly) — this claim did
NOT hold on the probed build. `use_text_index_postings_cache` DOES exist as
a real setting on 26.6.3.62, defaulting to `0` (off) — its existence is
confirmed, but no benchmark evidence was gathered on its actual effect.
Neither claim is relied on by `text_index_line_filter`. See the follow-up
issue this PR files for someone with continued 26.6+ access to settle these.

### DELTA-prefix aggregate table + backfill (cerberus issue #2389)

Auto-create can also provision an **opt-in, cerberus-owned** table + materialized
view backing exact, retention-independent DELTA-temporality prefix
reconstruction — the mechanism `rate()`/`increase()`'s counter-reset /
extrapolation-boundary correction needs for a DELTA-temporality OTel Sum
counter (see `Config.DeltaPrefixLookback`'s doc comment for the mechanism this
supplements). Unlike the metadata projections and the skip index above, no
upstream template backs this table — it never appears unless the operator
opts in, and it is entirely additive.

**Query answering now CAN read from this table**, gated behind a separate,
later opt-in from provisioning: `Config.DeltaPrefixReadEnabled`
(`CERBERUS_DELTA_PREFIX_READ_ENABLED`, default `false`). Provisioning the
table (`CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED`) only says the table + MV
exist; it says nothing about whether their one-time backfill (below) has run
and been verified. Reusing one flag for both would flip the read path live
mid-backfill and silently under-count every long-lived DELTA counter's
boundary correction — the exact bug this whole mechanism exists to fix,
reintroduced by the fix itself — so the two are independent knobs by design.
Set `CERBERUS_DELTA_PREFIX_READ_ENABLED=true` only after `delta-prefix-verify`
(below) passes clean. Until then — or for a deployment that never intends to
backfill — `Config.DeltaPrefixLookback`'s bounded approximation remains
cerberus's only DELTA-prefix mechanism, unchanged.

**Provisioning** needs *two* independent flags, both default `false`:
`CERBERUS_AUTO_CREATE_SCHEMA=true` (as for every other auto-created table) AND
`CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true`. This is new, unproven machinery,
so a deployment that already has schema auto-create on for the five upstream
tables does **not** get this table for free — the operator opts in a second
time, explicitly.

```sql
CREATE TABLE IF NOT EXISTS <db>.otel_metrics_sum_delta_prefix
(
    MetricName          LowCardinality(String),
    Attributes           Map(LowCardinality(String), String),
    ResourceAttributes    Map(LowCardinality(String), String),
    ServiceName          LowCardinality(String),
    BucketStart          DateTime64(9),
    PartialSum           SimpleAggregateFunction(sum, Float64)
)
ENGINE = AggregatingMergeTree
ORDER BY (MetricName, BucketStart, Attributes, ResourceAttributes, ServiceName);

CREATE MATERIALIZED VIEW IF NOT EXISTS <db>.otel_metrics_sum_delta_prefix_mv
TO <db>.otel_metrics_sum_delta_prefix AS
SELECT MetricName, Attributes, ResourceAttributes, ServiceName,
       toStartOfDay(TimeUnix) AS BucketStart, sum(Value) AS PartialSum
FROM <db>.otel_metrics_sum
WHERE AggregationTemporality = 1
GROUP BY MetricName, Attributes, ResourceAttributes, ServiceName, BucketStart;
```

`BucketStart` sits right after `MetricName` in `ORDER BY` (not last) so a
date-bounded read prunes on the primary key instead of scanning every bucket
of every series for a metric. `SimpleAggregateFunction(sum, Float64)` on
`AggregatingMergeTree` is what makes a plain `sum(PartialSum)` read correct
regardless of how much background merging has happened — no `FINAL`, no
staleness window (unlike `ReplacingMergeTree`, which would need one). The
table's TTL and storage tiering follow the same `CERBERUS_SCHEMA_TTL_METRICS`
/ `_TIER_AFTER_METRICS` knobs as the five upstream metrics tables, keyed on
`BucketStart` — kept in lockstep with the base table's own retention on
purpose: a bucket TTL'd out of the aggregate table is only ever one the base
table has already dropped too.

#### One-time DELTA-prefix backfill runbook

**A `CREATE MATERIALIZED VIEW` does not retroactively process rows already in
the base table** — the MV only captures INSERTs from the moment it is created
onward. So immediately after provisioning, the aggregate table is correct for
*new* data but **empty for history**, and reading it as-is would silently
**under-count** every long-lived DELTA series (the exact bug issue #2389
tracks fixing) — worse than not having the table at all. Two `cerberus
schema` subcommands cover the one-time catch-up, both opening a real
ClickHouse connection from the SAME `CERBERUS_*` / `cerberus.yaml`
configuration the server reads:

```sh
# 1. Record (or look up) the MV's own creation timestamp, e.g. from
#    system.tables.metadata_modification_time, or the wall-clock moment the
#    CREATE MATERIALIZED VIEW statement above ran. A real deployment is
#    almost always cut over mid-day, not at exactly midnight.

# 2. Backfill everything strictly before that exact timestamp.
cerberus schema delta-prefix-backfill --before 2026-08-20T14:32:10Z

# 3. Verify per-metric completeness before flipping the read-side flag.
cerberus schema delta-prefix-verify --before 2026-08-20T14:32:10Z
```

**The `--before` bound is security-review-grade — getting it wrong corrupts
data silently.** It MUST be the MV's own creation timestamp, exactly — not
rounded to a calendar day. Backfilling past it double-counts every row the
live MV already captured, inflating `PartialSum` for every bucket the two
overlap; backfilling with a bound *before* the true creation time
under-counts the gap between the two, just as silently. `delta-prefix-backfill`
and `delta-prefix-verify` both bound strictly by this exact instant, with no
day-rounding: an earlier revision rounded the bound down to its calendar day
(`toStartOfDay`) on the theory that this kept the aggregate table's own
day-granularity bucket from ever straddling "backfilled" and "MV-captured"
contributions — but `PartialSum` is a `SimpleAggregateFunction(sum, ...)`
column, so two partial contributions landing in the same bucket already merge
additively regardless of which writer produced them, and day-rounding instead
created a real gap: for a deployment cut over mid-day (the normal case), every
row between midnight and the MV's real creation instant was excluded from
BOTH the backfill (rounded-down bound skipped it) and the live MV (which
never fires for INSERTs older than its own creation) — a permanent, silent
under-count. Backfilling by the exact instant closes that gap: everything
strictly older than it is backfilled, and the live MV captures everything
from that instant onward, so the two windows meet exactly.

**Run this backfill AS SOON AS POSSIBLE after creating the table/MV — this
warning is as load-bearing as the `--before` bound above.** The DELTA-prefix
table shares its `TTL toDateTime(BucketStart) + <retention>` shape
(`CERBERUS_SCHEMA_TTL_METRICS`) with the base metrics tables — correct for
STEADY STATE, where both tables age out their oldest day together because
both are populated continuously, but not correct for this one-time backfill.
If an operator runs `delta-prefix-backfill` any time *after* the earliest
available historical day has already crossed its own `BucketStart +
<retention>` instant, the resulting `INSERT ... SELECT` writes rows for that
day that are **already past their own TTL the moment they land**. Because
this table is small and merges frequently, ClickHouse's routine background
TTL cleanup reaps those rows almost immediately — often within seconds to
minutes, well before an operator gets a chance to run `delta-prefix-verify`
and see a clean state. **This is unrecoverable**: no sequence of backfill
re-runs, with or without narrowing to just the affected day, with or without
dropping and recreating the table/MV, can bring that day back — the
constraint is the row's own age relative to "now", not anything about the
write path. Run the backfill (and `delta-prefix-verify` right after it)
*before* the earliest day in your history reaches that boundary; once it has
passed, that day's completeness is permanently gone, and both CLI verbs below
detect and report this case explicitly rather than leaving the operator to
diagnose an unexplained failure.

`delta-prefix-backfill` runs a single `INSERT INTO ... SELECT` — the same
`GROUP BY (MetricName, Attributes, ResourceAttributes, ServiceName,
toStartOfDay(TimeUnix))` shape the MV itself uses — filtered to
`AggregationTemporality = 1` (DELTA) and `TimeUnix` strictly before the
exact cutover instant. `--dry-run` prints the exact statement without
executing it, for review before a maintenance window. Cost is bounded to the
DELTA-temporality slice of the base table's history (empirically well under 1%
of real `otel_metrics_sum` rows — see `Config.DeltaPrefixLookback`'s doc
comment) plus one aggregate write per distinct `(MetricName, raw attribute
tuple, day)` — materially cheaper than the projection back-fill above despite
reading a wider column set. Before issuing that INSERT, it also checks — against
the base table, using the resolved `CERBERUS_SCHEMA_TTL_METRICS` retention —
whether any day within `--before`'s scope is already outside the target
table's own TTL as of right now (the unrecoverable case described above); if
so it prints a loud `WARNING` naming the affected day(s) after a successful
run rather than succeeding with zero signal. This is a warning, not an
error — the command still exits `0`, since the operator may already have
accepted the loss for those days or be intentionally re-running to pick up
the still-recoverable ones.

`delta-prefix-verify` compares the aggregate table's per-metric-name totals
(`sum(PartialSum)`, grouped by `MetricName` only) against the base table's own
DELTA-temporality totals (`sum(Value)` under the same `AggregationTemporality
= 1` + exact-instant `--before` filter) and reports PASS/FAIL with a
`--tolerance`-bounded per-metric mismatch table (`--json` for the
machine-readable form). **Scope note:** this is a per-metric *completeness*
check — did every DELTA row make it into the aggregate table — not a
per-series *identity-alignment* check against cerberus's read-time computed
series key; see `internal/deltaprefix`'s package doc for the full boundary
and where that alignment property is actually proven (a structural
plan-equality test plus a chDB round-trip probe, both in the read-side
mechanism itself — see below). A clean `delta-prefix-verify` pass is the
required confirmation before an operator sets
`CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED=true` **and**
`CERBERUS_DELTA_PREFIX_READ_ENABLED=true`. A deployment that never
backfills is not broken — it simply keeps running today's bounded
`CERBERUS_DELTA_PREFIX_LOOKBACK` approximation indefinitely, a legitimate
permanent choice for an operator who doesn't need exact boundary-correction
precision.

**A day already outside the target table's own retention is never reported
as a bare, unexplained mismatch.** `delta-prefix-verify` resolves the same
`CERBERUS_SCHEMA_TTL_METRICS` retention `delta-prefix-backfill` checks and,
for each day in its comparison window, detects whether that day's
`BucketStart + <retention>` has already elapsed as of the check's own run
time (the unrecoverable case described above). Any such day is EXCLUDED from
`AggregateTotals` / `BaseTotals` / the mismatch table — it can never be
turned into a PASS by any backfill re-run, so folding it into the same FAIL
as a genuine completeness gap would be indistinguishable from a real bug —
and instead surfaced as a separate, clearly labeled `NOTE` naming the
excluded day(s), auto-detected with no flag required (a real completeness
gap must never be silently hidden, so this is safe-by-default: it always
runs, and it is always visually distinct from the PASS/FAIL verdict above
it, in both the text and `--json` report forms).

#### Read-side mechanism and its measured PK-pruning cost

Once backfilled and verified, `CERBERUS_DELTA_PREFIX_READ_ENABLED=true`
switches `rate()`/`increase()`'s DELTA-prefix reconstruction to a two-term
read, replacing `CERBERUS_DELTA_PREFIX_LOOKBACK`'s bounded scalar scan:

1. `sum(PartialSum)` from the aggregate table, bounded to `BucketStart <
   toStartOfDay(<window start>)` — every full day strictly before the
   window's own day. Cost scales with day-count × a per-metric raw-tuple
   multiplicity (attribute key-sanitisation collisions, `Map`
   key-insertion-order variance across collector redeploys, dedicated-column
   overlay — see the series-identity note above), never with retained
   *sample* count.
2. `sum(Value)` from the base table, bounded to `[toStartOfDay(<window
   start>), <window start>]` — the current, still-open bucket's own
   contribution, at most one bucket width.

Both terms carry the same CUMULATIVE-only presence guard the
`CERBERUS_DELTA_PREFIX_LOOKBACK` scalar scan already carries: an
uncorrelated scalar subquery testing whether any row in the eval window is
genuinely DELTA-temporality. ClickHouse evaluates it once and, for the
overwhelmingly common case of a CUMULATIVE-only series, folds the whole
predicate to a constant `false` and skips both table reads entirely —
`rate()`/`increase()` over a CUMULATIVE counter never pays the cost below.
The cost this section measures applies only to a query the guard actually
lets through: a genuinely DELTA-temporality series in the eval window.

Both terms resolve to the same series through cerberus's ordinary
column-name GroupBy resolution — see `chplan.RangeWindow.DeltaPrefixAggregateInput`'s
doc — not a new join-key derivation.

**PK-pruning cost, re-measured against real production-shaped data.** An
earlier estimate (design discussion for this feature, not previously
committed to this document) against a *synthetic* 200-series/90-day probe
(18,000 rows) found a `MetricName`-only read scanning 71% of the table,
dropping to 34% once `BucketStart` moved to position 2 in `ORDER BY` — the
ordering this table already uses above — and flagged re-measuring against a
real high-cardinality metric as still open before this document claimed a
specific number. That re-measurement:
`test/perf/smoke/testdata/samples/svc_http_requests_total.parquet` — a real,
scrubbed 14-day Sum-metric capture, 18,591,129 raw samples across 31,073
distinct series — loaded into a chDB session and aggregated into this
table's exact shape (`GROUP BY MetricName, Attributes, ResourceAttributes,
ServiceName, toStartOfDay(TimeUnix)`), yielding 53,252 aggregate rows (most
series are active on only 1–2 of the 14 days — this sample's two-window daily
capture pattern, not a data-loading artefact). Against that table, `EXPLAIN
ESTIMATE` for a single-metric, half-window read (`WHERE MetricName = ... AND
BucketStart < <day 8 of 14>`) reads **26,624 of the table's 53,252 rows
(50.0%)** — essentially every series' rows for the included days — to answer
a query whose TRUE per-series contribution is typically 1–2 rows. This
confirms the synthetic finding at real production scale and sharpens it: even
with a real metric's genuine (non-synthetic) cardinality and day-distribution,
**no PK-level pruning exists below `MetricName`** — the series-identity
predicate is a `GROUP BY` key computed from the scan output, never a
`WHERE`-testable column, so cost scales with `date-range × metric-cardinality`
regardless of dataset size. This is still enormously cheaper than the removed
unbounded-retention scan (bounded by day-count regardless of series
cardinality, vs. literally the whole base table's history), but is a real,
named cost for a single-series `rate()`/`increase()` query against a
high-cardinality DELTA metric — not `date-range × 1` — and belongs in
capacity planning for any deployment enabling
`CERBERUS_DELTA_PREFIX_READ_ENABLED` against such a metric.

### Startup requirements preflight

`CERBERUS_REQUIREMENTS_CHECK` (**on by default**) runs a boot-time
requirements check immediately **after** the schema-create step. It
converts the classes of misconfiguration that would otherwise surface as
opaque query-time errors — or, for storage layout, as no error at all — into a
precise boot-time finding:

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
- **Absent *configured* metric table.** The one exception to the tolerance
  above, and it turns on **who chose the name**. When a deployment sets
  `schema.metrics.gaugeTable` / `sumTable` / `histogramTable` (or the
  equivalent `CERBERUS_SCHEMA_METRICS_*_TABLE` variable) and a **reachable**
  ClickHouse does not have that table, startup **fails fast** with a message
  naming both the table and the config key that set it. Naming a table is an
  assertion that it exists, and a typo'd or unprovisioned one never
  self-heals — it would instead silently degrade every `/api/v1/series`,
  `/api/v1/labels`, and `/api/v1/label/<name>/values` request into a
  ClickHouse `UNKNOWN_TABLE` error. A metric table whose name cerberus
  **defaulted** carries no such assertion: a deployment that ingests only logs
  and traces never provisions `otel_metrics_gauge` / `_sum` / `_histogram` and
  is not misconfigured for it, so their absence is the ordinary
  not-yet-provisioned race above — cerberus boots, reports NOT READY, and
  re-probes. Spelling a stock name out explicitly opts that table into the
  fail-fast check.
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
- **Inert storage tiering.** When a storage policy or a tiering volume is
  configured, the policy's volumes are read from `system.storage_policies` and
  the combination is checked for being accepted-but-inert: a **multi-volume
  (hot/cold) policy with no tiering rule** to move parts into it, a
  `CERBERUS_SCHEMA_TIER_VOLUME` the policy does not have, or a policy the
  server does not define at all. Each is reported as a **warning** naming the
  fix, never a boot failure — storage layout is a cost property of the tables,
  not a correctness property of the gateway, so refusing to boot would turn a
  storage-bill problem into an outage. A deployment that configures neither
  knob is not probed at all.

**Scoped to the enabled heads.** Every requirement above is scoped by
`CERBERUS_ENABLED_HEADS`: a Head maps 1:1 onto a telemetry signal (`prom` →
metrics, `loki` → logs, `tempo` → traces), and a table's checks — the
wrong-shape gate, the absent-table wait-and-reprobe, and the explicit-name
fail-fast — run **only** for the signals this process actually serves. A
`CERBERUS_ENABLED_HEADS=loki` split-mode pod (see
[`health.md`](health.md#per-head-readiness)) never validates or waits on
`otel_metrics_*` / `otel_traces` existing, so `/readyz` goes ready as soon as
`otel_logs` is provisioned — a logs-only pipeline no longer keeps a
fully-functional pod out of its Service forever waiting on tables it will
never ingest. The unset default (`prom,loki,tempo`) preserves the
all-three-signals behaviour every deployment had before this scoping existed.

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
`CERBERUS_REQUIREMENTS_CHECK=false` to skip every gate (logged as one line) —
useful when pointing cerberus at a deliberately non-default ClickHouse layout
that the shape gate doesn't model. The preflight needs ClickHouse reachable to
read the version and column metadata, but a server that is unreachable at the
preflight point is itself classified transient (a dial / connection-refused
error boots unready and re-probes, exactly like the connectivity ping above) —
**not** a fatal exit. What stays fatal is a *reachable* server that fails the
contract: a too-old / unparseable version, a wrong-shape table, a missing
metric table the deployment named itself, or an introspection *error* (as
opposed to a clean zero-row absence of a table nobody named, or the
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
release ritual. Every cycle runs these five steps top to bottom. The ordering
carries as much weight as the steps themselves:

1. **Drive everything merged first.** A cycle opens by draining the board: no
   open PR and no dangling branch-without-a-PR is left behind. A release ships
   the whole delta since the previous one, never a subset.
2. **Settle the retirement set before anything is cut.** When the cycle
   cuts a new minor — which a breaking change is by itself enough to force, see
   below — the line `main` is leaving falls out of the window defined in
   [release support window / EOL policy](#release-support-window--eol-policy)
   in that same cycle: with a one-line window, the departing line never
   receives a backport of its own. This step is a decision, not an action: the
   retirement itself is automatic, with the `eol-retire` job deleting the
   out-of-window branch after the new minor publishes — a no-op if that branch
   was never created (see step 4). A patch-only cycle retires nothing and
   passes straight through.
3. **Audit the delta.** One last pass over the complete diff since the previous
   release: code against comments against docs, DRY, KISS, soundness. This is
   the final gate — findings are fixed and merged onto `main` here, before any
   line is backported and before any tag exists. Also glance at `perf-nightly`'s
   own trend across the cycle's runs, not just its latest pass/fail: the gate
   catches a single run regressing past its committed ceiling, but a slow,
   multi-cycle creep that stays under headroom on every individual night is
   exactly the shape it cannot see by design. And glance at
   `perf-nightly-selfcheck`'s weekly runs (#2437): a red run there means the
   nightly gate itself stopped catching an injected regression, which is a
   worse, quieter failure than the gate itself going red.
4. **Backport within the current line only.** With a one-line support window
   there is never more than one supported `release/<major>.<minor>.x` line at
   once, so "backport" only ever means: a hotfix lands on `main` while `main`
   is still on the current minor, and ships in that minor's next patch. A cycle
   that cuts a new minor does **not** create a maintenance branch for the line
   it is leaving — that line falls out of the window in the same cycle (step
   2), so a branch created for it would have no window to ever receive a
   backport. See [maintenance lines](#maintenance-lines-hotfix-backports) for
   the branch mechanics, which exist for the general case but sit unused under
   the current one-line policy.
5. **Publish the new MINOR (or patch) release last.** `main`'s release is
   always the final publish of the cycle — there is no backport-line publish
   to sequence ahead of it, since step 4 never produces one under the current
   one-line policy.

**Breaking changes are accepted in a new minor.** On the cerberus version line
(`appVersion` / the `v<major>.<minor>.<patch>` tags) a breaking change does
**not** require a major bump — the minor is its vehicle. That makes "does this
delta break anything?" a step-2 input: a breaking change is on its own
sufficient reason for the cycle to cut a minor rather than a patch, and cutting
a minor is what retires the line `main` is leaving in the same cycle. A cycle
carrying neither a breaking change nor a new feature is a patch cycle: it
retires nothing and passes step 2 straight through.

One property follows from that order: the audit is the last thing that
merges — nothing lands between it and the tag it clears. A fix merged onto
`main` after the audit would ship unaudited in the head release, defeating the
point of auditing at all.

The single publish in step 5 runs through the machinery below — the head
release by merging its release PR.

### Two-tier test fence: merge gate vs. release gate

Every PR passes through exactly one of two fixed gates, never a per-diff
selection between them:

- **Merge gate** — the required-status-checks set on `main`, waited on by
  every ordinary PR: `check`, `lint`, `CodeQL`, `agpl-clean`, `schema-ddl`,
  `config-docs`, `pr-body`, `link-check`, `forbid-skip` (which subsumes the
  soft-assert / escape-hatch / feature-discipline / should-skip scans),
  `forbid-deferral`, `forbid-sql-raw` and `forbid-chplan-fn-literal` (both
  steps of `forbid-skip`), `update-golden-guard`, `quickstart`, `probe`,
  `chart-validate`, `coverage`, `property`, and `strict-scan`. It is small and fast by
  construction: build/correctness basics and the discipline scans, with the
  substrate smoke lanes and every other cost-dominating lane deferred to the
  release gate below.
- **Release gate** — the full matrix: every merge-gate check plus the
  cost-dominating lanes an ordinary PR does not need to wait on —
  `perf-guards` and `benchstat diff`, the chDB `roundtrip` / `integration` /
  `chdb-build` lanes, `gremlins` mutation testing, all six `compatibility/*`
  differential heads, `migration-e2e`, `perf-nightly` (the #2370 real-data
  regression gate — like `migration-e2e` it has no `pull_request:` trigger
  at all, only `push: [main, release/*.x]` + `schedule` + manual dispatch,
  so it never runs on an ordinary PR, heavy or otherwise), and the substrate
  smoke lanes `compose-smoke` / `dashboard` (never a branch-protection required context
  on `main` — `release.yml`'s preflight is their only reader; see
  `e2e.yml`'s header). Each of those lanes still triggers on every
  `pull_request` (so its check-run stays visible for author feedback), but
  short-circuits to a fast no-op unless the event is a push to `main` / a
  maintenance `release/*.x` branch, a schedule, a manual dispatch, or a pull
  request whose head branch starts with `release/` — the exact `RUN_HEAVY`
  pattern `coverage.yml` and `property.yml` originated (`compose-smoke` /
  `dashboard` short-circuit on the same ordinary-PR / release-PR split,
  documented in `e2e.yml` rather than through `RUN_HEAVY` itself). A release
  PR's head branch always matches, so its green status reflects the complete
  matrix; an ordinary PR pays only the merge gate's cost. `mutation` keeps its
  own diff-scoped selection (run only the phases whose package the PR
  touched) rather than a blanket no-op, because that scoping is what closed
  the v1.13.2-cycle hollow-green class described in `mutation.yml` — but
  `mutation` was never required by either gate; it is informational
  everywhere (see the de-gated-lanes table below).

This is a fixed split, not a per-diff selection: an ordinary PR always runs
the same merge-gate set regardless of which files it touches, and a release
PR always runs the same release-gate superset. An earlier, more elaborate
design attempted to route each PR through a *predicted* lane subset based on
its diff's blast radius; a backtest against real PRs found the predictor fell
back to the full lane set on the large majority of them anyway, so it bought
none of its intended savings while adding real selection-logic risk. The
two-tier split above replaces that attempt.

### Release pipeline (publish-on-merge)

Cerberus publishes when a **validated release PR is merged to main**, not when
a raw tag is pushed (release-please-style). The flow:

1. **Open a release PR.** Apply a `release:*` label to any issue (or run the
   `prepare-release` workflow manually). `prepare-release.yml` bumps the chart
   `version:` and/or `appVersion:`, rewrites the CHANGELOG, regenerates the
   chart README, and opens a PR from a `release/v<app>-chart-<chart>` branch.
2. **The PR runs the release gate.** Because the head branch starts with
   `release/`, every release-gate lane above (the e2e `split` + `crawl` legs
   included) does its real work instead of short-circuiting to a no-op, so a
   release PR's green status reflects the *complete* matrix.
3. **Merge when green.** The maintainer merges once every required check is
   green on a tree up to date with main. That merge-when-green gate covers
   only the checks that are native branch-protection required contexts;
   `compose-smoke` / `dashboard` / `profile` are deliberately not among them
   (see "Two-tier test fence" above), so GitHub's native auto-merge does not
   wait for them — a release PR can auto-merge while one of them is still red.
   `preflight` (below) is what catches that: it re-reads the release-staging
   PR's own `dashboard` check-run — the one CI run that ever exercises the
   k3d Grafana crawl against a release commit — directly from the source PR,
   rather than trusting the weaker push-triggered proxy (tsouza/cerberus#2361).
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
  not passed. It also re-validates `dashboard` a second, more specific way:
  `dashboard`'s crawl leg never runs on a push (only on a `release/*`-headed
  PR, schedule, or dispatch), so the push-triggered check-run this step
  otherwise reads is a smoke-only proxy with no crawl-coverage evidence for
  the exact commit being published. `release-source-pr-dashboard-gate.mjs`
  resolves the release-staging PR GitHub associates with the merge commit and
  re-reads *that* PR's own `dashboard` check-run instead — closing the gap
  that let PR #2360 auto-merge to main with a red crawl result
  (tsouza/cerberus#2361). Separately, before reading `RELEASE_REQUIRED_CHECKS`
  off the commit, `preflight` also resolves the PR that *produced* the merge
  commit (`lib/resolve-source-pr.mjs`, same `GET /commits/{sha}/pulls`
  endpoint, but filtered on an EXACT `merged && merge_commit_sha === sha`
  match rather than a `release/*` head-ref shape) and credits a required
  lane's green check-run posted on that PR's own tip commit too, alongside
  the commit's own. A squash-merged PR's tip tree is byte-identical to the
  merge commit's, so this lets a lane's `push:`-triggered re-run become
  provably unnecessary for a future release without `preflight` losing
  visibility into the validation that already happened on the PR
  (tsouza/cerberus#2394). It changes nothing about `preflight`'s behaviour
  today — every lane still posts its own check-run on the push commit, same
  as before — and resolves to nothing on the maintenance path, which merges
  no PR at all (see "Maintenance lines" below).
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
  release published most recently — which on a maintenance backport would aim
  `/releases/latest` at an older supported line.
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

| Lane                       | Why it does not gate a publish                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `compose-smoke-shard-info` | A matrix child of `compose-smoke`, which is required. The aggregate deliberately does not `needs:` the crawl info shard, so the shard posts its own check-run; treating that run as required would let a flake in an explicitly non-blocking shard hold a release.                                                                                                                                                                                                                                                 |
| `mutation`                 | A test-QUALITY ratchet, not a property of the artifact — and not a required check on either gate (its diff-scoped selection runs on ordinary PRs for author-time visibility only; see "Two-tier test fence" above). Requiring it here would put its ~11-leg full-sweep matrix on the critical path of every publish. On a maintenance line — where there is no PR at all — this means a hotfix publishes without a mutation verdict; that is the accepted cost of shipping hotfixes promptly.                      |
| `gremlins`                 | The `mutation` aggregator's own matrix legs (e.g. `gremlins phase4-promql-a`) post their OWN check-runs, under their own names — they do not share the `mutation` prefix, so de-gating `mutation` alone never covered them. Same reasoning as that row: a test-quality ratchet, not a property of the artifact. Caught when v1.16.0's release commit blocked on 6 pre-existing, already-tracked red `gremlins phase4-promql-*` legs even though `mutation` itself was already de-gated.                            |
| `drought`                  | `chaos-not-applicable-rate.yml`'s Wednesday-cron detector for the chaos lane's silent not-applicable outcomes. It mines chaos-job run HISTORY, not the commit it happens to post against, so a red run says nothing about the commit being released — its own header comment already excludes it from PR gating for the identical reason. Left required, an unlucky coincidence between the cron and a release push would hold a release hostage to accumulated chaos-lane drift the release itself did not cause. |

#### Homebrew tap

Stable releases publish a Homebrew cask to the
[`tsouza/homebrew-tap`](https://github.com/tsouza/homebrew-tap) tap, so operators
can install the single `cerberus` binary with:

```sh
brew install --cask tsouza/tap/cerberus
```

That is also the shortest way to get the `cerberus migrate` CLI onto the
machine that holds an operator's rules and dashboards, so the migration
playbook points at it as the default install path — see
[getting the `cerberus` binary](migration.md#step-1-install-the-binary).

This is wired via the goreleaser `homebrew_casks:` block. A *cask* is the right
vehicle for a pre-built binary — a formula describes something Homebrew builds
from source — and it is not a macOS-only choice, though not for the reason casks
are usually assumed to be Mac-bound. The entire Linux gate is Homebrew's
`check_stanza_os_requirements`, which proceeds only when a cask declares no
top-level `depends_on macos:` **and** every one of its artifacts is supported off
a Mac, and otherwise raises `<cask>: cask requires macOS.`. Both conjuncts
matter: `depends_on macos:` *is* consulted on Linux, contrary to folklore — what
makes it harmless here is that `requires_macos?` is set only by a *top-level*
`depends_on macos:`, so neither the implicit `MacOSRequirement` Homebrew attaches
to every cask nor goreleaser's output (which declares no `depends_on` at all)
trips it. The unsupported artifacts are the sixteen classes in
`MACOS_ONLY_ARTIFACTS` (`app`, `pkg`, `service`, …) plus an `installer` in its
`manual:` form — a scripted `installer` is portable. `supports_linux?` is not the
install-time predicate at all; its only caller is homebrew-cask's own CI matrix
generator, and it reports `false` for this cask even though `brew install` works.
What makes cerberus installable under Linuxbrew is that
its sole artifact is a plain `binary`, and that goreleaser emits `on_linux`
url/sha256 pairs for `linux_amd64` and `linux_arm64` from the same `builds:`
matrix that feeds the darwin ones. Because the release binaries are
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
install runs the *bare* `brew install tsouza/tap/cerberus` rather than the
`--cask` form operators are given, because the bare ref is the strictly harder
one to satisfy: the two are not equivalent when a tap serves a same-named
formula — Homebrew resolves the bare ref to the FORMULA while `--cask` sails
past it — so proving the bare ref lands on the cask proves the tap is
unambiguous, which a `--cask` install cannot observe. goreleaser writes
`Casks/cerberus.rb` and deletes
nothing, so the tap's whole file listing is additionally scanned for a leftover
formula — every root Homebrew loads formulae from (`Formula/`,
`HomebrewFormula/`, the tap root, the first two recursively so a sharded
`Formula/c/cerberus.rb` counts), not just the historical path — and the smoke
fails while one is present. The repair belongs to the tap repository, which no
release run can touch. Because the ref
being installed is the ambiguous one, the smoke also states *which* artifact
Homebrew picked: after the install, `cerberus` must appear in
`brew list --cask --versions` and must not appear in `brew list --formula
--versions`. Neither `command -v` nor `--version` can tell those two apart.

Deleting the formula decides what a **new** install receives. What an **existing**
formula install receives is decided by a second file, `tap_migrations.json` at the
tap root:

```json
{
  "cerberus": "tsouza/tap"
}
```

Homebrew looks every package an update *deleted* up in that map. A hit whose
target resolves to a cask in the same tap makes `brew update` unlink the formula
and install the cask in its place, printing `cerberus has been migrated from a
formula to a cask.`. A miss — including the map being absent — is
indistinguishable from "this package did not move", and that is the whole failure
mode: `brew upgrade` files the package under *Deleted Installed Formulae* and
moves on, because a deleted formula has no newer version to move to. Installing
the cask by hand afterwards does not rescue it either, since the formula's keg
still owns `<prefix>/bin/cerberus` and the cask declines to link over it
(`It seems there is already a Binary at … from formula cerberus; skipping link`).
The machine ends up with the new cask in the Caskroom, the old binary on `PATH`,
and no error printed anywhere. The users it strands are the ones who installed
cerberus *earliest*.

The target is written as the bare tap name, which is how homebrew/core spells its
own formula-to-cask migrations. Homebrew splits it on `/`: a two-part value has no
name component and is read as "same package, that tap", installing the
fully-qualified `tsouza/tap/cerberus`. A three-part `tsouza/tap/cerberus` takes a
different branch that keeps only the trailing name and installs the bare token
`cerberus`, resolved against whichever tap answers first — so the two spellings
are different instructions, not variants of one, and `brew-smoke.mjs` accepts only
the first. It reads the map on **every** release rather than treating the
migration as a one-time event, because the file is a blob in a repository this one
cannot write: nothing else would notice it being reverted, renamed or hand-edited,
and no fresh install — which is every install path CI performs — can observe its
absence.

The two shapes that write no cask are **not** skipped — each takes the
opposite assertion. An `rc.*` must have written none, so a cask declaring the
prerelease version is a reported regression; a release that is not the highest
stable tag must have left a strictly NEWER cask in place, so a tap that has
fallen back to the backport's own version is one too. The job runs after
`publish` because
`brew install` downloads the release tarball, which 404s while the release is
still a draft. It runs as a `macos-latest` + `ubuntu-latest` matrix, so that the
cask installs under Linuxbrew is exercised rather than assumed — cerberus is a
Linux-first server binary, and a cask that has lost its Linux artefacts installs
flawlessly on a Mac. (The Ubuntu image does ship Homebrew, under
`/home/linuxbrew`; it just leaves it off `PATH`, which is why one Linux-only
step adds it.) The cask's cross-platform *shape* — all four os/arch artefacts
present, no macOS-only artifact stanza — is additionally asserted from the cask
source on both legs and on all three release branches, because that is the one
failure neither install can see from the other's side.

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

Cerberus maintains **exactly one minor release line: the current minor**. When
a new minor ships, the line it just superseded reaches **end-of-life (EOL)**
immediately. An EOL line:

- gets **no further hotfixes**;
- has its `release/<major>.<minor>.x` maintenance branch **deleted
  automatically** when the new minor ships (the `eol-retire` job — see below);
- has its maintenance-line **publish/CI disabled** (the `preflight` gate refuses
  to publish a push on an out-of-window line — see below).

What stays: the **version tags and GitHub Releases** for EOL versions **remain
available** — only the future-hotfix branch is removed. Already-published images,
charts, and binaries are never unpublished.

**Worked example.** At **v1.6.x** current, the only supported line is
`release/1.6.x`. Shipping v1.6.0 retired `1.5.x`: `release/1.5.x` was deleted,
`v1.5.*` tags and Releases stay up. No older line was ever kept — a minor line's
support ends the moment the next minor ships.

**Enforcement.** The support window is enforced on both halves of the EOL
policy, sharing one piece of window math
(`.github/scripts/release-preflight.mjs`, `SUPPORTED_MINOR_LINES` — single
source of truth):

- **Passive (publish refusal).** The maintenance-release `preflight`
  (`supportWindowProblem`) refuses a push to a `release/<major>.<minor>.x` line
  that is 1+ minor(s) behind the current minor (derived from the stable `v*` tag
  set) — **before** any artifact publishes, independent of how green the commit
  is. An out-of-window line takes no further hotfixes.
- **Active (branch retirement).** When a NEW minor actually ships, the
  `eol-retire` job in `release.yml` deletes the maintenance branch that just
  fell out of the window **automatically** — no manual maintainer step. It runs
  only after a successful new-version publish, computes the line via
  `retireLineForPublish` (the same `SUPPORTED_MINOR_LINES` window: publishing
  `1.6.0` retires `release/1.5.x`, the line it just superseded), and deletes
  that `release/X.W.x` branch iff it exists. Guards: it retires **at most one**
  line and **only on a minor open**
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
merge --squash --delete-branch` flow is the source of truth
for operator-driven changes to the binary.
