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
| 8     | System / process lifecycle          | `internal/config/`, `internal/api/health/`, `cmd/cerberus/`, `internal/telemetry/`, `schema/ddl/` | Env-var contract, `/readyz` TTL coalescing, OTel telemetry attributes, signal-driven shutdown                        | Cross-process behaviour — Compose / k3d (Layer 9)                           |
| 9     | Playwright UX flows                 | `test/e2e/playwright/*.spec.ts`                                                                   | Grafana Explore / Logs / Trace panel request sequences against cerberus's three datasource APIs                      | Pure backend logic — Layers 1–8                                             |
| 10    | Chaos / failure-mode                | `internal/{chclient,api/{prom,loki,tempo,admit}}/chaos_test.go`, `test/regression/goleak_test.go` | CH-failure, mid-stream cursor faults, goroutine leaks, panic-mid-handler slot release, CH-disconnect circuit breaker | Long-tail platform-specific failures                                        |
| 11    | Perf benchmarks + alloc regressions | `internal/*/*_bench_test.go`                                                                      | Allocation count regressions per pipeline stage; bounded-RSS streaming cursor                                        | Wall-clock perf regressions — left to `perf-benchmark.yml` benchstat        |

## CI gates

| Gate                          | Workflow                                       | Trigger                            | Required PR check? | Scope                                                                                                             |
| ----------------------------- | ---------------------------------------------- | ---------------------------------- | ------------------ | ----------------------------------------------------------------------------------------------------------------- |
| `check`                       | `.github/workflows/ci.yml` (job `check`)       | PRs + push                         | Required           | `go test -race -cover ./...` (Layers 1, 2a, 2b, 3, 4, 5, 7, 8, 10 default)                                        |
| `lint`                        | `.github/workflows/ci.yml` (job `lint`)        | PRs + push                         | Required           | `golangci-lint` v2 + markdownlint + commitlint                                                                    |
| `forbid-skip`                 | `.github/workflows/ci.yml` (job `forbid-skip`) | PRs + push                         | Required           | `t.Skip*` + discipline-erosion wording + soft-assertion + skip-additions                                          |
| `probe`                       | `.github/workflows/chdb.yml` (job `probe`)     | PRs + push                         | Required           | chDB driver sanity (`TestChDBProbe`)                                                                              |
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
set to `expected_rows`. Use `just update-golden` and `just spec-chdb`
locally.

### Layer 7 — HTTP handler conformance

`internal/api/{prom,loki,tempo}/conformance_test.go` exercises every
documented HTTP endpoint with representative payloads and asserts the
wire envelope shape (`status`, `data.resultType`, content keys),
response headers, error envelope, and admission-control 503 + `Retry-After`.

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

## Regression meta-tests

`test/regression/` pins past CI failures so they can't silently
recur:

- `goleak_test.go` — process-level goroutine leak detection across
  every handler entrypoint.
- `justfile_test.go` — recipe-shape pins so a `just` syntax change
  doesn't silently break CI.
- `seed_test.go` — the e2e seed program's row-count and metric-name
  invariants.
