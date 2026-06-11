# Test strategy

Cerberus is tested in 11 layers, ordered roughly cheapest-and-fastest
to slowest-and-most-realistic. Each layer pins a different class of
contract: AST shape, plan-IR invariants, optimizer behaviour, emitted
SQL bytes, semantic equivalence under chDB execution, HTTP wire
conformance, process lifecycle, browser UX, failure-mode resilience,
performance ceilings.

This document is the canonical map of what each layer covers, where
the tests live, which CI gates run them, and how to add a new test
inside each layer.

## At a glance

| Layer | Name                                | Lives in                                                                                          | Catches                                                                                                              | Misses                                                                      |
| ----- | ----------------------------------- | ------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| 1     | Parser smoke / AST-shape pinning    | `internal/{promql,logql,traceql}/parser_*_test.go`                                                | Upstream parser renames a field, swaps an enum, changes a root-node type after a fork rebase                         | Semantic divergence below the AST surface                                   |
| 2a    | chplan IR snapshots in TXTAR        | `test/spec/<head>/*.txtar` (`-- chplan --` sections)                                              | Lowering regressions that don't change emitted SQL bytes                                                             | Optimizer-introduced regressions (covered by `-- chplan_optimized --` pair) |
| 2b    | Lowering edge cases                 | `internal/{promql,logql,traceql}/lower_*_test.go`                                                 | Edge inputs (NaN, empty matrix, scalar coercions) that don't appear in golden fixtures                               | Combinatoric blow-up — keep table-driven                                    |
| 3     | chplan IR invariants                | `internal/chplan/{equal,walk}_invariants_test.go`                                                 | `Equal()` false-positives / negatives; `Walk` / `Children` ordering drift; pointer-identity                          | Lowering bugs — IR is generic                                               |
| 4     | Optimizer rule properties           | `internal/optimizer/{rule_interaction,termination,decision_pins,regression_bank}_test.go`         | Rule-pair commutation, non-termination, mis-rewrites, decision-pin regressions                                       | Cross-rule chDB row drift (covered by Layer 6 chDB property)                |
| 5     | chsql Frag + QueryBuilder goldens   | `internal/chsql/{frag_goldens,query_builder_invariants,emit_node_goldens}_test.go`                | Frag render shape, slot-ordering invariants, append/replace semantics, Build idempotency                             | SQL that compiles but executes incorrectly — covered by Layer 6             |
| 6a    | PromQL chDB roundtrip               | `test/spec/promql/*.txtar` (`-- seed --` / `-- expected_rows --`)                                 | Optimizer/emitter rewrites that change the row set                                                                   | Behaviour outside the seeded corpus                                         |
| 6b    | LogQL chDB roundtrip                | `test/spec/logql/*.txtar`                                                                         | Same as 6a for LogQL                                                                                                 | Same as 6a                                                                  |
| 6c    | TraceQL chDB roundtrip              | `test/spec/traceql/*.txtar`                                                                       | Same as 6a for TraceQL                                                                                               | Same as 6a                                                                  |
| 7     | HTTP handler conformance            | `internal/api/{prom,loki,tempo}/conformance_test.go`                                              | Wire-format drift, error envelope shape, header pins, range-param parsing, admission control                         | Real-network failure modes (Layer 10) and UX flows (Layer 9)                |
| 7b    | Consumer-corpus replay              | `test/consumer-corpus/`                                                                           | Consumer-decode drift on captured Grafana request shapes (proto envelopes, bare JSON, drilldown queries)             | Shapes Grafana hasn't been observed sending — crawler mines captures        |
| 8     | System / process lifecycle          | `internal/config/`, `internal/api/health/`, `cmd/cerberus/`, `internal/telemetry/`, `schema/ddl/` | Env-var contract, `/readyz` TTL coalescing, OTel telemetry attributes, signal-driven shutdown                        | Cross-process behaviour — Compose / k3d (Layer 9)                           |
| 9     | Playwright UX flows                 | `test/e2e/playwright/*.spec.ts`                                                                   | Grafana Explore / Logs / Trace panel request sequences against cerberus's three datasource APIs                      | Pure backend logic — Layers 1–8                                             |
| 10    | Chaos / failure-mode                | `internal/{chclient,api/{prom,loki,tempo,admit}}/chaos_test.go`, `test/regression/goleak_test.go` | CH-failure, mid-stream cursor faults, goroutine leaks, panic-mid-handler slot release, CH-disconnect circuit breaker | Long-tail platform-specific failures                                        |
| 11    | Perf benchmarks + alloc regressions | `internal/*/*_bench_test.go`                                                                      | Allocation count regressions per pipeline stage; bounded-RSS streaming cursor                                        | Wall-clock perf regressions — left to `perf-benchmark.yml` benchstat        |

## CI gates

| Gate                          | Workflow                                       | Trigger                            | Required PR check? | Scope                                                                                                             |
| ----------------------------- | ---------------------------------------------- | ---------------------------------- | ------------------ | ----------------------------------------------------------------------------------------------------------------- |
| `check`                       | `.github/workflows/ci.yml` (job `check`)       | PRs + push                         | Required           | `go test -race -cover ./...` (Layers 1, 2a, 2b, 3, 4, 5, 7, 7b stub lane, 8, 10 default)                          |
| `lint`                        | `.github/workflows/ci.yml` (job `lint`)        | PRs + push                         | Required           | `golangci-lint` v2 + markdownlint + commitlint                                                                    |
| `forbid-skip`                 | `.github/workflows/ci.yml` (job `forbid-skip`) | PRs + push                         | Required           | `t.Skip*` + discipline-erosion wording + soft-assertion + skip-additions                                          |
| `probe`                       | `.github/workflows/chdb.yml` (job `probe`)     | PRs + push                         | Required           | chDB driver sanity (`TestChDBProbe`) + `just test-chdb` (api handler chdb tests, Layer 7b chdb lane)              |
| `roundtrip (<ql>)`            | `.github/workflows/chdb.yml` matrix            | PRs + push                         | Required           | TXTAR chDB roundtrip for promql / logql / traceql (Layer 6a-c)                                                    |
| `compatibility/<head>`        | `.github/workflows/compatibility.yml` matrix   | PRs + push + nightly               | Required           | Differential vs reference Prom / Loki / Tempo                                                                     |
| `dashboard` (E2E)             | `.github/workflows/e2e.yml` (job `dashboard`)  | push-to-main + nightly + manual    | Informational      | k3d + cerberus + Grafana + Playwright (Layer 9)                                                                   |
| `mutation` (per phase)        | `.github/workflows/mutation.yml` matrix        | PRs (path-match) + push + nightly  | Informational      | gremlins on each of `chplan` / `chsql` / `optimizer` / `promql` / `logql` / `traceql` / `qlcommon` @ 95% efficacy |
| `property`                    | `.github/workflows/property.yml`               | push-to-main + nightly + manual    | Informational      | rapid-driven property tests (Layer 4 + 6 cross-check)                                                             |
| `perf-benchmark`              | `.github/workflows/perf-benchmark.yml`         | PRs (path-match) + weekly + manual | Informational      | benchstat-based perf regression                                                                                   |
| `compose-smoke`               | `.github/workflows/compose-smoke.yml`          | PRs + push                         | Required           | `docker compose up --wait` + `/healthz` + `/readyz` + Grafana `/api/health`                                       |

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

### Layer 10 — Chaos / failure-mode

`internal/{chclient,api/...}/chaos_test.go` injects CH connection
drops, mid-stream cursor faults, panic-mid-handler, and the
`goleak`-pinned no-goroutine-leak invariant. The CH-disconnect circuit
breaker shields cerberus from amplifying transient CH outages into a
503 storm.

### Layer 11 — Perf + alloc regressions

`BenchmarkXxx` measures per-stage allocation counts via
`testing.AllocsPerRun`. `TestAllocs_Xxx` pins documented zero-alloc
hot paths (e.g. emitter slot append, Frag render). benchstat-based
wall-clock comparisons live in `perf-benchmark.yml` (manual dispatch).

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

| Phase              | Package              | Efficacy floor |
| ------------------ | -------------------- | -------------- |
| `phase1`           | `internal/chplan`    | 95%            |
| `phase2`           | `internal/chsql`     | 95%            |
| `phase3-optimizer` | `internal/optimizer` | 95%            |
| `phase4-promql`    | `internal/promql`    | 95%            |
| `phase4-logql`     | `internal/logql`     | 95%            |
| `phase4-traceql`   | `internal/traceql`   | 95%            |
| `phase5-qlcommon`  | `internal/qlcommon`  | 95%            |

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
visited-set converges (path-only keys; dynamic segments like service
names, trace ids, and folder uids parameterize; `/explore` collapses
to one surface), and applies the same universal oracles on every
page — no per-page code:

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
