# Test Audit — 2026-05-20

**Baseline commit**: [`ada7ccc`](https://github.com/tsouza/cerberus/commit/ada7ccc3d0ded2005df48727a4a4a987aa174ce8)
**Compat scores**: PromQL 100% / LogQL 100% / TraceQL 100%
**Auditor**: cron-pool + main thread + 3× parallel `Explore` agents
**Scope**: every testing layer (unit, TXTAR golden, chDB roundtrip, property,
regression, e2e, compatibility, mutation, shadow, conformance, forbid-skip,
probe) — looking for tests that pass without verifying behavior.

This document is the **baseline** for future audits. Diff a fresh audit against
it to surface drift.

## 1. Executive summary

The cerberus test suite is **broadly healthy**, with three actionable hotspots
that warrant follow-up PRs:

1. **`internal/logql/range_aggregation.go`** — 6–9 surviving mutants on
   window/anchor numeric coefficients indicate tests pin SQL *shape* but not
   *exact values*. Strengthening assertions here is the highest-leverage win.
2. **`compatibility/tempo/driver/differ.go:267`** — asymmetric "skip blanks"
   logic on `StartTimeUnixNano` could mask a real field-omission regression.
3. **`internal/api/loki/conformance_test.go:109`** — `query_range` empty case
   only asserts `resultType`, not actual content.

**Surprises** (counter to the surface-scan verdict):
- The Loki `should_skip` overlay is **24 entries**, not 13 — initial scan
  undercounted by missing the `exhaustive/unwrap-aggregations.yaml` upstream-
  pinned cluster (10 entries for `stddev_over_time` / `quantile_over_time` —
  upstream v2-engine limitations cerberus inherits).
- Property-test oracles are **clean** (no SUT imports, no degenerate
  generators) — a meaningful guarantee that property-test passes are real.
- `chplan` and `qlcommon` mutation phases sit at **100% efficacy** (0
  survivors). Those packages are belt-and-braces tested.

**Top-line metrics**:
| Layer | Status |
| --- | --- |
| 9 required PR checks (`check` / `lint` / `forbid-skip` / `probe` / 3× `roundtrip` / 3× `compatibility`) | All green at baseline commit |
| Mutation efficacy (7 phases) | 96.77% aggregate (2305 killed / 77 lived / 2382 total) |
| TXTAR fixture count | 585 (promql 249 + traceql 133 + logql 109 + chsql 62 + optimizer 23 + codegen 9) |
| `should_skip` overlay (Loki) | 24 entries — 18 KEEP, 4 STRENGTHEN-REASON, 0 REMOVE |
| `expected-failures.json` (Prom + Tempo) | 0 entries (empty) |
| Historical test-removal anti-patterns | 3 confirmed (#563/#429/#537), all subsequently unblocked |

## 2. Test landscape inventory

12 testing layers. Required PR gates marked **★**.

| # | Layer | Location | Invoked via | Trigger | Gate |
| --- | --- | --- | --- | --- | --- |
| 1 | Unit tests | `internal/`, `cmd/`, `compatibility/` (54 `_test.go` files) | `just test` → `go test -race ./...` | `ci.yml :: check` | ★ Required |
| 2 | TXTAR golden | `test/spec/{promql,logql,traceql,chsql,optimizer,codegen}/` (585 files) | embedded in `just test` (text-equality lane) | `ci.yml :: check` | ★ Required |
| 3 | chDB roundtrip | `test/spec/<ql>/roundtrip_chdb_test.go` + `internal/chclienttest/` (build-tag `chdb`) | `just spec-chdb` | `chdb.yml :: roundtrip` matrix | ★ Required |
| 4 | Probe | `internal/api/health/` + `TestChDBProbe` | `just test-chdb` | `chdb.yml :: probe` | ★ Required (TBD — task #89) |
| 5 | Property | `test/property/` (`pgregory.net/rapid` + from-scratch oracles) | `go test -tags chdb -rapid.checks=500 ./test/property/...` | `property.yml` | Informational (nightly + push-to-main) |
| 6 | Regression meta | `test/regression/{goleak,justfile,seed}_test.go` | embedded in `just test` | `ci.yml :: check` | ★ Required |
| 7 | e2e (k3d + Grafana + Playwright) | `test/e2e/` (+ `playwright/`, `k3s/`, `grafana/`, `seed/`) | `just e2e` (full lifecycle) | `e2e.yml :: dashboard` | Informational (push-to-main + nightly) |
| 8 | Compatibility harnesses (3 heads) | `compatibility/{prometheus,loki,tempo}/` (driver + corpus + docker-compose) | `just compat-{promql,logql,traceql}` | `compatibility.yml` (matrix) | ★ Required (on path match) |
| 9 | Mutation (7 phases) | `.gremlins.yaml` + per-package matrix | `just mutate` / per-phase matrix | `mutation.yml` (95% threshold per phase) | Informational (nightly + push-to-main) |
| 10 | Shadow-mode | `compatibility/prometheus/shadow/` | `just shadow-mode` | `shadow-mode.yml` | Informational (manual) |
| 11 | Conformance | `internal/api/{prom,loki,tempo}/conformance_test.go` | embedded in `just test` | `ci.yml :: check` | ★ Required (HTTP wire-format pinning) |
| 12 | forbid-skip | `.github/workflows/ci.yml:113-136` regex | grep at CI time | `ci.yml :: forbid-skip` | ★ Required |

**Spine of correctness**: layers 1+2+3+6+11+12 (`check` + `chdb roundtrip` +
`forbid-skip`) catch unit / golden / wire-shape / discipline regressions on
every PR. Layers 8+9+5+7 (compat + mutation + property + e2e) provide
broader / nightly / informational coverage.

## 3. Mutation-survivor analysis

7 gremlins phases, latest successful run = nightly 2026-05-20 (run IDs
omitted — see `mutation.yml` artifacts of run-id 26143128549).

| Phase | Total | Killed | Lived | Efficacy | Verdict |
| --- | --- | --- | --- | --- | --- |
| **chplan** | 266 | 266 | 0 | **100.00%** | perfect |
| chsql | 525 | 499 | 26 | 95.05% | strong (24 acceptable boundary mutants) |
| optimizer | 174 | 169 | 5 | 97.13% | strong (loop / boundary equivalents) |
| promql | 875 | 846 | 29 | 96.69% | 5–7 weak in `label_fns.go` float arithmetic |
| logql | 334 | 318 | 16 | 95.21% | **8–9 weak in `range_aggregation.go`** + 2 in `detected_level.go` |
| traceql | 123 | 122 | 1 | 99.19% | sole survivor is acceptable boundary |
| **qlcommon** | 85 | 85 | 0 | **100.00%** | perfect |
| **TOTAL** | **2382** | **2305** | **77** | **96.77%** | strong |

**Survivor classification**: 55–60 acceptable (71–78%) · 10–15 weak-assertion
(13–19%) · 2–3 missing-test (3–4%).

### 3.1 Top weak-assertion hotspots (PR-worthy)

| Rank | File:line | Mutation type | Suggested fix |
| --- | --- | --- | --- |
| **#1** | `internal/logql/range_aggregation.go:52, 61, 116, 256, 469, 513` | `ARITHMETIC_BASE` × 6 | Tests pin SQL shape; add value-range assertions on the emitted window/anchor coefficients (e.g., `step_ns`, `range_ns`, `offset_ns`). Likely targets: `TestEmit*Matrix*` + `TestLowerRangeAggregation*Shape*` |
| #2 | `internal/logql/detected_level.go:143:44, 143:46` | `ARITHMETIC_BASE` × 2 adjacent | Level extraction constant (mask/shift). Add a `level=info`/`level=warn`/`level=error`/`level=debug` matrix that exercises the exact boundary of the mask. |
| #3 | `internal/promql/label_fns.go:81:39, 83:79` | `ARITHMETIC_BASE` + `INVERT_NEGATIVES` × 4 | Float comparison/arithmetic in label-fn helpers. Tests pass on signed-magnitude inputs; add cases where the mutation would flip sign in a non-obvious way (`label_replace` with empty `\\1` capture, `label_join` with single-element separator). |

### 3.2 Acceptable survivors (no action)

Long tail of `CONDITIONALS_BOUNDARY` mutations on `len(x) > 0` ↔
`len(x) >= 0` patterns (mathematically equivalent for non-negative lengths),
loop-index `i++` ↔ `++i`, capacity `cap(x) + 0` arithmetic. ~55-60 survivors
fall in this bucket; killing them would require introducing tests that
assert on negative-length scenarios that can't happen at runtime.

## 4. Differ / canonicalisation audit

### 4.1 Prometheus shadow differ — CLEAN

`compatibility/prometheus/shadow/differ.go`. All canonicalisation steps are
symmetric across cerberus and reference Prom. `AbsEpsilon/RelEpsilon = 1e-9`
(tight). `labelKey()` sorts alphabetically. Timestamp comparison is exact.
NaN/Inf handled in `valuesClose()` with documented semantics.

### 4.2 Tempo differ — **1 HIGH concern**

`compatibility/tempo/driver/differ.go:267-279`:

```go
// StartTimeUnixNano: if either side is empty, skip the compare
if got.StartTimeUnixNano == "" || want.StartTimeUnixNano == "" {
    // skip
} else {
    // compare ...
}
```

**Concern (HIGH)**: blank-skipping is *asymmetric to absence*. If cerberus
ever regresses to omitting `StartTimeUnixNano` (e.g., a future refactor
breaks the field projection), this differ will silently accept the
divergence. Reference Tempo's output is the source of truth — if it has the
field, cerberus should too. The differ should fail if exactly one side has
the field present.

**Suggested fix**: replace the blank-skip with:
```go
if (got.StartTimeUnixNano == "") != (want.StartTimeUnixNano == "") {
    return diff{...} // asymmetric absence is a divergence
}
if got.StartTimeUnixNano != "" {
    // compare values
}
```

Other findings on this differ:
- `DurationMs` comparison uses integer equality with epsilon fallback. The
  fallback path is reachable when the two backends report durations in
  different precisions. **MEDIUM** — defensible but warrants a comment
  explaining when the fallback fires.
- Trace ID handling uses raw hex strings on both sides since PR #439. Clean.

### 4.3 Loki compliance tester — **1 HIGH concern (mitigated)**

`compatibility/loki/cmd/loki-compliance-tester/main.go:469-480`:

```go
if isEmpty(baseline) {
    return &caseResult{
        TestCase: tc,
        UnexpectedFailure: "baseline returned empty",
    }
}
```

**Concern (HIGH-mitigated)**: this bypass fails the case if the baseline is
empty. PR #583 added an `empty-result` corpus-tag fast-path that flips this
to a parity check — but only for cases the corpus tags. Cases that legitimately
return empty without the tag still hit this bypass and surface as
`baseline returned empty`.

**Status**: **acceptable** — the bypass is documented (`compareOne` comment
references upstream `loki-bench`'s `assertResultNotEmpty` convention). The
fail-not-skip behavior is correct (it surfaces the case for triage, not
silently passes).

Other findings:
- `tolerance = 1e-5` (10,000× looser than Prom/Tempo's `1e-9`). **MEDIUM** —
  matches upstream `remote_test.go`, but no comment explaining why Loki
  warrants looser tolerance.
- `normaliseTypedResult()` sorts streams/vector/matrix by `(labelsCmp,
  timestamp)`. Applied symmetrically. Clean.
- `diffStreams()` does byte-for-byte line match (no tolerance). Correct.

## 5. Property-oracle audit — CLEAN

Property tests are nightly-only, but their integrity matters for what they
gate:

**Oracle imports** — none of `test/property/oracle/{promql,logql,traceql}/*.go`
import `internal/promql`, `internal/logql`, `internal/traceql`,
`internal/chplan`, or `internal/chsql`. The PromQL oracle bridge does
import `internal/promshim/local`, but that's the Prometheus engine wrapper,
not cerberus's lowering path. **Verdict: no self-shadowing tautology.**

**Generators** — spot-checked 5 generators in `test/property/gen/*.go`. No
`rapid.IntRange(0, 0)`, no `rapid.Float64Range(0.0, 0.0)`, no single-element
`OneOf`. Search space is real.

**Framework comparator** (`test/property/framework.go`) — handles NaN/Inf
symmetrically, no "if oracle empty, pass" early-exit, no panic recovery
that silently passes.

## 6. Conformance audit

### 6.1 Prom conformance — CLEAN
`internal/api/prom/conformance_test.go`. Tests assert status + Content-Type
header + envelope (`resultType`, `data.result`). Content validation depth
is acceptable — sample-pair length, value stringification, label-set keys
all checked.

### 6.2 Loki conformance — **1 HIGH concern**

`internal/api/loki/conformance_test.go:109-156` —
`TestConformance_LokiQueryRangeWire`:

The empty test case (`wantStreams == 0`) only asserts `resultType` matches
"streams"; it does **not** assert that the streams array is actually empty.
A regression where cerberus returns a partially-populated streams array
would still pass this test.

**Suggested fix**: extend the empty case to assert `len(streams) == 0`
explicitly.

Other findings:
- `TestConformance_LokiQueryWire:32-107` — `wantStreams > 0` check only
  triggers if non-zero. Empty case validates decoding-success but not the
  array's cardinality. **MEDIUM** — same pattern as the HIGH but smaller
  blast radius.

### 6.3 Tempo conformance — **2 MEDIUM concerns**

`TestConformance_TempoSearchWire:83-130` and
`TestConformance_TempoSearchRecentWire:132-152` — both accept `Traces ==
nil`. The wire contract specifies `Traces: []` (empty JSON array). A
regression where cerberus returns `null` instead of `[]` would pass.

**Suggested fix**: assert `Traces != nil` even for empty cases, and
distinguish `nil` from `[]` in the JSON decoder.

`TestConformance_TempoEchoWire:41-59` is clean (asserts body == "echo").

## 7. should_skip overlay codification

24 entries in `compatibility/loki/cerberus-test-queries.yml`. 0 in Prom /
Tempo overlays.

**KEEP (18)** — 14 upstream-pinned (Loki v2 engine lacks
`stddev_over_time`, `quantile_over_time`, numeric label filters on specific
shapes; dataobj-engine schema gap) + 4 defensible seed-gap entries (missing
labels: `pod`, `namespace`, `service_name`, `container`, `cluster`).

**STRENGTHEN-REASON (4)** — entries #3, #6, #7, #11 all reference
`detected_level` structured metadata that the seeder doesn't emit. Reasons
are vague ("seeder writes none"). Should clarify whether this is a
seeder-roadmap item (Phase 2) or a deliberate scope limit.

**REMOVE / NOW-INVALID (0)** — no entries are stale at the 100/100/100
baseline.

**NEEDS-LINK (0)** — every entry cites `docs/loki-compliance-plan.md PR 6`
or an upstream-skip marker.

### Upstream-pinned skips (vendored, not in cerberus overlay)

Loki-bench's vendored corpus carries 14 `skip: true` entries that cerberus
inherits transparently:
- `fast/structured-metadata.yaml` (1) — dataobj-engine schema gap
- `regression/structured-metadata.yaml` (2) — numeric label filters
- `regression/metric-queries.yaml` (1) — `JSONParserErr`
- `regression/drilldown-patterns.yaml` (1) — `SampleExtractionErr` — **not
  mirrored in cerberus overlay**; appears recent and likely harmless
- `exhaustive/unwrap-aggregations.yaml` (9) — `stddev_over_time` /
  `quantile_over_time` unsupported by v2 engine

## 8. Historical PR anti-patterns

Confirmed 3 anti-patterns from PR-history sweep (#100–#583). All three were
subsequently unblocked.

| PR | What was removed | Why wrong | Unblocked by |
| --- | --- | --- | --- |
| **#563** | 3 TXTAR cases (`tag_values_v2_*`) from Tempo corpus | Reference Tempo's parser rejection is a query-scoping issue, not a cerberus bug | #564 (revert) + #566 (proper fix: scope fixtures to `.service.name` form) |
| **#429** | 8 `should_skip` entries in Loki overlay for `count_over_time` + `detected_level` | Underlying lowering features needed implementation, not skipping | #450 (LogQL unwrap + range agg stack) → #568 (stale-skip cleanup) |
| **#537** | 9 `should_skip` entries for cardinality-rejected cases | Test infrastructure tunable; harness Loki config could raise `max_query_series` | #569 (raise `max_query_series: 10000`, re-enable all 9) |

No new anti-patterns found in #100-#583. The cluster of 3 is contained;
follow-up unblock-PRs landed within 2-40 commits.

## 9. Recommendations / follow-up PRs

Sorted by leverage. Each row maps to a separate PR. ★ = mechanical /
always-on regardless of findings.

| # | PR | What lands | Leverage |
| --- | --- | --- | --- |
| **1** | **★ PR-α** — `should_skip` CI guard | New `ci.yml` step that fails on any PR adding `should_skip:` to `cerberus-test-queries.yml` without a non-empty `reason:` AND a `jira:` URL or inline GH-issue link | Prevents repeat of #429/#537 |
| **2** | **★ PR-β** — broaden `forbid-skip` regex | Extend `ci.yml :: forbid-skip` to reject `assert.Contains(x, "")`, `defer recover()`, empty-slice `ElementsMatch`. Test against codebase first (should produce 0 hits today) | Discipline pin against fake-pass patterns |
| **3** | PR-γ — `logql/range_aggregation.go` mutation kills | Add value-range assertions for window/anchor coefficients; kill 6 ARITHMETIC_BASE survivors | Strongest weak-test hotspot in audit |
| **4** | PR-δ — Tempo differ `StartTimeUnixNano` blank-skip | Replace asymmetric blank-skip with "fail on asymmetric absence" | Closes the only HIGH-severity differ concern |
| **5** | PR-ε — Loki conformance empty `query_range` | Strengthen `wantStreams == 0` case to assert `len(streams) == 0` explicitly | Closes the HIGH-severity conformance concern |
| **6** | PR-ζ — Tempo conformance nil-vs-empty | Strengthen `TestConformance_TempoSearch*Wire` to require `Traces != nil` for empty cases | Closes 2 MEDIUM conformance concerns |
| **7** | PR-η — `logql/detected_level.go:143` mutation kills | Add level-boundary matrix exercising the mask/shift constants | 2 ARITHMETIC_BASE survivors |
| **8** | PR-θ — `promql/label_fns.go` mutation kills | Add cases for `label_replace` with empty capture / `label_join` with single-element separator | 4 float-arith survivors |
| **9** | PR-ι — STRENGTHEN-REASON sweep | Clarify the 4 `detected_level` overlay entries' reasons (Phase 2 seeder vs deliberate scope) | Hygiene; preempts future audit nit |

Total: 9 follow-up PRs. Mechanical PRs (1, 2, 4, 5, 6, 9) can land same-day.
Mutation-kill PRs (3, 7, 8) take longer (need test design that genuinely
distinguishes the mutated value).

## 10. Audit metadata

- **Date**: 2026-05-20
- **Baseline commit**: `ada7ccc3d0ded2005df48727a4a4a987aa174ce8`
- **Compat scores at baseline**: prom 100% / loki 100% / tempo 100%
- **Mutation run ID** (analysis source): 26143128549 (nightly @ 05:22 UTC)
- **Workflows green at baseline**: `ci` / `lint` / `forbid-skip` / `probe` /
  `roundtrip (promql,logql,traceql)` / `compatibility/{prometheus,loki,tempo}` /
  `chdb` / `property` / `mutation` / `coverage` / `e2e` / `shadow-mode`
- **Auditor**: cron-pool sweep cycle + main thread + 3 parallel `Explore`
  agents (mutation analysis; differ + oracle + conformance; should_skip
  codification)
- **Output**: this document (baseline) + 9 follow-up PRs (separate)
- **Re-audit cadence**: run again before v1.0.0 GA tag; diff against this
  doc; the diff is what shifted.

## Related docs

- [`docs/test-strategy.md`](test-strategy.md) — canonical 12-layer test map
- [`docs/pre-ga-readiness-signoff.md`](pre-ga-readiness-signoff.md) — prior
  pre-GA readiness pass
- [`docs/pre-ga-coverage-audit.md`](pre-ga-coverage-audit.md) — line-coverage
  baseline
- [`docs/pre-ga-code-health-audit.md`](pre-ga-code-health-audit.md) — code-
  health audit (handler form-parsing, cursor-Close errors, etc.)
- [`docs/loki-compliance-plan.md`](loki-compliance-plan.md) — the Loki
  overlay's reason-of-record
- [`docs/upstream-forks.md`](upstream-forks.md) — fork-pin policy (Tempo
  accessors, OTel-CH DDL, Loki/Prom Dependabot boundary)
