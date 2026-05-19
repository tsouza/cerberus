# Pre-GA Code-Health Audit

> Diagnostic snapshot taken at `71d28e6` on 2026-05-15.
> Read-only audit — nothing was fixed here. Findings inform GA-readiness
> decisions; turning any of them into work items is a separate PR.

## TL;DR

Cerberus is in healthy shape going into GA:

- **Lint baseline is clean.** `golangci-lint run` (with the project's
  full v2 config) and `go vet ./...` both report `0 issues`.
- **No `unsafe.Pointer` or `reflect.FieldByName` regressions** anywhere
  outside the forks — `forbidigo` is doing its job and no Go source file
  imports `"unsafe"`.
- **Coverage floor is `internal/chclient` at 47.6 %** — driven by
  uncovered HTTP-shaped `Query*` accessors that need a live CH integration.
  Everything else lives above 50 %, most packages above 80 %.
- **Zero `unused` findings.** No dead exported symbols flagged by
  `staticcheck`/`unused`.
- **Three real TODO/FIXME markers**, all in `compatibility/prometheus/`
  YAML — narrative reminders, not code debt.
- **`CLAUDE.md` has narrow staleness pockets** (PR-in-flight phrasing,
  missing `compatibility/tempo/`, partial `docs/` listing) but
  the hard-rule and architecture sections all match reality.

## 1. Lint baseline

`golangci-lint run --timeout 5m ./...` against the in-repo `.golangci.yml`
(v2 schema; enabled linters: `errcheck`, `govet`, `staticcheck`,
`ineffassign`, `unused`, `gosec`, `revive`, `misspell`, `bodyclose`,
`errorlint`, `forbidigo`, `gocognit`, `cyclop`, `funlen`, `nestif`,
`dupl`, `maintidx`, `lll`):

```text
0 issues.
```

`go vet ./...`:

```text
0 issues.
```

Re-running individual linters at their tool defaults (lower than the
project's tuned thresholds) is included below as a "what would tighten
catch first?" signal — none of these are current violations.

## 2. TODO / FIXME / XXX / HACK inventory

Search: `grep -rEn "(TODO|FIXME|XXX|HACK)" --include=*.go --include=*.md
--include=*.yml --include=*.yaml .` (excluding `harness/*/upstream/`).

| File:line                                                     | Category           | Note                                                             |
| ------------------------------------------------------------- | ------------------ | ---------------------------------------------------------------- |
| `internal/schema/ddl/idempotency_test.go:290`                 | False positive     | `context.TODO()` API call, not a marker.                         |
| `internal/schema/ddl/idempotency_test.go:293`                 | False positive     | `context.TODO()` API call, not a marker.                         |
| `docs/compat-residual-audit-25898791664.md:522`               | Comment-only note  | Audit doc text referencing a now-resolved upstream TODO.         |
| `compatibility/prometheus/cerberus-test-queries.yml:41`  | Deferred behaviour | "Add tests for staleness support."                               |
| `compatibility/prometheus/cerberus-test-queries.yml:113` | Comment-only note  | "Check this more systematically for every node type?"            |
| `compatibility/prometheus/cerberus-test-queries.yml:128` | Deferred behaviour | "Add non-explicit many-to-one / one-to-many that errors."        |
| `compatibility/prometheus/cerberus-test-queries.yml:129` | Deferred behaviour | "Add many-to-many match that errors."                            |
| `.golangci.yml:14`                                            | Deferred behaviour | "tighten to 30 once `writeInto` is broken up by `emit_node.go`." |
| `.golangci.yml:19`                                            | Deferred behaviour | "tighten `max-complexity` to 25 once `writeInto` is decomposed." |
| `.golangci.yml:34`                                            | Deferred behaviour | "deduplicate the per-type fold helpers and tighten back to 200." |

**Categorisation:**

- **Real tech debt:** 0.
- **Deferred behaviour (planned follow-ups, all with rationale in place):** 5 — three in `.golangci.yml` reflecting roadmap work, two in the prom-compliance harness for missing test cases.
- **Comment-only notes:** 2.
- **False positives** (`context.TODO()` API calls counted by the regex): 2.

No `FIXME`, `XXX`, or `HACK` markers anywhere in the in-tree source or
docs (the search returned only `TODO`-shaped hits).

## 3. `go vet` + `unsafe` audit

`go vet ./...` returns 0 issues.

`grep -rE "unsafe\." --include=*.go .` (excluding `harness/*/upstream/`):

- **Zero imports of `"unsafe"`** in any Go file under `internal/`,
  `cmd/`, `test/`, `cerbtrace/`, `chclient/`, `chplan/`, `chsql/`,
  `config/`, `engine/`, `logql/`, `optimizer/`, `promql/`, `promshim/`,
  `qlcommon/`, `schema/`, `telemetry/`, or `traceql/`.
- One incidental hit in `internal/traceql/group_coalesce.go` — a **doc
  comment** explicitly stating the no-reflect / no-unsafe rule for the
  TraceQL lower path.

`forbidigo` (configured in `.golangci.yml`) bans `unsafe.Pointer` and
`reflect.Value.FieldByName` inside `internal/traceql/` and
`internal/api/tempo/`. The linter reports no violations, confirming
the upstream-fork accessor migration (`tsouza/tempo:cerberus-accessors`)
fully replaces the retired shims.

## 4. Test coverage shape

`go test -cover ./internal/...` (rebuild-friendly default profile;
race off because coverage doesn't need it):

| Package                   | Coverage   | Notes                                                                                                                                                                                                                                                                         |
| ------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/chclient`       | **47.6 %** | Uncovered: live-driver paths — `New`, `Conn`, `Close`, `QueryMetricMeta`, `QueryIndexStats`, `QueryIndexVolume`, `QueryLabelSets`, all of `progress.go`. These need a real CH or chDB-backed test; only `breaker` / `cursor` are exercised.                                   |
| `internal/engine`         | **51.6 %** | Uncovered: `QueryCursor`, `QueryPlanCursor` (cursor-shaped variants). The non-cursor `Query` + `QueryPlan` are at 100 %.                                                                                                                                                      |
| `internal/logql`          | **62.5 %** | Uncovered: `lowerVectorVector`, `vectorMatchingFromOpts`, `sampleShapeOverLogInner` (all in `binary.go`); `Name`, `Parse`, `IsMetricQuery`, `parseExprTraced` (in `lang.go`); `lowerVector` (literal). These represent the unfinished LogQL binary-op + parser-trace surface. |
| `internal/promshim/local` | **67.2 %** | `LabelValues`, `LabelNames`, plus histogram-shaped `H`, `FH`, `Copy` accessors — test-helper code mostly exercised indirectly via property tests under `test/property/`.                                                                                                      |
| `internal/chsql`          | **74.0 %** | The 26 % gap is mostly emit-path branches gated behind range-window edge cases; large file (`builder.go` 1 538 lines, `range_window.go` 1 796 lines) so percentage is misleading.                                                                                             |

Bottom-5 summary: chclient + engine each carry a real cluster of zero-
coverage `Query*` / cursor functions that need integration-grade tests;
logql carries a real unfinished-surface gap; promshim/local is mostly
helpers exercised indirectly; chsql's number is denominator-driven, not
absent-test-driven.

Five packages report **100 %** of statements covered (`api/format`,
`api/health`, `api/httperr`, `cerbtrace`, `qlcommon`).

## 5. Dead code

`golangci-lint run --no-config --default=none --enable=unused
--timeout 5m ./...`:

```text
0 issues.
```

No exported symbols flagged as unused.

`staticcheck` (part of the standard config) is also clean for the
`U1000` ("unused") family.

## 6. Cyclomatic / cognitive complexity hotspots

`gocyclo` is not installed locally; instead, ran the `cyclop` and
`gocognit` linters at **their tool defaults** (10 and 30 respectively),
which are tighter than the project's tuned thresholds (32 and 60 — see
`.golangci.yml` header comment). The findings below would all be
**current violations under default thresholds**, but **none are
current violations under the project's tuned thresholds**.

### Cyclomatic complexity (default cap = 10) — top 10 non-test internal/

| Function                            | Complexity | File                                                |
| ----------------------------------- | ---------- | --------------------------------------------------- |
| `(*HistogramQuantileNative).Equal`  | 21         | `internal/chplan/histogram_quantile_native.go:81`   |
| `(*RangeWindow).Equal`              | 18         | `internal/chplan/range_window.go:103`               |
| `(*MetricsAggregate).Equal`         | 18         | `internal/chplan/metrics_aggregate.go:130`          |
| `(*MetricsHistogramOverTime).Equal` | 16         | `internal/chplan/metrics_histogram_over_time.go:62` |
| `(*HistogramQuantile).Equal`        | 16         | `internal/chplan/histogram_quantile.go:63`          |
| `handleQueryExemplars`              | 15         | `internal/api/prom/exemplars.go:52`                 |
| `(*TopK).Equal`                     | 14         | `internal/chplan/topk.go:45`                        |
| `(*AbsentOverTime).Equal`           | 14         | `internal/chplan/absent_over_time.go:124`           |
| `(*VectorJoin).Equal`               | 13         | `internal/chplan/vector_join.go:122`                |
| `(*Handler).handleQueryRange`       | 13         | `internal/api/prom/handler.go:221`                  |

The cluster at the top is structural — `chplan.Node.Equal`
implementations are an unrolled equality across a fixed list of fields,
which the linter scores as one branch per field. They're already
covered by the project's tuned 32 max-complexity ceiling and they're
the cheapest possible shape to read. Not a real hotspot.

### Cognitive complexity (default cap = 30) — non-test internal/

| Function                    | Cognitive | File                                    |
| --------------------------- | --------- | --------------------------------------- |
| `(*QueryBuilder).writeInto` | 59        | `internal/chsql/builder.go:1435`        |
| `newLabelFormatStep`        | 32        | `internal/api/loki/post_process.go:184` |

`writeInto` is acknowledged in `.golangci.yml` (`TODO: tighten to 30
once writeInto is broken up by emit_node.go`). `newLabelFormatStep` is
the only **new** finding — a 60-line function chaining a compile loop +
a per-row closure with rename-vs-template branching. Below the project's
60 ceiling but worth a refactor pass when the LogQL post-processing
slice next gets touched.

### Function length (default cap = 60 lines / 40 stmts)

17 non-test functions exceed default funlen, all within the project's
tuned 150-line / 80-statement ceiling. The largest concentrations:

- `internal/chsql/range_window.go` — 119-line `emitRangeWindowMetrics`,
  85-line `emitWindowedArrayExtrapolated`, 81-line
  `emitWindowedArrayExtrapolatedMatrix`. Range-window emit is
  intrinsically big; these are the natural decomposition seams when
  the file next gets refactored.
- `internal/promql/lower.go` — 118-line `wrapRangeLatestPerSeries`,
  85-line `lowerCountValues`, 70-line `lowerRangeVectorCall`. Same
  story for the PromQL lower path.
- `internal/chsql/histogram_quantile.go` — 91-line
  `histogramQuantileValueFrag` (explicitly the function the project's
  funlen ceiling is sized to accommodate).

### Duplicate-code (`dupl`)

Two pairs of duplicates in `internal/optimizer/constant_fold.go`:

- Lines 124-164 ↔ 170-210 (~ 220 tokens). Already documented as
  acknowledged duplication in `.golangci.yml:34`.
- Lines 280-307 ↔ 309-336 (~ 70 tokens). Similar fold-by-type pattern;
  smaller and not currently called out.

Both are the "per-type fold helper" shape the linter config explicitly
allows; deduplicating is a roadmap item for the constant-fold refactor.

## 7. CLAUDE.md staleness vs current repo state

| Section                             | Claim                                                                                                                                  | Reality                                                                                                                                                                                                                                                                                                                                                                          | Status                     |
| ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------- |
| Hard rules → required CI            | "Required CI checks: `check` + `lint`."                                                                                                | `ci.yml` defines both jobs; `e2e.yml`'s `dashboard` runs only on main pushes + nightly + manual dispatch.                                                                                                                                                                                                                                                                        | **Current.**               |
| Hard rules → dashboard E2E          | "`dashboard` full-stack smoke ... lives as the `dashboard` job inside `.github/workflows/e2e.yml`."                                    | True. But `e2e.yml` also defines a sibling `startup-bench` job that CLAUDE.md doesn't mention.                                                                                                                                                                                                                                                                                   | **Mild gap.**              |
| Architecture map                    | "`compatibility/prometheus/`"                                                                                                     | True. **But:** `compatibility/tempo/` (vendored tempo-vulture + httpclient snapshot, landed PR #367) is NOT mentioned.                                                                                                                                                                                                                                                   | **Stale — missing entry.** |
| Architecture map (`test/spec/`)     | "[incoming via PR #265]"                                                                                                               | `test/spec/chplan_print.go` exists and is in active use. PR #265 has landed.                                                                                                                                                                                                                                                                                                     | **Stale phrasing.**        |
| Architecture map (`test/property/`) | "Phase 1 PR 1 in flight ... oracle/bridge.go temporary bridge to promshim/local (replaced by from-scratch evaluator in Phase 1 PR 2)." | Phase 1 PR 2 has **landed** — `test/property/oracle/promql/` (the from-scratch evaluator) exists and `bridge.go`'s own doc comment says "As of Phase 1 PR 2 the canonical oracle is the from-scratch PromQL evaluator".                                                                                                                                                          | **Stale phrasing.**        |
| Architecture map (`docs/`)          | "roadmap.md, optimizer-research.md, compatibility.md, engine.md, observability.md, 12factor.md, test-strategy.md, …"                   | Missing from the ellipsis: `chsql-audit.md`, `coverage-baseline.md`, `engine-evaluation.md`, `health.md`, `loki-compliance-plan.md`, `sql-builder-evaluation.md`, `tempo-compliance-plan.md`, `test-spec-audit.md`, `upstream-forks.md`. The ellipsis is correct ("…") but a few of these are linked from the same file (e.g. `sql-builder-evaluation.md`, `upstream-forks.md`). | **Mild gap.**              |
| Hard rules → typed chsql            | "Use `internal/chsql.Builder` / `chsql.QueryBuilder`."                                                                                 | Both types exist; `Builder.writeSQL` is unexported; no `chsql.Raw` / `chsql.Concat` in the public API.                                                                                                                                                                                                                                                                           | **Current.**               |
| Upstream forks → tempo accessors    | "~6 accessor methods"                                                                                                                  | The user's auto-memory says "11 accessors". Both `internal/traceql/group_coalesce.go` and `internal/traceql/aggregate.go` consume the fork-exposed `GroupOperation`/`CoalesceOperation` public fields. Exact count is not knowable from in-tree code without checking the fork.                                                                                                  | **Possibly stale count.**  |
| Toolchain → Go version              | "`go.mod` may pin a newer Go than what's installed system-wide."                                                                       | `go.mod` pins `go 1.26.2`; system has go1.26.2 and the toolchain machinery referenced.                                                                                                                                                                                                                                                                                           | **Current.**               |
| Toolchain → memberlist replace      | "`replace github.com/hashicorp/memberlist => github.com/grafana/memberlist v0.3.1-0.20260410131411-8c2f3bdae9db`"                      | Exact string match in current `go.mod`.                                                                                                                                                                                                                                                                                                                                          | **Current.**               |
| Forks → replace pins                | All four `replace` directives                                                                                                          | Exact string match.                                                                                                                                                                                                                                                                                                                                                              | **Current.**               |
| Project agreement                   | "the `Cerberus v1.0.0 Roadmap` GitHub Project" + "[Roadmap Project](https://github.com/users/tsouza/projects/1)"                       | Not validated here (would require `gh` API call). Out of scope for read-only file audit.                                                                                                                                                                                                                                                                                         | **Not verified.**          |

## Findings rolled up by priority

### Critical

None.

### High

None.

### Medium

1. **`internal/chclient` coverage at 47.6 %.** The uncovered surface
   (`New`, `Conn`, `Close`, `QueryMetricMeta`, `QueryIndexStats`,
   `QueryIndexVolume`, `QueryLabelSets`, `progress.go`) is the live-CH
   integration layer — a known weak spot for unit tests but the layer
   most likely to surprise in production. A small set of chDB-backed or
   testcontainers-backed integration tests would lift the floor cheaply.
2. **`internal/engine` coverage at 51.6 %.** Two zero-coverage
   functions (`QueryCursor`, `QueryPlanCursor`) — the cursor variants of
   the otherwise-fully-covered `Query` / `QueryPlan`. Adding a cursor-shape
   test fixture would close the gap.
3. **CLAUDE.md → `compatibility/tempo/` missing.** The
   architecture map should list it alongside `compatibility/prometheus/`
   so onboarding agents know where TraceQL harness work lives.
4. **CLAUDE.md → stale "in flight" / "incoming" phrasing for property
   tests + PR #265.** Both have landed; the phrasing implies otherwise.

### Low

1. **CLAUDE.md → missing `docs/` filenames from architecture map ellipsis.**
2. **CLAUDE.md → `startup-bench` job in `e2e.yml` not mentioned.**
3. **`newLabelFormatStep` (`internal/api/loki/post_process.go:184`)
   cognitive complexity 32.** Inside the 60 ceiling but the only
   non-`writeInto` function in the high-cognitive cluster. Natural
   refactor target when the LogQL post-process slice gets touched.
4. **Tempo accessor count inconsistency.** CLAUDE.md says "~6", user
   memory says 11. Reconcile against the fork's public surface.

### Not real findings (already acknowledged in tree)

- `writeInto` cognitive complexity 59 — documented in `.golangci.yml`.
- `histogramQuantileValueFrag` 91 lines — documented in
  `.golangci.yml`.
- `constant_fold.go` lines 124-164 ↔ 170-210 dupl — documented.
- `lll` 280 char ceiling for SQL string emission — documented.

## Methodology / repro

```sh
# Lint baseline
golangci-lint run --timeout 5m ./...
go vet ./...

# TODO/FIXME inventory
grep -rEn "(TODO|FIXME|XXX|HACK)" \
  --include="*.go" --include="*.md" \
  --include="*.yml" --include="*.yaml" . \
  | grep -vE "harness/[^/]+/upstream/"

# unsafe / reflect audit
grep -rEn "unsafe\." --include="*.go" .
grep -rEn "reflect\.Value\.FieldByName" --include="*.go" .

# Coverage shape
go test -cover ./internal/...
go test -coverprofile=cov.out ./internal/chclient/ ./internal/engine/ \
  ./internal/logql/ ./internal/promshim/local/
go tool cover -func=cov.out | grep '0.0%$'

# Dead-code
golangci-lint run --no-config --default=none --enable=unused \
  --timeout 5m ./...

# Complexity probe (default thresholds — tighter than project config)
golangci-lint run --no-config --default=none --enable=cyclop \
  --timeout 5m ./...
golangci-lint run --no-config --default=none --enable=gocognit \
  --timeout 5m ./...
golangci-lint run --no-config --default=none --enable=funlen \
  --timeout 5m ./...
golangci-lint run --no-config --default=none --enable=dupl \
  --timeout 5m ./...
```

Run dates: **2026-05-15**. Tool versions: `golangci-lint 2.12.2`,
`go 1.26.2`.
