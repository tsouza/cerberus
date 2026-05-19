# Test strategy

Cerberus is tested in 12 layers, ordered roughly cheapest-and-fastest
to slowest-and-most-realistic. Each layer pins a different class of
contract: AST shape, plan-IR invariants, optimizer behaviour, emitted
SQL bytes, semantic equivalence under chDB execution, HTTP wire
conformance, process lifecycle, differential agreement against
reference engines, browser UX, failure-mode resilience, performance
ceilings. Together the layers add up to ~1000+ tests across
`internal/`, `test/`, and `harness/`.

This document is the canonical map of what each layer covers, where
the tests live, which CI gates run them, and how to add a new test
inside each layer.

## At a glance

| Layer | Name                                | Status       | Lives in                                                                                                                                                                                         | Catches                                                                                                              | Misses                                                                      |
| ----- | ----------------------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| 1     | Parser smoke / AST-shape pinning    | PR #267 open | `internal/{promql,logql,traceql}/parser_*_test.go` (60 tests)                                                                                                                                    | Upstream parser renames a field, swaps an enum, changes a root-node type after a fork rebase                         | Semantic divergence below the AST surface                                   |
| 2a    | chplan IR snapshots in TXTAR        | PR #265 open | `test/spec/<head>/*.txtar` (`-- chplan --` section, 207 fixtures)                                                                                                                                | Lowering regressions that don't change emitted SQL bytes                                                             | Optimizer-introduced regressions (covered by `-- chplan_optimized --` pair) |
| 2b    | Lowering edge cases                 | planned      | `internal/{promql,logql,traceql}/lower_*_test.go`                                                                                                                                                | Edge inputs (NaN, empty matrix, scalar coercions) that don't appear in golden fixtures                               | Combinatoric blow-up — keep table-driven                                    |
| 3     | chplan IR invariants                | merged #247  | `internal/chplan/{equal,walk}_invariants_test.go` (125 tests)                                                                                                                                    | `Equal()` false-positives / negatives; `Walk` / `Children` ordering drift; pointer-identity                          | Lowering bugs — IR is generic                                               |
| 4     | Optimizer rule properties           | PR #266 open | `internal/optimizer/{rule_interaction,termination,decision_pins,regression_bank,property_extended}_test.go` (70 tests)                                                                           | Rule-pair commutation, non-termination, mis-rewrites, decision-pin regressions                                       | Cross-rule chDB row drift (covered by Layer 6 chDB property)                |
| 5     | chsql Frag + QueryBuilder goldens   | merged #248  | `internal/chsql/{frag_goldens,query_builder_invariants,emit_node_goldens}_test.go` (114 tests)                                                                                                   | Frag render shape, slot-ordering invariants, append/replace semantics, Build idempotency                             | SQL that compiles but executes incorrectly — covered by Layer 6             |
| 6a    | PromQL chDB roundtrip               | merged #256  | `test/spec/promql/*.txtar` (`-- seed --` / `-- expected_rows --`, 63 fixtures)                                                                                                                   | Optimizer/emitter rewrites that change the row set                                                                   | Behaviour outside the seeded corpus                                         |
| 6b    | LogQL chDB roundtrip                | merged #255  | `test/spec/logql/*.txtar` (39 fixtures)                                                                                                                                                          | Same as 6a for LogQL                                                                                                 | Same as 6a                                                                  |
| 6c    | TraceQL chDB roundtrip              | PR #263 open | `test/spec/traceql/*.txtar` (61 fixtures)                                                                                                                                                        | Same as 6a for TraceQL                                                                                               | Same as 6a                                                                  |
| 7     | HTTP handler conformance            | merged #250  | `internal/api/{prom,loki,tempo}/conformance_test.go` (~138 cases)                                                                                                                                | Wire-format drift, error envelope shape, header pins, range-param parsing, admission control                         | Real-network failure modes (covered by Layer 11) and UX flows (Layer 10)    |
| 8     | System / process lifecycle          | merged #249  | `internal/config/`, `internal/api/health/`, `cmd/cerberus/`, telemetry, schema/ddl (87 tests)                                                                                                    | Env-var contract, `/readyz` TTL coalescing, OTel telemetry attributes, signal-driven shutdown                        | Cross-process behaviour — Compose / k3d (Layer 10)                          |
| 9     | Differential shadow harness         | PR #262 open | `compatibility/prometheus/shadow/*_test.go` (116 new tests)                                                                                                                                      | Cerberus vs. reference engine drift on PromQL / LogQL / TraceQL corpora                                              | Implementation-defined corners that both sides handle the same              |
| 10    | Playwright UX flows                 | PR #261 open | `test/e2e/playwright/*.spec.ts` (~58 new tests across 4 specs)                                                                                                                                   | Grafana Explore / Logs / Trace panel request sequences against cerberus's three datasource APIs                      | Pure backend logic — Layers 1–8                                             |
| 11    | Chaos / failure-mode                | merged #253  | `internal/{chclient,api/{prom,loki,tempo,admit}}/chaos_test.go`, `test/regression/goleak_test.go` (70 tests, surfaced #259 `rowsCursor.Close` race; CH-disconnect circuit breaker added in #305) | CH-failure, mid-stream cursor faults, goroutine leaks, panic-mid-handler slot release, CH-disconnect circuit breaker | Long-tail platform-specific failures                                        |
| 12    | Perf benchmarks + alloc regressions | merged #251  | `internal/*/*_bench_test.go` (29 `BenchmarkXxx` + 11 `TestAllocs_Xxx`)                                                                                                                           | Allocation count regressions per pipeline stage; bounded-RSS streaming cursor                                        | Wall-clock perf regressions — left to `perf-benchmark.yml` benchstat        |

## CI gates

| Gate                            | Workflow                                      | Trigger                         | Required PR check?                        | Scope                                                                      |
| ------------------------------- | --------------------------------------------- | ------------------------------- | ----------------------------------------- | -------------------------------------------------------------------------- |
| `check`                         | `.github/workflows/ci.yml` (job `check`)      | PRs + push                      | Required                                  | `go test -race -cover ./...` (Layers 1, 2a, 2b, 3, 4, 5, 7, 8, 11 default) |
| `lint`                          | `.github/workflows/ci.yml` (job `lint`)       | PRs + push                      | Required                                  | `golangci-lint` v2 + markdownlint + commitlint                             |
| `dashboard` (E2E)               | `.github/workflows/e2e.yml` (job `dashboard`) | push-to-main + nightly + manual | Informational                             | k3d + cerberus + Grafana + Playwright (Layer 10)                           |
| `compatibility`                 | `.github/workflows/compatibility.yml`         | push-to-main + nightly + manual | Informational today; required at v1.0 cut | `prometheus/compliance` differential (PromQL truth-source)                 |
| `mutation` (`phase1`)           | `.github/workflows/mutation.yml` (matrix)     | push-to-main + nightly + manual | Informational                             | gremlins on `internal/chplan` @ 90% efficacy                               |
| `mutation` (`phase2`)           | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/chsql` @ 85% efficacy                                |
| `mutation` (`phase3-optimizer`) | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/optimizer` @ 85% efficacy                            |
| `mutation` (`phase4-promql`)    | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/promql` @ 85% efficacy                               |
| `mutation` (`phase4-logql`)     | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/logql` @ 85% efficacy                                |
| `mutation` (`phase4-traceql`)   | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/traceql` @ 85% efficacy                              |
| `mutation` (`phase5-qlcommon`)  | Same workflow, separate matrix entry          | push-to-main + nightly + manual | Informational                             | gremlins on `internal/qlcommon` @ 75% efficacy                             |
| `chdb`                          | `.github/workflows/chdb.yml`                  | nightly + manual                | Informational                             | TXTAR chDB roundtrip (Layer 6a/6b/6c) + handler tests under `-tags chdb`   |
| `property`                      | `.github/workflows/property.yml`              | push-to-main + nightly + manual | Informational                             | Oracle property tests under `./test/property/...` with rapid `N=500`       |
| `shadow-mode`                   | `.github/workflows/shadow-mode.yml`           | push-to-main + nightly + manual | Informational                             | Layer 9 differential corpora                                               |
| `fuzz`                          | `.github/workflows/fuzz.yml`                  | nightly + manual                | Informational                             | `FuzzParse` per QL head                                                    |
| `perf-benchmark`                | `.github/workflows/perf-benchmark.yml`        | weekly + manual                 | Informational                             | `go test -bench` sweep with benchstat regression detection                 |

The PR-required surface is small on purpose. `check` + `lint` keep the
fast feedback loop fast; everything below pollinates main on a slower
cadence. Promotion criteria for each informational gate are documented
in the relevant section below.

## Local execution

| Layer                 | Command                                                         | Notes                                                                                          |
| --------------------- | --------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| Default test pass     | `just test`                                                     | `go test -race -cover ./...` — Layers 1, 2a, 2b, 3, 4, 5, 7, 8, 11                             |
| chDB roundtrip        | `just spec-chdb`                                                | Requires `just chdb-install` once; runs `go test -tags chdb ./test/spec/...` (Layers 6a/6b/6c) |
| chDB handler tests    | `just test-chdb`                                                | `go test -tags chdb ./internal/chclienttest/... ./internal/api/...`                            |
| Property framework    | `go test -tags chdb ./test/property/...`                        | Oracle property test (`pgregory.net/rapid` shrinking) — requires libchdb                       |
| E2E (k3d + Grafana)   | `just e2e-up && just e2e-seed && just e2e-run && just e2e-down` | Full Playwright suite under `test/e2e/playwright/` runs via `just e2e-playwright`              |
| Compatibility harness | `just compatibility`                                            | `prometheus/compliance` Docker Compose harness                                                 |
| Mutation (whole-repo) | `just mutate`                                                   | Slow (minutes) — global `.gremlins.yaml` threshold; informational                              |
| Mutation (chDB lane)  | `just mutate-chdb`                                              | Tighter kill criterion via `-t chdb -i` against `internal/optimizer/` + `internal/chsql/`      |
| Fuzz one head         | `just fuzz QL=promql DURATION=60s`                              | Bounded fuzz run against `internal/<ql>/FuzzParse`                                             |
| Benchmarks            | `just bench`                                                    | `go test -bench=. -benchmem -benchtime=5x -run='^$' ./...`                                     |
| Startup benchmark     | `just startup-bench`                                            | Process-start → `/healthz` 200 latency                                                         |

The default lane is CGO-free. Anything that depends on chDB is
build-tagged `chdb` and skipped on `just test`; the chDB workflow
links libchdb explicitly via `just chdb-install`.

## Gremlins phased rollout

Gremlins v0.6.0 does not support per-package thresholds, so the
rollout uses a workflow matrix where each entry scopes
`gremlins unleash` to one package with its own `--threshold-efficacy`
flag. The global `.gremlins.yaml` value is the floor for the unscoped
whole-repo `just mutate`.

The bar per phase is set roughly 4-15 percentage points below the
observed nightly kill rate so the gate is meaningful — a real test
regression breaks the build — without flapping on legitimate
run-to-run variance. The "May 2026 raise" column captures the bump
landed in PR #378 which closed the gap between threshold and actuals.

| Phase | Package                                        | Target efficacy | May 2026 raise | Status                              |
| ----- | ---------------------------------------------- | --------------- | -------------- | ----------------------------------- |
| 1     | `internal/chplan`                              | 90%             | 80% -> 90%     | Rolled out, informational (PR #260) |
| 2     | `internal/chsql`                               | 85%             | 75% -> 85%     | Rolled out, informational (PR #268) |
| 3     | `internal/optimizer`                           | 85%             | 70% -> 85%     | Rolled out, informational           |
| 4     | `internal/{promql,logql,traceql}` + `lower.go` | 85%             | 65% -> 85%     | Rolled out, informational           |
| 5     | `internal/qlcommon` (label_replace template)   | 75%             | new            | Rolled out, informational           |

Promotion to a required PR gate is gated on all phases: once they
stay green for a week of nightly runs, `mutation` becomes a required
check and the `gremlins unleash` command lives in `ci.yml` instead
of `mutation.yml`.

### Raising the bar

The bar is set deliberately below the observed kill rate. If kill
rate creeps up over time, raise the bar by editing both the matrix
entry in `.github/workflows/mutation.yml` and the table above in
the same PR. The opposite direction — lowering the bar to make a
flaky run pass — is a smell: investigate the regression instead.
Setting the bar too tight breaks CI on the legitimate run-to-run
variance gremlins exhibits, since mutation order, test timeouts,
and the small fraction of TIMED_OUT mutants can shift kill rate by
1-2 percentage points across runs.

The chDB-tagged mutation lane (`just mutate-chdb`) is the
sharpest-blade kill criterion: a mutant that changes the emitted SQL
text but preserves the rendered row set is correctly NOT killed
(the optimizer property test + chDB roundtrip fixtures both pass on
semantically equivalent mutants).

## Oracle property testing

`test/property/` is the third tier in the cerberus test pyramid (the
first two are TXTAR goldens and the differential shadow harness).
Where the lower tiers pin specific inputs to specific outputs, the
property tier randomises BOTH the data and the query and asserts
cerberus agrees with an independent oracle on every iteration. Failure
shrinking (via `pgregory.net/rapid`) reduces the reproducer to a
one-series, one-point dataset and a two-token query.

Phase 1 has landed end-to-end — framework scaffolding (Phase 1 PR 1)
plus the from-scratch PromQL oracle (#272, Phase 1 PR 2). The package
is `chdb`-tagged end-to-end so the default CGO-free `just test` lane
stays green. As of #280, the `property` workflow is unskipped and runs
on push-to-main + nightly + manual dispatch.

The `property` workflow (`.github/workflows/property.yml`) runs with
`-rapid.checks=500` — five times wider than rapid's default 100. The
default 100 still applies for developers running
`go test -tags chdb ./test/property/...` locally; the nightly lane is
the wider sweep. Failures upload any rapid-shrunk reproducers from
`test/property/testdata/rapid/` as a workflow artifact, and the `-v`
test log prints the rapid seed so a failing run reproduces locally via
`go test -tags chdb -run TestPromQL_Property -rapid.seed=<N> ./test/property/...`.

The from-scratch oracle has already paid for itself: it surfaced the
"Path to GA" wave of PromQL correctness gaps (rate/increase/delta on
empty windows, label_replace, absent, quantile φ clamp, topk, date
funcs, ±Inf/NaN wire-format, etc. — see `docs/roadmap.md` § "Path to
GA"). Each gap landed with a pinned seed under
`test/property/testdata/rapid/` so the regression can't silently
resurface.

### Phases

- **Phase 1 PR 1 — framework scaffolding (shipped).** The oracle was a
  temporary bridge to Prometheus's own `promql.Engine` via
  `internal/promshim/local`. This was a sanity check on the framework
  infrastructure (generators, chDB session, comparator, shrinking
  driver). Lives at `test/property/{framework,chdb,gen,oracle,promql_test}.go`.
- **Phase 1 PR 2 — from-scratch oracle (shipped via #272).** Replaced
  `oracle/bridge.go` with an in-tree PromQL evaluator reading the same
  `MetricsModel`. The test is now a true differential property:
  cerberus must match an INDEPENDENT spec of PromQL semantics.
- **Phase 2 — LogQL oracle.** Same architecture against a from-scratch
  LogQL evaluator over the random log-stream generators.
- **Phase 3 — TraceQL oracle.** Same architecture against TraceQL.

Each phase reuses the framework wiring (rapid → dataset → chDB →
cerberus → oracle → comparator). The chDB session and the comparator
are language-agnostic; only the generator and the oracle change per QL.

### Files

```text
test/property/
  doc.go             — package overview + phase plan
  framework.go       — runner: rapid.Check loop, Dataset / Query / Outcome types
  chdb.go            — chdb-tagged session helpers (open, apply DDL, decode cell)
  chdb_stub.go       — !chdb tag: t.Skip so default CGO-free lane stays green
  gen/metrics.go     — random Dataset generator (DDL + in-memory mirror)
  gen/promql.go      — random PromQL query generator
  oracle/bridge.go   — temporary bridge to promshim/local (replaced in Phase 1 PR 2)
  promql_test.go     — TestPromQL_Property_Bridge — chdb-tagged entry point
```

The framework has no dependency on `test/spec/` internals — the chDB
session helpers are replicated in-package so the two suites can evolve
independently.

## How to add a test

The right layer depends on what bug class you're guarding against.
Pick the cheapest layer that reproduces the regression.

### TXTAR fixture (Layers 2a, 6a/6b/6c)

```sh
# Use the skill — it scaffolds the file with the right sections.
# (Invoked from Claude Code's slash command.)
/cerberus:add-fixture <ql> <name>
```

The skill creates `test/spec/<ql>/<name>.txtar` with the section
headers (`-- input --`, `-- sql --`, `-- chplan --`, optionally
`-- seed --` and `-- expected_rows --`). After the implementation
lands, run `just update-golden` to populate the expected sections.

To opt into the chDB roundtrip (Layer 6), add a `-- seed --` block
(CREATE TABLE + INSERTs) and a `-- expected_rows --` block (JSON array
of rows). The `chdb`-tagged runner under `test/spec/runner_chdb.go`
applies the seed to an ephemeral in-process chDB session, executes the
fixture's emitted SQL, and asserts row equality.

### Conformance test (Layer 7)

Add a function to the relevant `internal/api/<head>/conformance_test.go`
that hits the handler via `httptest.NewServer`, decodes the response
into a struct with the upstream-documented JSON tags (so field-order
drift is invisible), and asserts the contract. The existing
`stubQuerier` / `tailStubQuerier` helpers cover the test seam.

### Chaos / goleak (Layer 11)

For CH-failure / mid-stream behaviour: drop a test into
`internal/chclient/chaos_test.go` using the `fakeRows` /
`failingRows` driver fakes or the in-test TCP proxy. For
goroutine-leak coverage: add a test to `test/regression/goleak_test.go`
that opens a real `httptest.NewServer`, drives N requests through one
handler entrypoint, closes the server, and calls
`goleak.VerifyNone(t, goleakOpts()...)`.

### chplan IR invariant (Layer 3)

For a new Node or Expr type: add `Equal()` + `Walk` + `Children`
coverage to `internal/chplan/{equal,walk}_invariants_test.go`. Each
file is table-driven — the table grows; the test runner does not.

### Optimizer rule property (Layer 4)

For a new rule: add an entry to
`internal/optimizer/rule_interaction_test.go` to pin commutation with
existing rules, a termination case to `termination_test.go`, and a
chDB property case to `property_extended_test.go` (`chdb` tag — 10
random plans per rule).

### chsql Frag golden (Layer 5)

Add a row to the unified table in
`internal/chsql/frag_goldens_test.go` for the new Frag constructor.
For new `QueryBuilder` slot semantics, extend
`query_builder_invariants_test.go`.

### Property test (oracle)

Today: add to `test/property/promql_test.go`. The generators in
`test/property/gen/` produce datasets + queries; the oracle in
`test/property/oracle/` computes the expected result; the comparator
checks cerberus matches. Once Phase 1 PR 2 lands, replace the bridge
oracle with the from-scratch evaluator.

### Conformance with shadow differential (Layer 9)

Add a case to the corpus in `compatibility/prometheus/shadow/cmd/`.
Each case names a parsed query plus a deterministic seed; the
harness diffs cerberus's `VectorResult` against the reference
engine's via `shadow.Compare`. The `shadow-mode.yml` workflow runs
the suite nightly.

## Determinism + chDB quirks

Round-trip fixtures depend on row order being stable. The runner
does NOT sort the result set; authors must guarantee deterministic
row order via the seed's `INSERT` ordering plus an `ORDER BY` in the
emitted SQL.

chdb-go v1.11.0 has three quirks the runner papers over:

- `rows.Err()` returns `fmt.Errorf("empty row")` at end-of-iteration
  instead of `io.EOF`. `tolerantRowsErr` ignores that exact string.
- `rows.ColumnTypes()` panics on `Map(String, String)` columns. The
  runner sizes its scan slice by parsing the outer SELECT's
  projection list textually.
- The parquet driver panics on the MAP logical type during
  `rows.Scan`. The runner auto-rewrites any top-level projection
  aliased `Attributes` / `ResourceAttributes` / `ScopeAttributes` /
  `SpanAttributes` to `toJSONString(<expr>) AS <alias>` and decodes
  the resulting string back to a `map[string]string` for the
  `reflect.DeepEqual` against `expected_rows`.

See `internal/chclient/chdb_probe_test.go` for the original
investigation.

## Reading order if you're new

1. This file — what each layer covers, where it lives, how to add.
2. `test/spec/runner.go` + `runner_chdb.go` — the TXTAR runner.
3. `test/property/doc.go` — the property pyramid + phase plan.
4. `compatibility/prometheus/shadow/README.md` — the differential harness.
5. `internal/chplan/equal_invariants_test.go` — the canonical IR
   invariant pattern to mirror for a new Node / Expr type.
