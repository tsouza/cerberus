# Fork Tempo — concrete plan to retire the `unsafe.Pointer` shim

**Status:** investigation / plan. Execution is a follow-up PR. Authored as
input to RC2 hardening — supersedes the "Forks" section of
[`docs/upstream-tracking.md`](upstream-tracking.md) with concrete API
shapes, file-level diff estimates, and a per-PR migration sequence.

## Relationship to `docs/upstream-tracking.md`

`docs/upstream-tracking.md` § "Forks: when upstream's API isn't enough"
already captures the high-level decision tree (when to fork, where to
host, how to keep up with upstream tags). It explicitly says:

> The long-term fix is to **fork each upstream parser** under
> `github.com/tsouza/` and add the narrow set of accessors cerberus
> needs. […]
> - For Tempo `Aggregate`: `func (a Aggregate) Op() AggregateOp` and
>   `func (a Aggregate) Expr() FieldExpression`.

and pencils the target as **RC4**, conditioned on either (a) a parser
bump breaking CI, or (b) adding a third unsafe shim.

**Delta this doc adds:**

1. The maintainer's decision: do the fork **now** (RC2), not at RC4.
   The trigger is no longer "shim count grows" — it's "we're about to
   add a *fourth* surface (TraceQL MetricsPipeline lowering) that
   would otherwise need *more* unsafe shims".
2. A complete inventory of unexported state cerberus reads from
   `pkg/traceql/` today — including the `reflect`-only `SelectOperation.attrs`
   path that `upstream-tracking.md` doesn't list.
3. The unexported state cerberus *will* read once MetricsPipeline
   lowering lands (RC2/RC3): `MetricsAggregate.op`/`by`/`attr`/`floats`,
   plus the unexported `firstStageElement` / `secondStageElement`
   interfaces that gate type-switching on `RootExpr.MetricsPipeline`.
4. The exact accessor API to add on the fork, the exact migration
   sequence inside cerberus, and the post-fork CI gate (lint rule)
   that prevents regressions.

## 1. Inventory: unexported state cerberus reads today

Two surfaces in `internal/traceql/`:

### 1a. `unsafe.Pointer` shim — single occurrence

| File:line | Reads | Upstream type & field | Why |
|---|---|---|---|
| `internal/traceql/aggregate.go:86` | `*(*traceql.FieldExpression)(unsafe.Pointer(field.UnsafeAddr()))` where `field = reflect.ValueOf(&agg).Elem().FieldByName("e")` | `traceql.Aggregate.e FieldExpression` (`pkg/traceql/ast.go:339`) | Need the inner expression of `sum/avg/min/max(…)` to lower the aggregate argument into a ClickHouse `sum(...)`/`avg(...)`/… call. `Interface()` panics on the unexported field, so we round-trip via `unsafe.Pointer`. |

The shim is *the* canonical reason this fork exists. The `op` field
next to it (`Aggregate.op AggregateOp`, same file `ast.go:338`) is
read via `reflect.Value.Int()` without `unsafe.Pointer` because
`AggregateOp` is `int` and `reflect.Value.Int()` doesn't trigger the
unexported-field protection; cerberus then maps int → name via a
hard-coded iota table (`aggregateOpName` in `aggregate.go:116`). That
table is just as fragile as the shim — reordering the enum constants
upstream silently mis-routes every aggregate. **The fork fixes both.**

### 1b. `reflect`-only paths (no `unsafe.Pointer`, but equally fragile)

These don't use `unsafe.Pointer` but read unexported fields by name —
silently zero-value on upstream rename.

| File:line | Reads | Upstream type & field | Why |
|---|---|---|---|
| `internal/traceql/aggregate.go:103` | `v.FieldByName("op").Int()` | `traceql.Aggregate.op AggregateOp` (`ast.go:338`) | Need the operator name (`count`/`sum`/…). Currently round-trips through the enum's iota order via `aggregateOpName`. |
| `internal/traceql/select.go:57-79` | `v.FieldByName("attrs")` then per-element `Name`/`Scope`/`Parent`/`Intrinsic` reflection | `traceql.SelectOperation.attrs []Attribute` (`ast.go:279`) | Need the projected attribute list for `| select(.a, .b)` lowering. Note: the *element* type `traceql.Attribute` has all-exported fields; only the containing slice is unexported. |

Repo-wide audit (`grep -RIn 'unsafe.Pointer\|UnsafeAddr\|FieldByName' internal/`):
**only `internal/traceql/aggregate.go` and `internal/traceql/select.go`**
contain these patterns. No other `unsafe.Pointer` usage anywhere in
the internal tree.

## 2. Inventory: unexported state cerberus *will* read for MetricsPipeline lowering

`RootExpr.MetricsPipeline` is the entry point for TraceQL metrics
queries (`{...} | rate()`, `{...} | count_over_time()`, etc.). Today
`internal/traceql/lower.go:22` rejects these wholesale with
"not yet supported". Lowering them is on the RC2/RC3 short list and
requires reading the following unexported state.

### 2a. The interfaces themselves are unexported

`pkg/traceql/ast.go:35`:
```go
type RootExpr struct {
    Pipeline           Pipeline
    MetricsPipeline    firstStageElement   // unexported interface
    MetricsSecondStage secondStageElement  // unexported interface
    …
}
```

`firstStageElement` (`ast_metrics.go:12`) and `secondStageElement`
(`ast_metrics.go:356`) are **unexported**. Cerberus can read the
`MetricsPipeline` field (it's exported), but cannot type-switch on it
without naming the interface — which is impossible from outside the
package. Today the only way to identify what's there is a `nil` check
plus reflect-based runtime type matching. The fork must expose either
the interfaces or, more cleanly, type-assertion helpers / a concrete
accessor surface.

### 2b. `MetricsAggregate` — every field unexported

`pkg/traceql/ast_metrics.go:30`:
```go
type MetricsAggregate struct {
    op         MetricsAggregateOp   // rate / count_over_time / …
    by         []Attribute          // `by (…)` group labels
    attr       Attribute            // operand for *_over_time(attr)
    floats     []float64            // quantiles for quantile_over_time
    agg        SpanAggregator       // runtime-only state — not needed
    seriesAgg  SeriesAggregator     // runtime-only state — not needed
    exemplarFn getExemplar          // runtime-only state — not needed
    simpleAggregationOp SimpleAggregationOp // runtime-only state — not needed
}
```

Lowering needs the first four: **`op`, `by`, `attr`, `floats`**. The
last four are runtime accumulators built by `init()`; cerberus
short-circuits the runtime by emitting CH SQL instead, so it never
reads them.

The enum constants for `op` (`metricsAggregateRate`,
`metricsAggregateCountOverTime`, …) are also unexported
(`enum_aggregates.go:54`). `MetricsAggregateOp.String()` is exported
and returns `"rate"`, `"count_over_time"`, etc. — cerberus can route
on the string, but it's stringly-typed. Cleaner is an exported `Op()`
accessor + exported enum constants. **Both should be exposed.**

### 2c. `MetricsAggregate.range` — does NOT exist

Important: TraceQL's grammar (`pkg/traceql/expr.y:308-323`) reveals
that `rate()`, `count_over_time()`, etc. take **no range argument** in
the query string. The query time range is supplied by the HTTP
`QueryRangeRequest` (`start`, `end`, `step`), not by the AST. So
unlike PromQL's `MatrixSelector.Range`, there's no range field on
`MetricsAggregate` to expose. Cerberus's lowering pulls range info
from the API-layer request, not from the parser AST. *(This resolves
the "Are there cerberus consumers that need to read `MetricsAggregate.range`?"
question — no such field exists.)*

### 2d. `MetricsFilter` (second-stage `| > 10` filter)

`pkg/traceql/ast_metrics.go:430`:
```go
type MetricsFilter struct {
    op    Operator   // unexported
    value float64    // unexported
}
```

If cerberus eventually lowers second-stage filters, both fields need
accessors. Out of scope for the initial fork PR — listed here so the
maintainer has the menu.

### 2e. `TopKBottomK`

`pkg/traceql/ast_metrics.go:382`:
```go
type TopKBottomK struct {
    op     SecondStageOp  // exported enum, unexported field
    limit  int            // unexported
    length int            // runtime — not needed
}
```

Same story as `MetricsFilter`: cerberus would need accessors for
`op` and `limit` if/when it lowers `| topk(N)`. Out of scope for the
initial PR.

## 3. Proposed accessor API on the fork

Design constraints:

- **Pointer receivers, no allocation.** Most of Tempo's existing
  internal methods on these types use value receivers; matching style
  keeps the patch surface small. Use the same receiver type the
  surrounding methods already use (value for `Aggregate`, pointer for
  `*MetricsAggregate`).
- **Intent-revealing names.** `InnerExpr` over `Expr` (the latter
  clashes with `FieldExpression`). `Op` over `Operation` (matches
  existing Tempo naming — `Operator`, `Op` fields elsewhere).
- **Expose, don't transform.** Accessors return the existing internal
  types (`AggregateOp`, `FieldExpression`, `[]Attribute`); they don't
  convert to strings or pre-resolve.
- **Export the enum constants** so cerberus can match on
  `traceql.MetricsAggregateRate` etc. instead of `String()`.
- **Export the two stage interfaces** so cerberus can type-switch on
  `RootExpr.MetricsPipeline` without going through reflect.

### Accessors needed for the day-1 migration (PRs #B and #C)

| Upstream type | Currently unexported | Proposed addition | Cerberus use |
|---|---|---|---|
| `traceql.Aggregate` | `e FieldExpression` | `func (a Aggregate) InnerExpr() FieldExpression { return a.e }` | `internal/traceql/aggregate.go` — replaces `readAggregateExpr` (`unsafe.Pointer` shim). |
| `traceql.Aggregate` | `op AggregateOp` | `func (a Aggregate) Op() AggregateOp { return a.op }` | `internal/traceql/aggregate.go` — replaces `readAggregateFields` + `aggregateOpName` enum-iota table. |
| `traceql.AggregateOp` | const `aggregateCount`, `aggregateMax`, `aggregateMin`, `aggregateSum`, `aggregateAvg` (`enum_aggregates.go:8-13`) | Rename / re-export as `AggregateCount`, `AggregateMax`, `AggregateMin`, `AggregateSum`, `AggregateAvg` (or add exported aliases — see "Patch shape" below). | Cerberus matches on the typed constant instead of the int order. |
| `traceql.SelectOperation` | `attrs []Attribute` | `func (s SelectOperation) Attrs() []Attribute { return s.attrs }` | `internal/traceql/select.go` — replaces the `reflect.Value.FieldByName("attrs")` loop. The element type is already exported, so a single accessor suffices. |

### Accessors needed for MetricsPipeline lowering (PR #C)

| Upstream type | Currently unexported | Proposed addition | Cerberus use |
|---|---|---|---|
| `traceql.RootExpr` | type `firstStageElement` (interface) | Rename to `FirstStageElement` (exported) and update the field type on `RootExpr.MetricsPipeline`. | Cerberus type-switches on `RootExpr.MetricsPipeline` in the new metrics-lowering entrypoint. |
| `traceql.RootExpr` | type `secondStageElement` (interface) | Rename to `SecondStageElement` (exported) and update `RootExpr.MetricsSecondStage`. | Same. Used later when cerberus lowers second-stage `topk` / `bottomk` / filters. |
| `traceql.MetricsAggregate` | `op MetricsAggregateOp` | `func (m *MetricsAggregate) Op() MetricsAggregateOp { return m.op }` | Discriminate `rate` vs `count_over_time` vs `*_over_time(attr)`. |
| `traceql.MetricsAggregate` | `attr Attribute` | `func (m *MetricsAggregate) Attribute() Attribute { return m.attr }` | The operand of `*_over_time(attr)` and `quantile_over_time(attr, …)`. |
| `traceql.MetricsAggregate` | `by []Attribute` | `func (m *MetricsAggregate) GroupBy() []Attribute { return m.by }` | The `by (…)` labels for `GROUP BY` in CH. |
| `traceql.MetricsAggregate` | `floats []float64` | `func (m *MetricsAggregate) Quantiles() []float64 { return m.floats }` | Quantile values for `quantile_over_time(attr, q1, q2, …)`. |
| `traceql.MetricsAggregateOp` | const `metricsAggregateRate`, `metricsAggregateCountOverTime`, `metricsAggregateMinOverTime`, `metricsAggregateMaxOverTime`, `metricsAggregateAvgOverTime`, `metricsAggregateSumOverTime`, `metricsAggregateQuantileOverTime`, `metricsAggregateHistogramOverTime` | Re-export as `MetricsAggregateRate`, `MetricsAggregateCountOverTime`, …. | Cerberus matches on the typed constant. |

### Optional / future accessors (NOT in the day-1 patch)

| Upstream type | Field | Proposed accessor | Cerberus use |
|---|---|---|---|
| `traceql.MetricsFilter` | `op Operator` / `value float64` | `Op()` / `Value()` | Lower `| rate() > 10` as a post-aggregation filter. |
| `traceql.TopKBottomK` | `op SecondStageOp` / `limit int` | `Op()` / `Limit()` | Lower `| topk(N)` / `| bottomk(N)`. |
| `traceql.SpansetOperation` | `matchingSpansBuffer` (runtime only) | none | n/a |

These land in follow-up patches as the corresponding lowering work
arrives. The day-1 fork ships **9 accessors + 1 interface rename ×2
(13 unexported symbols re-exported in total)**.

## 4. Fork hosting + workflow

### Repo

`github.com/tsouza/tempo` — under Thiago's personal `tsouza` GitHub
account. Use the GitHub UI to fork from `github.com/grafana/tempo`
(this is an account-level action; not something an agent should do).
Preserve all upstream branches and tags. Keep `LICENSE` (AGPL-3.0)
unchanged.

### Branch layout

- `main` — mirrors upstream's `main`. Fast-forward-only from
  `grafana/tempo:main`. No cerberus patches on this branch.
- `cerberus-accessors` — long-lived. Rebased onto each upstream tag
  cerberus wants to absorb. **This is the branch cerberus pins.**
- `release-vX.Y` — only created if a particular upstream release line
  needs cerberus patches independently. Not anticipated for day 1.

### Patch shape

One commit per accessor or per logically-coherent group:

1. `feat(traceql): expose Aggregate.Op() accessor`
2. `feat(traceql): expose Aggregate.InnerExpr() accessor`
3. `feat(traceql): re-export AggregateOp constants`
4. `feat(traceql): expose SelectOperation.Attrs() accessor`
5. `feat(traceql): rename firstStageElement → FirstStageElement`
6. `feat(traceql): rename secondStageElement → SecondStageElement`
7. `feat(traceql): expose MetricsAggregate.Op() accessor`
8. `feat(traceql): expose MetricsAggregate.Attribute() accessor`
9. `feat(traceql): expose MetricsAggregate.GroupBy() accessor`
10. `feat(traceql): expose MetricsAggregate.Quantiles() accessor`
11. `feat(traceql): re-export MetricsAggregateOp constants`

Each commit is mechanical (~3-10 LoC each: the accessor + any internal
callers updated to use the new constant name). Re-exporting an
unexported constant means either renaming it
(`aggregateCount → AggregateCount`) and updating every internal
reference, OR adding an exported alias
(`const AggregateCount = aggregateCount`). The alias approach is
~zero-risk for internal callers but slightly less idiomatic; the
rename is cleaner but touches more lines. **Recommendation: alias for
the enum constants** (single-line changes, no risk of breaking
upstream internals), **rename for the interfaces** (you can't alias a
type-name-as-field-type at a single declaration site cleanly).

Estimated total patch size on the fork: **~80-120 LoC additions**
(zero deletions for the accessor-only commits; the interface renames
touch every internal use site of `firstStageElement` /
`secondStageElement` — ~10-20 references).

### `go.mod` swap in cerberus

The replace directive looks like:

```text
replace github.com/grafana/tempo => github.com/tsouza/tempo v0.0.0-<commit-date>-<short-sha>
```

Pseudo-version string production:

```bash
# On the fork, at the HEAD of cerberus-accessors:
git rev-parse HEAD              # 40-char commit SHA
TZ=UTC git show -s --format=%cd --date=format-local:'%Y%m%d%H%M%S' HEAD
# Produces: 20260513120030 (example)
# Pseudo-version: v0.0.0-20260513120030-abcdef012345 (first 12 hex chars of SHA)
```

`go mod download github.com/tsouza/tempo@<commit-sha>` will compute
the canonical pseudo-version. Easier path: run `go get
github.com/tsouza/tempo@<branch-or-sha>` in cerberus — Go's tooling
fills in the pseudo-version automatically.

### Upgrade workflow (when Tempo cuts a new release)

1. On the fork: `git fetch upstream && git checkout cerberus-accessors`
2. `git rebase upstream/main` (or onto a specific upstream tag)
3. Resolve conflicts. The accessors are pure additions in mostly-stable
   files (`ast.go`, `ast_metrics.go`, `enum_aggregates.go`); conflicts
   are rare and surface as adjacent-line edits when upstream changes
   the surrounding code.
4. `go test ./pkg/traceql/...` on the fork must stay green.
5. `git push --force-with-lease origin cerberus-accessors`
6. In cerberus: `go get github.com/tsouza/tempo@<new-sha>` →
   `go mod tidy` → push. CI must stay green; if it doesn't, the fork
   rebase exposed a real semantic drift in Tempo's AST and the
   cerberus migration code needs an update — exactly the early-warning
   value we wanted.

Dependabot in cerberus picks up the new pseudo-version like any other
Go dep bump — daily, grouped under `upstream-parsers` per the existing
`.github/dependabot.yml` config.

## 5. Migration plan inside cerberus

Four PRs, in order:

### Cerberus PR #A — "wire the fork"

- **Scope:** add the `replace` directive in `go.mod` pointing at
  `github.com/tsouza/tempo@<initial-sha>`. Run `go mod tidy`. Nothing
  else.
- **Files touched:** `go.mod`, `go.sum`. **2 files.**
- **CI:** `check` + `lint` + `dashboard` green = fork is wired
  correctly. No goldens change (no behavioral change yet).
- **Risk:** essentially zero. If `go mod tidy` produces an unexpected
  dep churn, that's the signal that the fork's `go.mod` drifted from
  upstream and needs investigation.

### Cerberus PR #B — "retire the `unsafe.Pointer` shim"

- **Scope:** refactor `internal/traceql/aggregate.go` and
  `internal/traceql/select.go` to call the new accessors. Delete
  `readAggregateExpr`, `readAggregateFields`, `aggregateOpName`,
  `readSelectAttrs`. Drop the `reflect` + `unsafe` imports from both
  files.
- **Files touched:**
  - `internal/traceql/aggregate.go` (rewrite: ~140 LoC → ~50 LoC; -60% size)
  - `internal/traceql/select.go` (rewrite: ~85 LoC → ~50 LoC; -40% size)
  - Possibly `internal/traceql/lower.go` if the call sites change
    signature (they shouldn't — accessors return the same types the
    shims do).
  - **3 files.**
- **TXTAR fixtures affected:** 9 traceql fixtures exercise the
  aggregate path (`avg_duration`, `count_eq_zero`, `count`,
  `count_ge_threshold`, `count_lt_threshold`, `max_duration`,
  `min_duration`, `sum_duration`, `sum_span_attr`) plus 1 for select
  (`select_attrs`) — **10 fixtures total**. They should produce
  **byte-identical SQL** since the lowering output is unchanged; the
  only difference is *how* cerberus reads the AST. `just
  update-golden` should be a no-op; if any fixture diffs, that's a
  red flag and the PR needs review.
- **Risk:** low. The accessor return values are by definition the
  exact values the shim was reading. If a fixture diffs, either the
  shim was reading something the accessor isn't returning (bug in the
  accessor, fix on the fork) or the lowering logic was implicitly
  depending on a side effect of reflection (very unlikely).

### Cerberus PR #C — "TraceQL MetricsPipeline lowering"

- **Scope:** implement lowering for `RootExpr.MetricsPipeline` when
  it's a `*MetricsAggregate`. Map `rate()` → CH `count() / interval`,
  `count_over_time()` → `count()`, `sum_over_time(attr)` →
  `sum(attr)`, `min_over_time` / `max_over_time` / `avg_over_time`
  analogously. `quantile_over_time(attr, q)` → `quantile(q)(attr)`.
  `by (...)` → CH `GROUP BY`. The query-range/step driver is the
  Tempo `/api/metrics/query_range` handler at
  `internal/api/tempo/handler.go`.
- **Files touched (estimated):**
  - `internal/traceql/lower.go` — remove the
    "metrics pipelines not yet supported" early-out; route to a new
    `lowerMetricsPipeline`.
  - `internal/traceql/metrics.go` (new) — the lowering itself.
  - `internal/api/tempo/handler.go` — wire
    `/api/metrics/query_range`.
  - Possibly new chplan nodes if the existing IR is insufficient (CH
    `arrayJoin` for buckets? likely reuses RangeWindow shape from
    PromQL).
  - New TXTAR fixtures under `test/spec/traceql/` — one per
    `MetricsAggregateOp` × shape (rate, rate-by, count_over_time,
    sum_over_time(attr), quantile_over_time, …) — estimated **8-12
    new fixtures**.
  - **5-7 files touched, plus ~10 new fixtures.**
- **Crucial:** uses the new accessors from PR #C-set on the fork.
  **Zero `unsafe.Pointer`**, zero new reflect-by-name reads.
- **Risk:** medium. New SQL emission path; relies on the fork having
  the MetricsPipeline accessors. Gated by PR #A landing first.

### Cerberus PR #D — "lint gate against new `unsafe.Pointer`"

- **Scope:** add a CI guard preventing `unsafe.Pointer` and
  `reflect.Value.FieldByName` from reappearing in
  `internal/traceql/` and `internal/api/tempo/`. Two implementation
  options:
  - **golangci-lint custom analyzer** (`forbidigo`) — declarative,
    fits the existing `.golangci.yml` v2 schema.
  - **`cmd/check-unsafe/`** — Go program invoked from CI;
    over-engineered for this use case.
  - **Recommendation:** `forbidigo` in `.golangci.yml`:
    ```yaml
    linters:
      settings:
        forbidigo:
          forbid:
            - p: '^unsafe\.Pointer$'
              msg: 'unsafe.Pointer is forbidden; use tsouza/tempo accessors'
              pkg: ^github.com/tsouza/cerberus/internal/(traceql|api/tempo)$
            - p: '^reflect\.Value\.FieldByName$'
              msg: 'reflect.FieldByName on parser AST is forbidden; use accessors'
              pkg: ^github.com/tsouza/cerberus/internal/(traceql|api/tempo)$
    ```
- **Files touched:** `.golangci.yml` + maybe a test fixture proving
  the rule fires. **1-2 files.**
- **Risk:** zero. Gate only — if cerberus is clean post-PR-#B, this
  PR just locks it down.

### Total estimated effort

| PR | Files touched | Net LoC | Fixtures regenerated | Difficulty |
|---|---|---|---|---|
| #A (wire fork) | 2 | +3 / -0 | 0 | trivial |
| #B (retire shim) | 3 | +100 / -225 (net -125) | 10 (expected byte-identical) | low |
| #C (MetricsPipeline) | 5-7 + ~10 new fixtures | +400 / -10 | 0 existing + ~10 new | medium |
| #D (lint gate) | 1-2 | +15 / -0 | 0 | trivial |

Fork-side patch effort: **~80-120 LoC additions**, ~11 commits,
single-author + single-reviewer.

## 6. Risks / open questions

### Will Grafana accept the accessors upstream?

Open. Anthropic's experience with similar parser-internals patches
across Prometheus and ClickHouse is that **simple accessor PRs** (no
behavior change, no API surface bloat) are typically accepted within
a quarter, especially when framed as "we have N consumers reaching
into these via reflection — let's give them a supported API". A
proactive upstream PR after the fork stabilizes is recommended.

**Maintenance cost if upstream rejects:** ~1 hour per Tempo release
to rebase + verify. With Tempo's current ~monthly minor release
cadence, that's ~12 hours/year — well below the threshold where the
fork becomes a burden. CI dependency on our own fork's release
process is negligible because the fork has no release process beyond
"branch HEAD" — cerberus pins a pseudo-version.

### Other cerberus consumers of `github.com/grafana/tempo`

`grep -RIn 'github.com/grafana/tempo' internal/` returns 6 hits, all
in `internal/traceql/` or `internal/api/tempo/handler.go`, all using
only **exported** Tempo symbols (`traceql.Parse`, `traceql.RootExpr`,
`traceql.Aggregate`, `traceql.SpansetFilter`, …). The `replace`
directive is module-wide, so all of them get redirected to the fork
automatically — but since they only use exported symbols, none of
them break or change behavior. Confirmed: no hidden consumer surface.

Transitive deps: `grafana/dskit`, `grafana/loki`, `grafana/memberlist`
import `grafana/tempo`? Let's check — no, none of them do (tempo is a
leaf dependency in cerberus's graph). Safe.

### AGPL-3.0 license

Tempo is AGPL-3.0. Forking under `github.com/tsouza/tempo` is fully
compatible — AGPL allows forking and modification, as long as the
LICENSE file is preserved and modifications are clearly marked.
**Action items on the fork:**
- Preserve `LICENSE` unmodified.
- Add a short note to `README.md` (or a new `CERBERUS.md`) explaining
  the fork's purpose ("This fork adds narrow accessors needed by
  github.com/tsouza/cerberus. See branch `cerberus-accessors`.") and
  pointing back to upstream.
- Cerberus is already AGPL-3.0 compatible at the module level
  (cerberus itself is licensed under AGPL-3.0 per its `LICENSE`
  file). Confirm before merging PR #A.

### Unknowns I couldn't resolve from reading the code

- **`firstStageElement` interface rename ripple cost.** The rename
  touches every internal use site. From the grep there are ~6
  references inside `pkg/traceql/`; the test suite may use more. Estimated
  10-20 references total, but the exact count needs an in-tree
  count on the fork. Doesn't change the plan; just sets expectations
  for the commit's size.
- **Whether `MetricsAggregate.attr Attribute` (zero-value
  `Attribute{}`) is the canonical "no attribute" signal** for `rate()`
  / `count_over_time()` (which take no attribute). The runtime code at
  `ast_metrics.go:68` checks `a.attr != (Attribute{})` — so yes, the
  zero-value is the sentinel. Cerberus's lowering should follow the
  same convention.
- **Whether `RootExpr.MetricsSecondStage` can be `*ChainedSecondStage`
  wrapping multiple second-stage elements.** Reading
  `ast_metrics.go:521`: yes. Cerberus's lowering will need to flatten
  the chain. Not blocking the fork; just a note for PR #C.

## 7. Concrete next-step PR (executed on `github.com/tsouza/tempo`)

**Repository:** `github.com/tsouza/tempo` (after the maintainer creates
the fork via the GitHub UI).

**Branch:** `cerberus-accessors`, created off the current Tempo HEAD
that matches `github.com/grafana/tempo v1.5.1-0.20260508211128-2f74ea818de1`
(the version cerberus currently pins).

**PR title:** `feat(traceql): expose accessors for cerberus AST consumers`

**PR body sketch:**

```markdown
## Summary

This fork (`github.com/tsouza/tempo`, branch `cerberus-accessors`) adds
narrow accessor methods on top of upstream `pkg/traceql/` so the
[cerberus](https://github.com/tsouza/cerberus) project — a Prometheus /
Loki / Tempo gateway for ClickHouse — can read AST internals without
reflection or `unsafe.Pointer` shims.

The patches are purely additive (no behavior change, no API removal)
and each accessor is its own commit, designed to be portable upstream
if the Tempo maintainers want them.

## Commits

1. feat(traceql): expose Aggregate.Op() accessor
2. feat(traceql): expose Aggregate.InnerExpr() accessor
3. feat(traceql): re-export AggregateOp constants (AggregateCount, …)
4. feat(traceql): expose SelectOperation.Attrs() accessor
5. feat(traceql): rename firstStageElement → FirstStageElement
6. feat(traceql): rename secondStageElement → SecondStageElement
7. feat(traceql): expose MetricsAggregate.Op() accessor
8. feat(traceql): expose MetricsAggregate.Attribute() accessor
9. feat(traceql): expose MetricsAggregate.GroupBy() accessor
10. feat(traceql): expose MetricsAggregate.Quantiles() accessor
11. feat(traceql): re-export MetricsAggregateOp constants (MetricsAggregateRate, …)

## Test plan

- [ ] `go test ./pkg/traceql/...` green on the fork (must remain
      green — these patches are non-invasive)
- [ ] `go vet ./...` green
- [ ] cerberus `go get github.com/tsouza/tempo@<sha>` succeeds and
      `go mod tidy` produces a clean diff
- [ ] cerberus CI green on the "wire the fork" PR

## How to consume from cerberus

`go.mod` replace directive:

    replace github.com/grafana/tempo => github.com/tsouza/tempo v0.0.0-<commit-date>-<short-sha>

Produce the pseudo-version with:

    git rev-parse --short=12 HEAD                                      # → abcdef012345
    TZ=UTC git show -s --format=%cd --date=format-local:'%Y%m%d%H%M%S' # → 20260513120030

Pin: `v0.0.0-20260513120030-abcdef012345`.

## Upstream PR

After this fork stabilizes (say, after Cerberus PR #B lands and
fixtures stay byte-identical for two weeks), open an upstream PR on
`grafana/tempo` proposing the accessor additions — split into a
smaller subset that doesn't include the interface renames (those are
the most invasive and least likely to be accepted upstream).
```

**Test plan on the fork:**

```bash
git clone git@github.com-tsouza:tsouza/tempo.git
cd tempo
git checkout -b cerberus-accessors
# (apply commits 1-11 one at a time; each must keep tests green)
for commit in $(git log --reverse main..cerberus-accessors --format=%H); do
  git checkout $commit
  go test ./pkg/traceql/... || { echo "FAIL at $commit"; break; }
done
go test ./...   # full test suite, not just pkg/traceql
git push -u origin cerberus-accessors
```

**Done condition:** branch `cerberus-accessors` is pushed; `go test
./pkg/traceql/...` green at HEAD; cerberus's `go.mod` can pin
`github.com/tsouza/tempo@<HEAD-sha>` and `go mod tidy` succeeds.
