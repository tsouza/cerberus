# Test strategy

Cerberus is tested in 13 layers, ordered roughly cheapest-and-fastest
to slowest-and-most-realistic. Each layer pins a different class of
contract: AST shape, plan-IR invariants, optimizer behaviour, emitted
SQL bytes, semantic equivalence under chDB execution, function-surface
parity, HTTP wire conformance, process lifecycle, browser UX,
deterministic failure-mode resilience, performance ceilings,
compute-fan-out guards, and live-stack chaos resilience.

This document is the canonical map of what each layer covers, where
the tests live, which CI gates run them, and how to add a new test
inside each layer.

## At a glance

| Layer | Name                                 | Lives in                                                                                          | Catches                                                                                                                                                                              | Misses                                                                                     |
| ----- | ------------------------------------ | ------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------ |
| 1     | Parser smoke / AST-shape pinning     | `internal/{promql,logql,traceql}/parser_*_test.go`                                                | Upstream parser renames a field, swaps an enum, changes a root-node type after a fork rebase                                                                                         | Semantic divergence below the AST surface                                                  |
| 2a    | chplan IR snapshots in TXTAR         | `test/spec/<head>/*.txtar` (`-- chplan --` sections)                                              | Lowering regressions that don't change emitted SQL bytes                                                                                                                             | Optimizer-introduced regressions (covered by `-- chplan_optimized --` pair)                |
| 2b    | Lowering edge cases                  | `internal/{promql,logql,traceql}/lower_*_test.go`                                                 | Edge inputs (NaN, empty matrix, scalar coercions) that don't appear in golden fixtures                                                                                               | Combinatoric blow-up — keep table-driven                                                   |
| 3     | chplan IR invariants                 | `internal/chplan/{equal,walk}_invariants_test.go`                                                 | `Equal()` false-positives / negatives; `Walk` / `Children` ordering drift; pointer-identity                                                                                          | Lowering bugs — IR is generic                                                              |
| 4     | Optimizer rule properties            | `internal/optimizer/{rule_interaction,termination,decision_pins,regression_bank}_test.go`         | Rule-pair commutation, non-termination, mis-rewrites, decision-pin regressions                                                                                                       | Cross-rule chDB row drift (covered by Layer 6 chDB property)                               |
| 5     | chsql Frag + QueryBuilder goldens    | `internal/chsql/{frag_goldens,query_builder_invariants,emit_node_goldens}_test.go`                | Frag render shape, slot-ordering invariants, append/replace semantics, Build idempotency                                                                                             | SQL that compiles but executes incorrectly — covered by Layer 6                            |
| 6a    | PromQL chDB roundtrip                | `test/spec/promql/*.txtar` (`-- seed --` / `-- expected_rows --`)                                 | Optimizer/emitter rewrites that change the row set                                                                                                                                   | Behaviour outside the seeded corpus                                                        |
| 6b    | LogQL chDB roundtrip                 | `test/spec/logql/*.txtar`                                                                         | Same as 6a for LogQL                                                                                                                                                                 | Same as 6a                                                                                 |
| 6c    | TraceQL chDB roundtrip               | `test/spec/traceql/*.txtar`                                                                       | Same as 6a for TraceQL                                                                                                                                                               | Same as 6a                                                                                 |
| 6d    | Function-surface parity ledger       | `test/surface-parity/`, `test/rejection-parity/`, `test/inventory/`                               | A symbol cerberus fails to lower (wrong-reject) or answers when the reference rejects (wrong-accept)                                                                                 | Whether an accepted symbol returns the *right rows* (Layer 6a-c)                           |
| 7     | HTTP handler conformance             | `internal/api/{prom,loki,tempo}/conformance_test.go`                                              | Wire-format drift, error envelope shape, header pins, range-param parsing, admission control                                                                                         | Real-network failure modes (Layer 10) and UX flows (Layer 9)                               |
| 7b    | Consumer-corpus replay               | `test/consumer-corpus/`                                                                           | Consumer-decode drift on captured Grafana request shapes (proto envelopes, bare JSON, drilldown queries)                                                                             | Shapes Grafana hasn't been observed sending — crawler mines captures                       |
| 8     | System / process lifecycle           | `internal/config/`, `internal/api/health/`, `cmd/cerberus/`, `internal/telemetry/`, `schema/ddl/` | Env-var contract, `/readyz` TTL coalescing, OTel telemetry attributes, signal-driven shutdown                                                                                        | Cross-process behaviour — Compose / k3d (Layer 9)                                          |
| 9     | Playwright UX flows                  | `test/e2e/playwright/*.spec.ts`                                                                   | Grafana Explore / Logs / Trace panel request sequences against cerberus's three datasource APIs                                                                                      | Pure backend logic — Layers 1–8                                                            |
| 10    | Chaos / failure-mode (deterministic) | `internal/{chclient,api/{prom,loki,tempo,admit}}/chaos_test.go`, `test/regression/goleak_test.go` | CH-failure, mid-stream cursor faults, goroutine leaks, panic-mid-handler slot release, CH-disconnect circuit breaker (stubbed-querier injection)                                     | Long-tail platform-specific failures; real-deployment fault behaviour (Layer 13)           |
| 11    | Perf benchmarks + alloc regressions  | `internal/*/*_bench_test.go`                                                                      | Allocation count regressions per pipeline stage; bounded-RSS streaming cursor                                                                                                        | Wall-clock perf regressions — left to `perf-benchmark.yml` benchstat                       |
| 12    | Compute fan-out guards               | `internal/perf/fanout`, `test/perf/` (scaling harness; cardinality / wall / decision ratchets)    | Upward fan-factor regression, a new unbounded shape (`CrossJoin` / uncapped `WITH RECURSIVE`), super-linear scaling                                                                  | A fan-out in a construct with no guard and no fixture (the nightly profiler)               |
| 13    | Live-stack chaos (real faults)       | `.github/scripts/chaos-run.mjs`, `test/e2e/chaos/manifests/`                                      | Resilience contracts under REAL faults against the k3d stack: CH-outage breaker trip + recovery, query-timeout breaker-neutrality, replica resilience, admit shed, network partition | Pure backend logic (Layers 1-8); steady-state UX (Layer 9) — chaos is fault-injection only |

## CI gates

The eleven **required** status checks on `main` are `check`, `lint`,
`forbid-skip`, `probe`, `roundtrip (promql)`, `roundtrip (logql)`,
`roundtrip (traceql)`, `compatibility/prometheus`, `compatibility/loki`,
`compatibility/tempo`, and `compose-smoke`. Every other job below is
informational — it runs (push-to-main, nightly, or dispatch) and reports,
but a red result does not block a merge. Informational does **not** mean
tolerated: a red informational lane is a real failure to fix, it is just
not wired as a branch-protection gate (typically because it needs the chDB
substrate, a Docker stack, or a soak streak before promotion).

One subtlety on the three `compatibility/<head>` checks: they are required,
but the *check* fails only on infrastructure breakage — per-case **numeric
parity drift is report-only** (rendered into the badge score, not the exit
code; see [`compatibility.md`](compatibility.md#ci-integration) and
[#503](https://github.com/tsouza/cerberus/pull/503)). The only lane that
hard-fails on a numeric parity diff is the *informational*
`compatibility/prometheus-forced-route` (`FAIL_ON_DIFF=1`). So the
required differential checks gate *that the harness runs clean*, while the
parity number itself is a continuously-measured score rather than a merge
gate. Promoting a fail-on-parity gate to required is a tracked improvement.

| Gate                                    | Workflow (job)                                       | Trigger                             | Required? | Scope                                                                                                                                                                                                                    |
| --------------------------------------- | ---------------------------------------------------- | ----------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `check`                                 | `ci.yml` (`check`)                                   | PR + push                           | Required  | `just test` (`go test -race -cover ./...`) + `just build`. Default-tag lanes: 1, 2a, 2b, 3, 4, 5, **6d** (surface / rejection / inventory parity ratchets), 7, 7b stub, 8, 10, 11, 12 solver-decision ratchet            |
| `lint`                                  | `ci.yml` (`lint`)                                    | PR + push                           | Required  | `golangci-lint` v2 + `go-arch-lint` + `actionlint` + `commitlint` (PR) + `markdownlint-cli2`                                                                                                                             |
| `forbid-skip`                           | `ci.yml` (`forbid-skip`)                             | PR + push                           | Required  | `t.Skip*`, discipline-erosion wording, soft-assertion / silent-recover, escape-hatch patterns, `should_skip` overlay, regex self-test                                                                                    |
| `probe`                                 | `chdb.yml` (`probe`)                                 | PR + push + nightly                 | Required  | chDB driver sanity (`TestChDBProbe`) + `just test-chdb` (api-handler + Layer 7b chdb lane)                                                                                                                               |
| `roundtrip (promql / logql / traceql)`  | `chdb.yml` (matrix)                                  | PR + push + nightly                 | Required  | TXTAR chDB roundtrip per head (Layer 6a-c)                                                                                                                                                                               |
| `compatibility/prometheus`              | `compatibility.yml` (`compatibility/prometheus`)     | PR + push + nightly + dispatch      | Required  | PromQL differential vs reference Prometheus (`prometheus/compliance` harness)                                                                                                                                            |
| `compatibility/loki`                    | `compatibility.yml` (`compatibility/loki`)           | PR + push + nightly + dispatch      | Required  | LogQL differential vs reference Loki + vendored `loki:pkg/logql/bench` corpus                                                                                                                                            |
| `compatibility/tempo`                   | `compatibility.yml` (`compatibility/tempo`)          | PR + push + nightly + dispatch      | Required  | TraceQL differential vs reference Tempo (cerberus-owned TXTAR corpus)                                                                                                                                                    |
| `compose-smoke`                         | `e2e.yml` (`compose-smoke`)                          | PR + push + nightly                 | Required  | `docker compose up --wait` + `/healthz` / `/readyz` / Grafana `/api/health` + Playwright catch-net + `compose` crawl (lean PR, full nightly)                                                                             |
| `compatibility/prometheus-forced-route` | `compatibility.yml` (forced-route job)               | PR + push + nightly + dispatch      | Info      | Corpus-wide proof that the solver route B (`CERBERUS_EVAL_ROUTE=sharded`) is byte-identical to route A vs reference Prom                                                                                                 |
| `compatibility/promql-surface`          | `compatibility.yml` (`compatibility/promql-surface`) | PR + push + nightly + dispatch      | Info      | Re-probes a **flag-ON** reference Prometheus over every `parser.Functions` symbol; asserts cerberus rejects nothing the reference accepts. Pins `test/surface-parity/inventory.json` against drift (Layer 6d, live half) |
| `perf-guards`                           | `chdb.yml` (`perf-guards`)                           | PR + push + nightly                 | Info      | `just perf-chdb` (`go test -tags chdb ./test/perf/...`): the cardinality / scale-wall ratchets, per-construct scaling harness, and cycle-guards (Layer 12 chdb lanes)                                                    |
| `perf-profile`                          | `perf-profile.yml` (`profile`)                       | push + nightly + dispatch           | Info      | Corpus-wide compute-fan-out profiler over every executable TXTAR fixture (EXPLAIN + per-subquery `count()` fan-factor); top-40 to step summary (Layer 12, Component B)                                                   |
| `perf-benchmark`                        | `perf-benchmark.yml` (`benchstat diff`)              | PR (path-match) + weekly + dispatch | Info      | benchstat wall-clock regression vs baseline (Layer 11)                                                                                                                                                                   |
| `dashboard`                             | `e2e.yml` (`dashboard`)                              | push + nightly + dispatch           | Info      | k3d + cerberus + Grafana + Playwright full smoke + `k3d` crawl (Layer 9)                                                                                                                                                 |
| `chaos`                                 | `e2e.yml` (`chaos`)                                  | push + nightly + dispatch           | Info      | k3d live-stack fault injection — resilience contracts under real faults (Layer 13). Phase-1 on push; full set on nightly/dispatch                                                                                        |
| `startup-bench`                         | `e2e.yml` (`startup-bench`)                          | push + nightly + dispatch           | Info      | cerberus reaches `/healthz` under 2 s against an inline ClickHouse                                                                                                                                                       |
| `mutation` (per phase)                  | `mutation.yml` (matrix)                              | push + nightly + dispatch           | Info      | gremlins per package (`chplan` / `chsql` / `optimizer` / `promql` / `logql` x4 / `traceql` / `qlcommon`) at the phase efficacy floor                                                                                     |
| `property`                              | `property.yml`                                       | push + nightly + dispatch           | Info      | rapid-driven oracle property tests, PromQL / LogQL / TraceQL (Layer 4 + 6 cross-check)                                                                                                                                   |
| `coverage`                              | `coverage.yml`                                       | push + nightly + dispatch           | Info      | merged default-tag + chdb-tagged cover profile, per-package summary                                                                                                                                                      |

The `compatibility/promql-surface`, `compatibility/prometheus-forced-route`,
and `perf-guards` lanes ride the same `compatibility.yml` / `chdb.yml`
workflows as their required siblings and run on every PR, but are not yet
promoted to branch-protection gates — they stay informational until each has
held a green soak streak (the same flip discipline the three
`compatibility/<head>` heads went through). The chdb-free half of the
function-surface ledger (`test/surface-parity/`, `test/rejection-parity/`,
`test/inventory/` regenerability ratchets) DOES gate on every PR through the
required `check` job, so a wrong-reject / wrong-accept regression fails red
regardless of the live-reference lane's gate status.

## Per-layer guidance

### Layer 1 — Parser smoke / AST-shape pinning

Tests live in `internal/{promql,logql,traceql}/parser_*_test.go`. Each
exercises the upstream parser on a corpus of queries representative of
cerberus's lowering surface and asserts:

- The parse succeeds for valid input and the AST root-node type is the
  expected one.
- Invalid input fails with the documented error class.
- Field accessors used by `lower.go` produce the expected shapes (e.g.
  `LabelMatchers.Type`, `RangeAggregation.Operation`).

Add a case by appending to the table-driven test in the relevant file.
The corpus deliberately overlaps with `test/spec/` fixtures so a
parser-shape change surfaces here AND in the golden TXTAR diff.

### Layer 2a — chplan IR snapshots in TXTAR

`test/spec/<head>/<name>.txtar` carries an `-- input --` section (the
query), a `-- sql --` section (the emitted SQL), and a `-- chplan --`
section (the deterministic IR pretty-printer output from
`test/spec/chplan_print.go`). Lowering changes that alter the IR but
not the SQL surface here.

Use the `/cerberus:add-fixture` skill to scaffold; run `just
update-golden` after the lowering lands to fill in the expected text.

### Layer 2b — Lowering edge cases

Table-driven tests in `internal/<head>/lower_*_test.go` cover edge
inputs that don't appear in the golden corpus: NaN scalars, empty
matrices, type coercions, off-by-one anchor counts.

### Layer 3 — chplan IR invariants

`internal/chplan/equal_invariants_test.go` and `walk_invariants_test.go`
pin the generic IR contract: `Equal()` is symmetric and reflexive,
`Walk` and `Children` iterate in the documented order, pointer identity
is not load-bearing.

### Layer 4 — Optimizer rule properties

`internal/optimizer/rule_interaction_test.go` runs every rule pair on a
small corpus and asserts commutativity (or documented
non-commutativity). `termination_test.go` runs the fixpoint loop with
a bounded iteration cap to catch non-terminating rewrites.
`decision_pins_test.go` pins specific input → rewrite outcomes that the
optimizer is committed to.

### Layer 5 — chsql Frag + QueryBuilder goldens

`internal/chsql/frag_goldens_test.go` pins every Frag type's render
output. `query_builder_invariants_test.go` confirms slot-ordering
semantics (`Select` then `From` then `Where` etc.) and Build
idempotency.

### Layer 6 — chDB roundtrip

Fixtures with both `-- seed --` and `-- expected_rows --` sections run
under the `chdb` build tag. The runner DDL-applies the OTel-CH schema,
loads the seed rows, executes the emitted SQL, and compares the result
set to `expected_rows`. `just update-golden` regenerates this layer too
— it runs a second, chdb-tagged pass over `./test/spec/...` so
`expected_rows` cells can never go stale behind a `-- sql --` change
(it requires libchdb.so; see `just chdb-install`). Use `just spec-chdb`
to verify locally without rewriting.

### Layer 6d — Function-surface parity ledger

Layers 6a-c prove that an *accepted* query returns the right rows. Layer
6d proves cerberus accepts and rejects the right *set* of grammar symbols
in the first place — the conformance frontier between the three upstream
parsers' grammars and what cerberus's `parse → fold → lower → optimize →
emit` pipeline actually admits. Three sibling packages, all chDB-free, all
gating on every PR through `check`:

- **`test/surface-parity/`** — the authoritative ledger. It enumerates
  every symbol the three upstream parser symbol tables expose (PromQL
  `parser.Functions` + aggregators + binary ops + modifiers, LogQL `Op*`
  consts, TraceQL intrinsics + metrics-ops), runs the cerberus verdict
  (accept / reject) and the reference-backend verdict on each, and
  classifies every pair into a four-way grid: `parity-accept` (both
  accept), `parity-reject` (both reject), `wrong-reject` (cerberus 422s a
  symbol the reference accepts — a real coverage gap), `wrong-accept`
  (cerberus answers a query the reference won't). `inventory.json` is the
  pinned artifact; `inventory_test.go` runs a three-leg ratchet
  (regenerability + a raise-only floor on the wrong-reject and wrong-accept
  sets), so a NEW wrong-reject — a freshly-added grammar symbol cerberus
  doesn't lower, or one that regressed from accept — fails CI red, and a
  burndown that *closes* a gap also fails until the inventory is
  regenerated (`CERBERUS_UPDATE_INVENTORY=1`), keeping the ledger in
  lock-step with the surface.

  The PromQL reference oracle is the **flag-ON** reference Prometheus HTTP
  verdict (started with `--enable-feature=promql-experimental-functions`,
  matching cerberus's own parser config), pinned in
  `promql-reference-verdicts.json`. The in-process ratchet reads the pinned
  artifact (Docker-free); the `compatibility/promql-surface` CI job
  re-probes the live reference and fails on drift, so the artifact can
  never silently diverge from the real backend.

- **`test/rejection-parity/`** — the SITE-based complement. Where
  surface-parity starts from the parser's accepted grammar, rejection-parity
  diffs cerberus's KNOWN 422 code-sites against the reference's error class
  for the same probe, so a rejection whose *message / status* drifts from
  the reference is caught even when the accept/reject verdict agrees.
  `catalogue.json` + `catalogue_test.go` ratchet it the same way.

- **`test/inventory/`** — per-head capability inventories
  (`{promql,logql,traceql}_test.go`), regenerable under the same
  `CERBERUS_UPDATE_INVENTORY=1` convention.

The user-facing translation of this ledger — every function / operator /
intrinsic with its support status — lives in
[`coverage.md`](coverage.md), which is generated from `inventory.json` and
presents the ledger classes in honest support language (an experimental fn
cerberus implements that the flag-OFF reference would reject is "Supported
(experimental)", not a raw "wrong-accept").

### Layer 7 — HTTP handler conformance

`internal/api/{prom,loki,tempo}/conformance_test.go` exercises every
documented HTTP endpoint with representative payloads and asserts the
wire envelope shape (`status`, `data.resultType`, content keys),
response headers, error envelope, and admission-control 503 + `Retry-After`.

### Layer 7b — Consumer-corpus replay

`test/consumer-corpus/` is the shift-left lane for consumer-contract
bugs: a corpus of REAL request shapes Grafana sends
(`grafana-<version>/*.json`, one entry per file with provenance,
the Grafana-side request, and per-entry expectations), replayed
against the in-process handlers and decoded EXACTLY as the consumer
decodes — strict gogo/proto unmarshal into `tempopb` types for the
Tempo proto endpoints, bare logproto-shaped JSON for Loki, Prom API
envelopes for Prometheus. The 2026-06 incident week (bare `Trace` vs
`TraceByIDResponse` #764, enveloped `detected_fields` #774, missing
`spanSets` #770, drilldown `<groupBy> != nil` 422s, blank regex
`__name__` breakdowns #769) was only caught by the e2e browser stack;
every one of those is reproducible here at unit cost.

Two lanes share the corpus: the default-tag lane (runs in `check`)
backs handlers with canned-row stubs and pins routing, status, and
consumer decodability; the `chdb`-tagged lane (runs in the chdb
workflow's `probe` job via `just test-chdb`) executes the full
parse → lower → optimize → emit → chDB pipeline over small seeds and
additionally evaluates each entry's data predicates. A ratchet
meta-test forbids corpus shrink (total + per-datasource entry floors
are raise-only) and rejects entries naming decoders / predicates /
stub fixtures the harness doesn't implement.

Division of labour with Layers 7 and 9: Layer 7 pins cerberus's OWN
wire contract endpoint by endpoint; Layer 7b pins what GRAFANA
actually sends and reads, request by captured request. The e2e
crawler and drilldown specs (Layer 9) are the corpus MINERS — when a
Grafana bump introduces a new request shape, capture it into a new
version-keyed corpus directory rather than widening assertions in
place. Known-unfixed bugs stay as failing entries (never tolerated,
never allow-listed): a red corpus entry is the layer doing its job.

### Layer 8 — System / process lifecycle

Covers env-var parsing (`internal/config/`), `/healthz` + `/readyz`
TTL coalescing, OTel resource attribute composition, schema DDL
idempotency, and signal-driven shutdown.

### Layer 9 — Playwright UX flows

`test/e2e/playwright/*.spec.ts` boots a k3d cluster (cerberus +
ClickHouse + Grafana + telemetrygen), provisions the cerberus
datasources, and walks Explore / Dashboard / Logs / Trace panel
flows.

### Layer 10 — Chaos / failure-mode (deterministic)

`internal/{chclient,api/...}/chaos_test.go` injects CH connection
drops, mid-stream cursor faults, panic-mid-handler, and the
`goleak`-pinned no-goroutine-leak invariant via a **stubbed querier** —
deterministic, in-process, on the required `check` lane. The
CH-disconnect circuit breaker shields cerberus from amplifying transient
CH outages into a 503 storm. This layer proves the *logic* of each
resilience contract; Layer 13 proves the same contracts hold against a
*real deployment* under *real faults*.

### Layer 11 — Perf + alloc regressions

`BenchmarkXxx` measures per-stage allocation counts via
`testing.AllocsPerRun`. `TestAllocs_Xxx` pins documented zero-alloc
hot paths (e.g. emitter slot append, Frag render). benchstat-based
wall-clock comparisons live in `perf-benchmark.yml` (manual dispatch).

### Layer 12 — Compute fan-out guards

The axis Layer 11's read-side benchmarks are blind to: peak intermediate
cardinality and wall-time scaling against a query parameter (step count,
chain depth, recursion depth). The thing cerberus watches is the **fan
factor** — peak intermediate rows ÷ leaf scan rows — and four lanes hold
it flat from cheap-static to broad-corpus (see
[`performance.md`](performance.md#how-fast-is-kept-fast--the-assurance-framework)
for the strategy):

- **Static fan-out lint** (`internal/perf/fanout`, in `check`) — flags
  structurally-unbounded shapes (an unbounded `CrossJoin`, an `arrayJoin`
  feeding a `JOIN`, an uncapped `WITH RECURSIVE`, a correlated subquery) on
  the lowered plan *and* emitted SQL of every corpus fixture. No chDB
  needed.
- **Per-construct scaling harness** (`test/perf/scaling`, chdb `perf-guards`
  job) — sweeps a parameter for a known-hot construct and asserts wall-time
  stays sub-linear *and* peak intermediate cardinality stays bounded.
- **Cardinality + scale-wall ratchets** (`test/perf/cardinality_ratchet_test.go`,
  `scale_wall_pin_chdb_test.go`, chdb `perf-guards` job) — pin every
  fixture's fan factor + structural flags + recursion depth in
  `cardinality-baseline.json` / `scale-wall-baseline.json` and fail on an
  upward regression, a new unbounded shape, or a deeper recursion. A
  decrease never blocks; the ceiling tightens only on a deliberate
  `just update-cardinality-baseline`.
- **Solver-decision ratchet** (`test/perf/solver_decision_ratchet_test.go`,
  chDB-free, in `check`) — pins the per-fixture route A/B classification
  against `solver-decision-baseline.json` so a routing-heuristic change
  surfaces in the diff.
- **Corpus-wide fan-out profiler** (`test/perf/profile`, the `perf-profile`
  workflow, nightly + push, informational) — profiles every executable
  TXTAR fixture via in-process chDB `EXPLAIN` + per-subquery `count()`,
  ranks by fan factor, and surfaces the worst as a job step-summary. The
  wide net for a fan-out in a construct nobody wrote a guard for.

### Layer 13 — Live-stack chaos (real faults)

The robustness layer *above* Layer 10's deterministic unit chaos: it
fault-injects against the running k3d e2e stack (cerberus + ClickHouse +
Grafana + OTel collector that `just e2e-up` stood up) and asserts the
landed resilience contracts hold under **real** faults, not
stubbed-querier injection. Lives as the `chaos` job in
`.github/workflows/e2e.yml`, driven by `.github/scripts/chaos-run.mjs`
(node ESM, kubectl + `fetch`) with the overlay/NetworkPolicy manifests
under `test/e2e/chaos/manifests/`. Locally: `just e2e-up &&
just e2e-seed-rolling && just e2e-wait-otel && just e2e-chaos-overlay &&
just e2e-chaos`.

What it proves (the contracts, mapped to their landing PRs):

- **Circuit breaker (#883).** `ch-pod-kill` deletes the single-replica
  CH Deployment (Recreate → clean outage). Under fault, a tight query
  loop forces the shared breaker OPEN: every head returns 503 +
  `Retry-After: 5` `errorType=unavailable` (accepting the documented
  502-then-503 ordering), `/readyz` goes 503 with `circuit` in the body,
  and `/healthz` stays 200 (liveness is breaker-independent — a CH
  outage must NEVER restart cerberus). After CH auto-recreates, the
  HALF-OPEN probe closes the breaker, `/readyz` and all heads return to
  200, `cerberus_ch_breaker_trips_total >= 1` (monotonic) and
  `cerberus_ch_breaker_state == 0`.
- **Per-query wall-clock timeout (#886).** `ch-slow-query-timeout`
  issues a deliberately slow `query_range` (wide range, 1 s step →
  millions of anchors) that blows past the small `CERBERUS_QUERY_TIMEOUT`
  the overlay set → clean 503 `errorType=timeout`. Critically
  **breaker-neutral**: CH code-159 `TIMEOUT_EXCEEDED` is coerced to
  success in `breaker.record`, so a burst of slow queries does NOT trip
  the breaker (`state == 0`, `trips_total` unchanged), `/readyz` stays
  200, and a separate fast query still 200s (admit slot + pooled
  connection released).
- **Replica resilience.** `cerberus-pod-kill` deletes ONE of the ≥2
  HPA-floor replicas (scoped by name, never both); the Service keeps
  serving from the survivor (aggregate success ≥ 95 %, retrying a single
  mid-drain connection reset). The replacement rejoins endpoints; the
  surviving replica set shows no unexpected restart (the killed pod is
  *replaced*, not restarted in place — so the lane deliberately does NOT
  inherit the dashboard job's blanket `restartCount == 0` assert).
- **Network partition (phase-2).** `ch-network-partition` applies a
  deny-egress NetworkPolicy (kube-router) to blackhole cerberus → CH —
  the slower path to the same breaker-OPEN end state. **Gated** on a
  runtime enforcement probe: if kube-router is not enforcing
  NetworkPolicy in the pinned k3d image, the scenario is recorded
  not-applicable (`::notice::`) and the breaker contract is covered by
  `ch-pod-kill` instead — never a vacuous pass.
- **Admission control + pool (phase-2).** `load-admit-saturation`
  bursts concurrency beyond the small overlay admit cap → some requests
  shed cleanly with 503 + `Retry-After: 1` `server saturated` while a
  below-cap request still 200s; `cerberus_admit_rejected_total` climbs;
  the breaker stays CLOSED (admit + pool-acquire rejections are
  breaker-neutral).
- **Handler panic (#885).** Already pinned deterministically by Layer 10
  — the live lane only corroborates that the process recovered cleanly
  from the cumulative fault storm (all 3 heads 200, no lingering 5xx) as
  a passive end-of-run health gate, not a dedicated scenario.

Design notes (flake resistance): every recovery check polls to a
**generous bounded deadline** (never asserts immediately after a fault);
faults are one-shot + idempotent (`kubectl delete pod` / `apply`
policy), retries live on the ASSERT side only; scenarios run
**sequentially with heal-between-each** so one scenario's residue can't
poison the next; metric-based asserts (read back through cerberus's own
Prom head — cerberus has no `/metrics` endpoint) are POST-recovery
corroboration with a settle poll, because OTLP → collector → CH flush
lags the fault by seconds, so during-fault timing keys on immediate HTTP
status + `/readyz` body + kubectl state. The lane is **informational**:
push-to-main + nightly + manual only, never a PR gate, never a
branch-protection required check (k3d is heavy and chaos flakes).

## Property tests

`test/property/` runs rapid-driven property tests under the `chdb`
build tag. The architecture is:

```text
test/property/
  framework.go        — rapid.Check driver + comparator
  gen/                — random data + query generators per head
  oracle/             — from-scratch evaluators (PromQL / LogQL / TraceQL)
                         that do NOT import internal/{promql,logql,traceql}
                         so the oracle is not the SUT
  promql_test.go      — wires gen + oracle + chDB exec for PromQL
  logql_test.go       — same for LogQL
  traceql_test.go     — same for TraceQL
```

Each test:

1. Draws a dataset (rapid).
2. Draws a query against that dataset.
3. Lowers the query to SQL, runs it under chDB.
4. Runs the from-scratch oracle on the same dataset.
5. Compares result sets.

Failures shrink to a minimal repro via rapid's automatic shrinking and
land in the standard test output.

## Gremlins mutation

Per-package mutation thresholds live in `.gremlins.yaml`; each package
runs as its own matrix entry in `.github/workflows/mutation.yml`. The
gate is informational on push-to-main; flipped to required when a
phase has held the 95% efficacy floor for a stable streak.

| Phase                       | Package              | Efficacy floor |
| --------------------------- | -------------------- | -------------- |
| `phase1`                    | `internal/chplan`    | 95%            |
| `phase2`                    | `internal/chsql`     | 95%            |
| `phase3-optimizer`          | `internal/optimizer` | 95%            |
| `phase4-promql`             | `internal/promql`    | 95%            |
| `phase4-logql-lower`        | `internal/logql`     | 95%            |
| `phase4-logql-aggregation`  | `internal/logql`     | 93%            |
| `phase4-logql-other-a`      | `internal/logql`     | 95%            |
| `phase4-logql-other-b`      | `internal/logql`     | 95%            |
| `phase4-traceql`            | `internal/traceql`   | 95%            |
| `phase5-qlcommon`           | `internal/qlcommon`  | 95%            |

`internal/logql` is split into four sibling matrix entries (each scoped
to `./internal/logql` but with disjoint `--exclude-files` regexes) to
keep the `go test ./internal/logql` cycle under the ubuntu-latest memory
ceiling; `phase4-logql-aggregation` sits one point lower at 93% to absorb
a documented equivalent mutant. See `.github/workflows/mutation.yml` for
the per-phase exclude sets.

A surviving mutant is either (a) a legitimately weak assertion that
needs strengthening, (b) a functionally-equivalent mutation (`<` vs
`<=` on a boundary that's never hit, slice-cap arithmetic that
`append` regrows past), or (c) a missing test. The gremlins JSON
artifact on each run names the file + line + mutation kind.

### Surviving-mutant policy

When a mutant survives the phase threshold, pick the remedy in this
order — the goal is to keep production code clear and let the test
suite carry the discipline:

1. **PREFERRED — prove equivalent.** Add a comment in the source
   explaining why the mutated branch is semantically identical to the
   original, then drop the phase efficacy threshold in `.gremlins.yaml`
   by 1 percentage point to absorb the equivalent mutant. The mutation
   count is now defensible and the source stays clear.
2. **ACCEPTABLE — add a distinguishing test.** Write a unit / property
   test whose output differs between the original and the mutated
   branch. This is the right call when the mutation reveals real
   under-tested behaviour.
3. **REJECTED — refactor production code to make the mutant
   distinguishable.** This is pattern #11 (DEFEAT-MUTANT) — the
   codebase loses clarity to satisfy the mutation tool. Don't do it.

Prior PRs #504 and #664 carry pattern-#3 refactors. They are not
reverted (their diffs are now load-bearing for the published
thresholds), but new violations should follow remedy #1 or #2.

## Grafana surface crawler (Layer 9 extension)

`test/e2e/playwright/crawl/` extends Layer 9 from *enumerated* UX
flows to *discovered* ones. Where the iterate-\* specs visit known
surfaces (dashboards, panels, the drilldown-app catalogue), the
crawler (`crawl/crawl.spec.ts`) BFS-walks every same-origin link
reachable from the Grafana root, canonicalizes URLs so the
visited-set converges (path + structural-param keys; dynamic
segments like service names, trace ids, and folder uids
parameterize; `/explore` collapses to one surface), and applies the
same universal oracles on every page — no per-page code:

1. **Zero browser console errors** (no cerberus-origin noise filter,
   ever).
2. **Zero non-2xx responses** on the datasource API families
   (`/api/ds/query`, `/api/dashboards/`,
   `/api/datasources/proxy/uid/`, `…/resources/`) and zero tunneled
   `.results.<refId>.error` bodies — the only sanctioned failures are
   those attributable to a panel's declared
   `cerberus.expect: "error:<substring>"` contract.
3. **Panel tri-state**: every rendered panel ends in
   has-data | declared-empty | declared-error; an undeclared
   "No data" fails with the panel title + URL.
4. **No page-level crash banner** and no visible `role="alert"`
   error banner.

**Interaction sweep** (`crawl/interactions.ts`). Visiting a surface
at its default control state is not enough: the 2026-06-10 maintainer
find — clicking the Traces Drilldown breakdown groupBy "kind"
attribute fired `{… && kind != nil} | rate() by(kind)` and cerberus
422'd on the nil-comparison — was a state no harvested link encodes.
After each surface's base audit the crawler therefore discovers its
view-affecting interactive controls and drives every planned
deviation, each against a **fresh navigation** of the surface
(deterministic provenance: one state = surface default + exactly one
control deviation). Control kinds discovered: tab strips
(`role=tablist`), radio groups (`role=radiogroup`, re-found at drive
time by option-name signature — the generated input ids are
mount-order dependent), select/combobox dropdowns (probed open to
learn their option sets; datasource pickers, sort-bys, level
filters), titled option lists (the Traces Drilldown attribute
picker), metric select tiles, and adhoc-filter builders (driven as
one representative key → value pair). Mutating affordances
(save/delete/create/add-tab), free-text search inputs, and the
time/refresh pickers (owned by iterate-time-ranges) are excluded —
the crawl stays read-only.

State identity follows the URL. A deviation the app encodes into the
URL becomes a **first-class surface**: the canonicalizer retains
*structural* params (`StructuralParamRule` in `crawl/lib.ts`) —
low-cardinality ones verbatim (`?actionView=comparison`,
`?var-groupBy=kind`) with the app's cold-boot default dropped, and
high-cardinality ones parameterized (`?metric={metric}`, the
`{service}` doctrine) — and the BFS visits the discovered state
fresh with the full oracle set. A deviation that does **not** encode
to the URL is audited in place with the same oracles and pins into
the inventory under the state notation
`<canonical>#<control>=<value>` (high-cardinality representatives
record `{rep}` so data-derived values can't flicker the inventory).

Bounding (the locked pairwise design, enforced in
`planInteractions`): structural controls (≤ 12 options) enumerate
**fully**; high-cardinality controls take **one representative**;
cross-control combos form **pairwise via surface chaining** — a
surface pinning one structural param sweeps with the representative
plan (one option per control, each interaction forming a param
pair), and surfaces pinning ≥ 2 params are terminal (visited, never
expanded). Every plan is hard-capped (24 single-sweep / 16 pairwise
per surface); overflow **fails the crawl listing the full plan** —
never a silent truncation. Depth doctrine unchanged: lean sweeps the
configured representative roots (the three drilldown app entries)
with one state per control; full sweeps every eligible surface
exhaustively.

Two sibling specs ride the same lane:

- `crawl/dsquery.spec.ts` — consumer-grade replays through
  `POST /api/ds/query`, the datasource plugin **backend**. The plugin
  decode layer (frames, RFC3339 shapes, enums) fails on wire drift
  the datasource-proxy probes pass through, so this is a distinct
  oracle, not duplication. (Tempo replays only `queryType: traceId`
  — the Tempo plugin backend rejects TraceQL search by design.)
- `crawl/lints.spec.ts` — deterministic data-quality lints. Each lint
  pins a named incident class: histogram degeneracy (all observations
  in one bucket fabricating constant quantiles) on every histogram
  family a quantile panel consumes, and identical-quantile-series
  (p50 ≡ p95 bitwise — the same single-bucket signature).

**One engine, N stack configs.** The crawler is a stack-agnostic
framework: the engine — BFS walk, URL canonicalization, the universal
oracles, the ds/query replays, the lints, and the inventory-ratchet
mechanics — lives in `crawl/lib.ts` + the three specs and never
branches on a stack name. Everything that legitimately differs
between Grafana deployments is declared as data in `crawl/stacks.ts`,
one `CrawlStackConfig` per stack: the default Grafana base URL, the
anonymous-auth assumption (typed `true` — the engine drives no login
flow, and the crawl proves the assumption live before walking), the
crawl scope rules, the per-stack inventory + exclusions file names,
the exact datasource UID set the ds/query replays pin (the live
`/api/datasources` answer must EQUAL it — provisioning drift fails
loudly), the lint input floors (floors on lint *input*, never
tolerances on verdicts), the lean representative seeds, and the hard
page caps. `CRAWL_STACK=<name>` selects the config: unset →
`playwright.config.ts` ignores `crawl/**` entirely (0 crawl tests);
an unknown name → loud error at config-load time, never a silent
skip.

Two stacks are registered: `compose` (the repo-root quickstart stack;
the `compose-smoke` job opts in with `CRAWL_STACK=compose` —
`SWEEP_DEPTH` follows the standard doctrine: `lean` per-PR, `full`
nightly) and `k3d` (the `dashboard` job in `e2e.yml`; its crawl step
runs on schedule + manual dispatch only, always at
`SWEEP_DEPTH=full` — the per-PR fast lane is the compose stack's
job). Depth changes states, never rules; `full` crawls exhaustively
under a **hard page cap that fails the run when exceeded**, so
surface growth forces a deliberate cap bump in `stacks.ts`.

**The surface-inventory ratchet.**
`crawl/grafana-surface-inventory.<stack>.json` pins each stack's
canonical visited set (mirroring `test/inventory/`'s regenerability
convention). A newly discovered surface — e.g. a Grafana bump adding
an app page — fails the crawl until the inventory is regenerated
deliberately:

```sh
# against a healthy instance of the named stack
CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=<stack> \
  npx playwright test crawl/crawl.spec.ts
```

Coverage shrink (a pinned surface no longer visited) fails
symmetrically and has no regen escape. The per-stack exclusions file
(`crawl/grafana-surface-exclusions.<stack>.json`) is empty by design
and shrink-biased; an entry must document genuine impossibility, and
a URL in both files fails as a stale exclusion.

**Adding a stack.** Register a `CrawlStackConfig` in
`crawl/stacks.ts`, commit an *empty* inventory (`"surfaces": []`,
`stack` field matching the config name, in canonical marshalled form)
plus an empty exclusions file, and wire `CRAWL_STACK=<name>` into the
stack's CI lane. The empty inventory is a **bootstrap convention,
never a steady state**: `assertInventoryBootstrapped` fails every run
loudly — with the regen instructions — until the first exhaustive
crawl's inventory is committed; only the regen run itself
(`CERBERUS_UPDATE_INVENTORY=1`) is exempt. The k3d stack bootstraps
this way because booting k3d locally is heavy: dispatch the `e2e`
workflow with `update_crawl_inventory=true`, then commit the uploaded
inventory artifact. The red crawl lane in the interim is deliberate
pressure — bootstrap cannot silently become permanent.

**Doctrine: the AI sweep generates oracle classes; CI runs them.**
The crawler exists because an off-CI AI screenshot sweep (2026-06-09)
found 34 unique error signatures across 55 BFS-visited pages —
several on surfaces no enumerated spec visits. Every find decomposed
into a deterministic signal once named; the AI's irreplaceable role
is *discovering which invariants to check*. When a future sweep (or a
human) finds a new bug class: name the deterministic signal, cite the
incident in a comment, implement it — as a universal per-page oracle
in `crawl/crawl.spec.ts` if it applies everywhere, or as a lint in
`crawl/lints.spec.ts` if it's an API-level data-quality rule — and
aggregate violations into the existing `failures[]` reporting. Never
per-surface tolerance lists: if a lint can't be deterministic without
one, narrow its scope by *consumption* (see the
histogram-degeneracy lint's quantile-panel scoping for the pattern).

## Regression meta-tests

`test/regression/` pins past CI failures so they can't silently
recur:

- `goleak_test.go` — process-level goroutine leak detection across
  every handler entrypoint.
- `justfile_test.go` — recipe-shape pins so a `just` syntax change
  doesn't silently break CI.
- `seed_test.go` — the e2e seed program's row-count and metric-name
  invariants.
