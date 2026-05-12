# RC2 P0 #4 — PromQL subqueries

Plan produced by the `Plan` agent at the start of RC2 P0 work. Breaks
`m[1h:5m]` / `rate(m[5m])[1h:5m]` / `max_over_time(rate(m[5m])[1h:5m])`
support into shippable PR-sized tasks. Each 4.n is independently
mergeable in order.

## Out of scope for P0 #4 (push to RC3 or P1)

- **Nested subqueries** (`rate(rate(m[5m])[1h:5m])[2h:10m]`) — 2+ levels.
- **Subquery `@` modifier + `@ start()`/`@ end()`** — same milestone as instant `@ start/end`.
- **Empty-step resolution from outer eval step** — hardcode 1m (Prom's default); thread real step in M2.1.
- **Recursive-form subqueries via recording rules** — orthogonal.
- **Aggregator inside subquery** (`max_over_time(sum by (job) (rate(...))[1h:5m])`) — 4.8 lands a deferral, real fix in RC3.

## Tasks

### 4.1 — Failing TXTAR fixtures for the subquery surface

**Scope.** Pure fixture-add PR; no code. Use `/cerberus:add-fixture`. Adds:

- `subquery_bare_vector.txtar` — `up[5m:1m]`
- `subquery_bare_vector_with_label.txtar` — `up{job="api"}[5m:1m]`
- `subquery_bare_default_step.txtar` — `up[5m:]`
- `subquery_offset.txtar` — `up[5m:1m] offset 10m`
- `subquery_over_rate.txtar` — `rate(http_requests_total[5m])[1h:5m]`
- `subquery_max_over_time_rate.txtar` — `max_over_time(rate(http_requests_total[5m])[1h:5m])` ← canonical Grafana shape
- `subquery_sum_over_time_rate.txtar` — `sum_over_time(rate(http_requests_total[5m])[1h:1m])`
- `subquery_avg_over_time_increase.txtar` — `avg_over_time(increase(http_requests_total[5m])[30m:5m])`
- `subquery_aggregated_outer.txtar` — `max_over_time(sum by (job) (rate(http_requests_total[5m]))[1h:5m])`

**Test plan.** Existing `TestLower` walks fixture dir; expect every new fixture to fail with `unsupported expression *parser.SubqueryExpr` until 4.5–4.7 ship.

**Dependencies.** None.

**Open questions.** Default-step semantics — hardcode 1m + document in fixture comment, schedule real step threading for M2.1.

### 4.2 — Extend `RangeWindow` IR for step-grid emission

**Scope.** `internal/chplan/range_window.go`. Adds `RangeWindow.OuterRange time.Duration` — when non-zero, emission produces one row per anchor between `End - OuterRange` and `End` (inclusive) spaced by `Step`. Zero = today's instant case (single anchor at End). Equal() compares the new field.

**Test plan.** `internal/chplan/node_test.go` Equal cases for the new field. No SQL changes.

**Dependencies.** 4.1.

**Open questions.** Field name — `OuterRange` vs `MatrixRange` vs `AnchorRange`. Prefer `OuterRange`. Anchors list pre-computed at lower-time vs derived at emit-time — prefer derived.

### 4.3 — Step-grid emission in `emit_range_window.go`

**Scope.** Generalise `emitWindowedArrayCb` to emit one row per anchor when `OuterRange > 0 && Step > 0`. Approach:

```
SELECT series_key, anchor_ts, <valueExpr>
FROM (
  SELECT series_key, anchor_ts,
    arrayMap(p -> tupleElement(p, 2), window_pairs) AS window_vals
  FROM (
    SELECT series_key, anchor_ts,
      arrayFilter(
        p -> tupleElement(p, 1) >= anchor_ts - toIntervalNanosecond(<range_ns>)
          AND tupleElement(p, 1) <= anchor_ts,
        series_array
      ) AS window_pairs
    FROM (
      SELECT series_key,
        arraySort(groupArray((TimeUnix, Value))) AS series_array,
        arrayJoin(arrayMap(
          i -> <end> - toIntervalNanosecond(i * <step_ns>),
          range(0, <num_anchors>)
        )) AS anchor_ts
      FROM (<input>)
      GROUP BY series_key
    )
  )
)
```

`<num_anchors> = OuterRange/Step + 1` (end-inclusive). `Offset` subtracts from each anchor expression.

**Test plan.** Direct chsql unit tests in `emit_test.go` that build matrix-shape RangeWindows by hand. New chsql-side TXTAR `test/spec/chsql/range_window_matrix_rate.txtar` + `range_window_matrix_over_time.txtar`. Existing `rate_*.txtar` MUST remain bit-identical (instant path).

**Dependencies.** 4.2.

**Open questions.** `arrayJoin` placement (sanity-check CH semantics); `OuterRange % Step != 0` rounding (Prom's actual: end-inclusive, so `[5m:2m]` = 3 anchors).

### 4.4 — Wire subquery output through `chclient.Sample`

**Scope.** Matrix-shape RangeWindow's outer SELECT must surface anchor timestamp as `TimeUnix`. Concretely:

- Inner: project the four sample columns directly when `OuterRange > 0`: `SELECT '' AS MetricName, <Attributes> AS Attributes, anchor_ts AS TimeUnix, <valueExpr> AS Value FROM (...)`.
- Handler `wrapWithSampleProjection` (`internal/api/prom/handler.go`) already handles derived shapes via `isDerivedShape` — extend to detect a matrix-shape RangeWindow and skip the wrap (or wrap with `TimeUnix = anchor_ts` ref instead of `now64(9)`).

Instant case (`OuterRange == 0`) keeps today's shape — handler wraps with `now64(9)` synthesis.

**Test plan.** New unit tests in `emit_test.go` directly emitting matrix-shape. Existing `rate_*.txtar` unchanged.

**Dependencies.** 4.3.

**Open questions.** None major. `Attributes` flows through from `GroupBy: [ColumnRef("Attributes")]` — verify.

### 4.5 — Lower bare `*parser.SubqueryExpr` over `VectorSelector`

**Scope.** Add `*parser.SubqueryExpr` to the top-level `lower` switch in `internal/promql/lower.go`. MVP: inner is a `*parser.VectorSelector`. Lowers to a `RangeWindow` with new sentinel `Identity bool` (cleaner than overloading `Func`) where:

- `Identity: true`
- `Range: e.Step` (or 1m fallback for empty step)
- `OuterRange: e.Range`
- `Step: e.Step`
- `Offset` / `End` from `anchorFromSelector(inner_vs)` + the subquery's own modifiers.
- `GroupBy: [Attributes]`.

`emit_range_window.go` learns the Identity case: emit `if(length(window_vals) > 0, window_vals[length(window_vals)], nan)` — last sample in window.

**Test plan.** `subquery_bare_*.txtar` and `subquery_offset.txtar` turn green. Run `GOLDEN_UPDATE=1 just test-spec`. New unit tests for nested-subquery rejection + subquery `@ start()`/`@ end()` rejection.

**Dependencies.** 4.2 + 4.3 + 4.4.

**Open questions.** Inner-step lookback semantics — Prom doesn't define this precisely. Substituting `Step` is pragmatic; if compatibility shows drift, change to 5m (Prom's lookback delta) regardless of Step. Flag for M6 compat check.

### 4.6 — Lower `*parser.SubqueryExpr` over range-vector calls

**Scope.** Extends 4.5: when SubqueryExpr's inner is a `*parser.Call` over `*parser.MatrixSelector` (`rate(m[5m])[1h:5m]`), lower to a single matrix-shape `RangeWindow` (no chaining yet). `Func` is the inner range-fn name; `Range` is the inner matrix range; `OuterRange`/`Step` from the subquery.

**Test plan.** `subquery_over_rate.txtar` turns green; add `subquery_increase.txtar` and `subquery_over_time_inside.txtar`.

**Dependencies.** 4.5.

**Open questions.** This task does NOT handle outer-Call-over-SubqueryExpr (`max_over_time(rate(m[5m])[1h:5m])`) — that's 4.7. Could merge 4.6+4.7 if 4.6 is < 100 LOC.

### 4.7 — Lower outer range-vector function over subquery

**Scope.** The canonical Grafana shape: outer `*parser.Call` whose arg is `*parser.SubqueryExpr`. Lowers to a **chained** RangeWindow:

```
RangeWindow{
  Func: "max_over_time", Range: 1h, Step: 0,    // outer reducer (instant over the inner matrix)
  Input: RangeWindow{                            // inner matrix from 4.6
    Func: "rate", Range: 5m, OuterRange: 1h, Step: 5m,
    Input: <Scan + Filter>,
  },
}
```

The outer RangeWindow's existing `arraySort(groupArray((TimeUnix, Value)))` reads naturally over the inner matrix's `(TimeUnix=anchor_ts, Value=rate)` output (from 4.4).

Lowering changes:
- `lowerCall` accepts `*parser.SubqueryExpr` as valid arg for range-vector functions.
- `lowerRangeVectorCall` adds a SubqueryExpr branch that recurses into `lowerSubquery` then wraps with outer RangeWindow.

**Test plan.** `subquery_max_over_time_rate.txtar`, `subquery_sum_over_time_rate.txtar`, `subquery_avg_over_time_increase.txtar` turn green.

**Dependencies.** 4.6.

**Open questions.** Verify the outer RangeWindow's `GroupBy: [ColumnRef("Attributes")]` correctly flows through the inner's output column scope.

### 4.8 — Lower subquery over aggregator output (defer with clear error)

**Scope.** `max_over_time(sum by (job) (rate(m[5m]))[1h:5m])` — SubqueryExpr's inner is `*parser.AggregateExpr`. The middle RangeWindow needs to evaluate the Aggregate at each anchor — but the inner Aggregate already projects `TimeUnix = now64(9)` (eval time), collapsing to a single anchor. **Real semantic problem.**

MVP: surface a clear lowering error: "subquery over aggregated expression: deferred to RC3 once chained RangeWindow over aggregate output is supported." `subquery_aggregated_outer.txtar` (from 4.1) captures the deferral message rather than green SQL.

**Test plan.** Update the fixture; add lower_test.go error case.

**Dependencies.** 4.7.

**Open questions.** Worth maintainer sign-off before merging the deferral. Alternative: implement for real — requires dropping the `now64(9)` projection at the Aggregate level and threading `arrayJoin(anchor_array)` through GROUP BY. Bigger refactor; recommend deferral.

### 4.9 — Optimizer awareness for subquery RangeWindow shapes

**Scope.** Verify existing optimizer rules don't mis-rewrite the new matrix-shape RangeWindow. `FilterFusion`, `ConstantFold`, `ProjectionPushdown` walk via `rewriteChildren`; RangeWindow is already in the switch. New `OuterRange` field is shallow-copied correctly by `cp := *v`. Confirm `ProjectionPushdown` doesn't push columns past a matrix-shape RangeWindow in a wrong way.

No new optimizer rules in P0 #4. RC3 adds subquery-aware rules.

**Test plan.** New TXTAR `test/spec/optimizer/subquery_projection_pushdown.txtar` pinning the no-mis-rewrite contract. Add unit test in `optimizer_test.go`.

**Dependencies.** 4.7.

**Open questions.** Highest-risk task for silent regressions. Add explicit "RangeWindow with OuterRange != 0 is opaque to current rules" assertion.

### 4.10 — Wire subqueries through Prom HTTP handler

**Scope.** `executeInstant` and `executeRange` should already work because `toVector` and `toMatrixStepGrid` are row-oriented, not series-row-counting. This task is verification + E2E:

- Add `test/e2e/e2e_prom_subquery_test.go` (or extend `e2e_prom_extra_test.go`):
  1. Seed CH with synthetic counter samples.
  2. Issue `max_over_time(rate(http_requests_total[5m])[1h:5m])`.
  3. Assert response is correctly shaped.
- Add Playwright spec for a Grafana dashboard with a subquery panel.

**Test plan.** Above.

**Dependencies.** 4.7.

**Open questions.** Instant subquery via `/api/v1/query` — Prom returns matrix, cerberus today would return vector via `toVector`. Punt — Grafana's instant panels rarely send subqueries. Match compat-suite outcome at M6.

### 4.11 — Roadmap + CLAUDE.md update

**Scope.** Replace the RC2 subquery one-liner in `docs/roadmap.md` with a block listing 4.x tasks landed, what punted to RC3 (4.8 — aggregator-inside-subquery), and the compat-suite expected delta. Update `internal/promql/lower.go` `Lower` doc comment.

**Test plan.** None.

**Dependencies.** 4.7 minimum; ideally after 4.10.

**Open questions.** Maintainer may prefer rolling into the final task's PR rather than standalone docs.

## Cross-cutting risks

1. **`Identity bool` vs `Func: "identity"` sentinel.** Prefer the boolean; cleaner separation.
2. **Matrix-shape SQL size**: a 24h subquery with 5m step → 288 anchors per series. CH handles it; long log lines are the only side-effect.
3. **Compatibility-suite drift at M6**: counter resets at anchor boundaries, empty-window NaN behaviour, etc. Pre-emptively note in `harness/compatibility/expected-failures.json`.
4. **`Step` field semantics overload**: serves both query_range and subquery inner-step. Document; consider splitting at RC3 cleanup.
5. **No new Sprintf-on-SQL** (CLAUDE.md hard rule). Existing range_window.go grandfathered; new code stays clean.
