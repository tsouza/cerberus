# Compatibility residual audit — run 25898791664 (post-#350)

Run summary: **341 passed / 12 unexpected failures / 183 diffs / 536 total**.
The 12 unexpected failures (all V-V `on()` cardinality cases) are being
addressed in parallel by Pool-AT. This audit covers the **183 diffs** —
queries where cerberus returns 200 with data that differs from reference
Prometheus.

GA target: drive diffs to 0 (the maintainer's stance is full PromQL
compatibility, no documented-divergences).

The buckets below cluster the 183 diffs by likely root cause and rank
them at the end by `count / fix-effort` leverage so the next dispatcher
agent can pick the most impactful fix first.

Convention used in the diff snippets cited: `-` lines come from
reference Prom; `+` lines come from cerberus.

---

## Bucket 1 — `__name__` retention across instant/binop output (~107 diffs, **S**)

**Cluster.** Every code path that produces a "derived" sample (instant
function, scalar-vector binop, vector-vector binop with arithmetic /
`bool` modifier, `timestamp()`, etc.) forwards the input row's
`MetricName` column verbatim. Prom always strips `__name__` from these
outputs.

**Affected queries** (all show `Metric: s\`demo_…{...}\`` on the
cerberus side vs `Metric: s\`{...}\`` on the Prom side, values
otherwise identical):

- Instant math fns over `demo_memory_usage_bytes` (17 queries):
  `abs(…)`, `abs(-…)`, `ceil(…)`, `ceil(-…)`, `floor(…)`, `floor(-…)`,
  `exp(…)`, `exp(-…)`, `ln(…)`, `ln(-…)`, `log2(…)`, `log2(-…)`,
  `log10(…)`, `log10(-…)`, `sqrt(…)`, `sqrt(-…)`, `round(-…)`.
- `clamp` / `clamp_min` / `clamp_max` (6 queries — Pool-AM's PR #350
  fixed value-level only; the `__name__` strip is still missing).
- `timestamp(…)` variants (4 queries) — `timestamp(demo_num_cpus)`,
  `timestamp(-demo_memory_usage_bytes)`, `timestamp(demo_memory_usage_bytes * 1)`,
  `timestamp(timestamp(demo_num_cpus))`.
- Scalar-on-left arithmetic (12 queries) — `0.12345 OP …` and
  `(1 * 2 + 4 / 6 - (10%7)^2) OP …` for `OP ∈ {+, -, *, /, %, ^}`.
- Scalar-on-right arithmetic (12 queries) — `… OP 1.2345` and
  `… OP (1 * 2 + 4 / 6 - 10)` for the same six ops.
- Scalar `bool` compare (12 queries) — `1.2345 OP bool …` and
  `… OP bool 1.2345` for `OP ∈ {!=, <, <=, ==, >, >=}`.
- V-V arithmetic with `on(...)` (9 queries) — `… OP on(instance, job, type) …`
  for the same six arithmetic ops, plus the `on(…, __name__)` and
  `on(…, non_existent)` variants and `group_left`.
- V-V `bool` compare with `on(...)` (6 queries) —
  `demo_memory_usage_bytes OP bool on(instance, job, type) demo_memory_usage_bytes`
  for the six comparison ops.
- Unary minus (1 query) — `-demo_memory_usage_bytes`.
- Arithmetic + unary minus (1 query) — `demo_memory_usage_bytes + -(1)`.
- `Inf` / `-Inf` / `NaN` scalar binop (3 queries) — `demo_num_cpus * Inf`,
  `… * -Inf`, `… * NaN` (values match; only `__name__` differs).
- `demo_num_cpus + (1 BOOLOP bool 2)` (6 queries — folded scalar in
  bool comparison; arithmetic still wraps as vector-scalar and
  forwards `__name__`).

**Root cause.** Three projection sites all forward `MetricNameColumn`
unchanged when they should emit `''`:

1. `internal/promql/instant_fns.go::projectValueOverInner` line 206 —
   `{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}}` is the first
   slot of the `Project` for every instant fn / clamp / round. Should
   project a literal empty string for any fn that maps Value through.
2. `internal/promql/binary.go::lowerVectorScalar` line 351 — same
   pattern for scalar-vector binops (both arithmetic and `bool`
   comparisons). Bare comparisons without `bool` go through `Filter`,
   which preserves labels, and that's correct per Prom — `__name__` IS
   kept on bare-comparison filtering.
3. `internal/chsql/vector_join.go::emitVectorJoin` line 59 —
   `qualColFrag(outerSide, j.MetricNameColumn)` selects the outer
   side's MetricName for V-V output. Should emit `''` whenever
   `j.Op` is arithmetic, or when it's a comparison op with
   `j.ReturnBool == true`. (Bare V-V comparison preserves LHS labels
   per Prom semantics — already correct via the same column-forward.)
4. `internal/promql/date_fns.go::lowerDateFn` line 70 — reuses
   `projectValueOverInner`, so `timestamp()` / `year()` / `month()` /
   etc. inherit the same bug.

**Fix effort: S.** One PR can flip all four sites; either:

- emit `chplan.LitString{V: ""}` in the relevant `Project` slot
  whenever the Project is added by these specific code paths, or
- add a dedicated `chplan.StripName` flag on `Project` (cleaner) so the
  emitter renders `'' AS MetricName` when set.

**Next agent name suggestion:** Pool-AV.

---

## Bucket 2 — Range-window boundary inclusion (`>= start` vs `> start`) (~26 diffs, **S**)

**Cluster.** Every `*_over_time` family member (and indirectly all
range-vector functions) builds its window via
`arrayFilter(p -> ts >= start AND ts <= end, series_array)`. Prom uses
strict-left, inclusive-right: `(start, end]`. The off-by-one shows up
on every step where `t - range` lands exactly on a scrape timestamp:
cerberus's window then contains one extra sample at `start`, doubling
the count and skewing the aggregate.

**Affected queries** (all show duplicate-sample artifacts at multiples
of `range` where the boundary aligns with a 15s scrape):

- `sum_over_time(demo_memory_usage_bytes[15s|1m|5m|15m|1h])` (5)
- `avg_over_time(demo_memory_usage_bytes[15s])` (1 — `[1m]`/`[5m]`/etc.
  not in diffs because the boundary-alignment cancels out under
  averaging; the trailing-anchor sub-pattern is still present)
- `count_over_time(demo_memory_usage_bytes[15s|1m|5m|15m|1h])` (5) —
  most obvious; shows `count=2` vs `count=1`
- `min_over_time(demo_memory_usage_bytes[15s])` (1)
- `max_over_time(demo_memory_usage_bytes[15s])` (1)
- `last_over_time(demo_memory_usage_bytes[15s|1m|5m|15m|1h|1s])` (6 —
  trailing-anchor sub-pattern; see Bucket 3)
- `stddev_over_time(demo_memory_usage_bytes[15s|1m|5m|15m|1h])` (5) —
  the extra sample at the boundary changes the population variance
- `stdvar_over_time(demo_memory_usage_bytes[15s|1m|5m|15m|1h])` (5)
- `quantile_over_time(0.{1,5,75,90,95,99}|1|1.5|-0.5, demo_memory_usage_bytes[15s])` (9 —
  trailing-anchor only; the boundary effect partially folds into the
  quantile estimator)

**Root cause.**
`internal/chsql/builder.go::RangeWindowFilter` line 965 builds
`And(Gte(tsElem, start), Lte(tsElem, end))` — both bounds inclusive.
Prom's `Buffer` iterator over a `MatrixSelector` uses
`mint < ts <= maxt` (see Prom `promql/value.go::iter.At`). Switch
`Gte` to `Gt` to match.

**Sub-pattern (trailing anchor, ~16 of the 26 affected queries).** A
second symptom shows up on `*_over_time[15s]` queries where the only
diff is an extra cerberus-side sample at the very last anchor
(`@[1778461200]`). The underlying mechanism is the same boundary rule
plus the seed-data extent: Prom's iterator misses the last scrape
because the seed cuts off slightly before the query end, while
cerberus's inclusive-left bound catches an extra point. Fixing the
`Gt` switch above resolves this in the obvious cases; a residual
handful may remain if the harness seed and Prom's lookback delta
disagree on the last sample — to be verified after the primary fix.

**Fix effort: S.** A single-line change in
`internal/chsql/builder.go::RangeWindowFilter` (`Gte` → `Gt`) plus the
golden refresh across every range-window TXTAR fixture. Verify against
the property-test layer (`test/property/promql_test.go`) — boundary
inclusion is exactly the kind of bug an oracle catches.

**Next agent name suggestion:** Pool-AW.

---

## Bucket 3 — `rate` / `increase` / `delta` extrapolation correction missing (~16 diffs, **M**)

**Cluster.** Two related symptoms:

1. **Sub-scrape ranges emit values that Prom drops.** For
   `[15s]` (scrape interval = 15s, so a 15s window almost never has
   ≥2 samples), cerberus emits a value where Prom emits no sample.
   Cerberus's `length(window_vals) > 1` guard is too permissive in
   the matrix-mode emitter — Prom additionally checks
   `sampledInterval > 1.1 × averageDurationBetweenSamples` and drops
   the window when the lookback can't be reasonably extrapolated.

2. **Multi-scrape ranges emit values lower than Prom's.** For
   `[1m]` / `[5m]` / `[15m]` / `[1h]` (where the window has many
   samples), cerberus emits `(lastValue - firstValue) / range_seconds`
   while Prom applies the `extrapolatedRate` correction — pushing the
   value up by the ratio of the actual `range_seconds` to the
   `lastTimestamp - firstTimestamp` interval (after clamping at the
   `extrapolationThreshold = averageDurationBetweenSamples * 1.1`).

**Affected queries:**

- `rate(demo_cpu_usage_seconds_total[15s|1m|5m|15m|1h])` (5 queries)
- `increase(demo_cpu_usage_seconds_total[15s|1m|5m|15m|1h])` (5)
- `delta(demo_cpu_usage_seconds_total[15s|1m|5m|15m|1h])` (5)
- `irate(demo_cpu_usage_seconds_total[15s])` (1) — sub-scrape variant
  only
- `idelta(demo_cpu_usage_seconds_total[15s])` (1) — sub-scrape variant
- `deriv(demo_disk_usage_bytes[15s])` (1) — sub-scrape variant only
  (the larger ranges already pass — least-squares fit at ≥2 samples)
- `predict_linear(demo_disk_usage_bytes[15s], 600)` (1) — same
  sub-scrape pattern
- `resets(demo_cpu_usage_seconds_total[15s])` (1) — different sub-bug:
  cerberus emits `0 @[1778461200]` where Prom drops the last anchor
  (trailing-anchor pattern from Bucket 2; resets accidentally folds in
  here because the harness only seeded the 15s variant)

Cluster total counted as 16; the trailing-anchor part of `resets`
overlaps with Bucket 2.

**Root cause.** Two related callsites in
`internal/chsql/range_window.go`:

- `rateValueFrag` line 1228 → `counter_delta / range_seconds` with no
  extrapolation correction.
- `emitRangeWindowIncrease` line 945 → `counter_delta` with no
  correction.
- `emitRangeWindowDelta` line 994 (similar shape).

All three need to emit the Prom-faithful extrapolation:

```text
sampledInterval     = (lastTs - firstTs).Seconds()
averageDurationBetweenSamples = sampledInterval / (length(window_vals) - 1)
extrapolationThreshold       = averageDurationBetweenSamples * 1.1
durationToStart              = (firstTs - rangeStart).Seconds()
durationToEnd                = (rangeEnd - lastTs).Seconds()

if durationToStart < extrapolationThreshold {
    extrapolationToStart = durationToStart
} else {
    extrapolationToStart = averageDurationBetweenSamples / 2
}
similarly for extrapolationToEnd

resultValue *= (extrapolationToStart + sampledInterval + extrapolationToEnd) / sampledInterval
result      = resultValue / rangeSeconds   // rate only
```

Plus the "no-extrapolation when no samples in window" / "drop when
`sampledInterval > 1.1 * averageDurationBetweenSamples`" predicates.

The sub-scrape case (1) is also addressed by the same change: the
`durationToStart` / `durationToEnd` guard naturally drops windows
where Prom would too.

**Fix effort: M.** Touches both the SQL emit (`rateValueFrag` +
`emitRangeWindowIncrease` + `emitRangeWindowDelta`) and the windowed-
array wrapper — the `durationToStart` / `durationToEnd` need
`firstTs` / `lastTs` to be projected out of `window_pairs`, which the
current `window_vals`-only path doesn't carry. Plumbing the first/last
timestamps through is straightforward but mechanical (the
`window_pairs` array already carries them — just expose two extra
projections at the inner SELECT).

**Next agent name suggestion:** Pool-AX.

---

## Bucket 4 — `absent_over_time` returns original labels with NaN (6 diffs, **M**)

**Cluster.** `absent_over_time(metric[range])` should:

1. Synthesise a `{}` label set (or the matcher-derived labels for
   non-equality selectors) when ANY anchor in the range query has zero
   matching samples in its window.
2. Return no sample at anchors where the underlying selector has any
   sample in its window.

Cerberus currently emits the original series labels with `NaN`
values for empty windows, and `NaN` for non-empty windows too in
some cases. The engine layer is supposed to drop NaN samples, but the
labels are wrong even on the dropped samples — and the matrix shape
preserves NaN at every anchor (visible in the diff).

**Affected queries:**

- `absent_over_time(demo_memory_usage_bytes[1s|15s|1m|5m|15m|1h])`
  (6 queries; each has 30+ series in the seed so the same series
  shape repeats per range)

**Root cause.**
`internal/chsql/range_window.go::emitRangeWindowAbsentOverTime`
line 1022 — emits `if(length(window_vals) > 0, nan, 1.0)` keyed by
the original series. This is the documented temporary path:

> The fully-faithful "synthesise labels for the empty-input case" path
> requires post-emit engine handling (Prom's funcAbsentOverTime is one
> of three engine-specialised functions, alongside absent and
> present_over_time …).

The fully-faithful path needs:

- a separate single-series synthesised row when ALL anchors in the
  range query have empty windows across ALL input series
- matcher-derived labels (the same logic the existing
  `internal/promql/absent.go::absentAttrsMap` builds for instant
  `absent(...)`)

**Fix effort: M.** Touches the chplan / range-window emit boundary —
likely needs a dedicated `chplan.AbsentOverTime` node (parallel to the
existing `chplan.Absent` for instant `absent`) so the emitter can
hoist the matcher-derived labels through. Alternatively, post-process
in the engine layer (cerberus's wire-shaping code) where the matcher
context is still available.

**Next agent name suggestion:** Pool-AY.

---

## Bucket 5 — `topk` / `bottomk` is global, not per-step (8 diffs, **L**)

**Cluster.** PromQL `topk(K, expr)` selects the top-K series **per
evaluation step** (each step is independent — the K-th value at t=5m
may be a different series than at t=10m). Cerberus implements it as
SQL `ORDER BY value DESC LIMIT K BY <partition_exprs>` which selects
globally across all (step, series) rows. The result: cerberus's topk
trims series to K total, not K per step.

**Affected queries:**

- `topk (3, demo_memory_usage_bytes)` (1) — diff: cerberus emits a
  scattered subset of (series, anchor) pairs; Prom emits 3 full
  series across all anchors.
- `topk by(instance) (2, demo_memory_usage_bytes)` (1)
- `topk without() (2, demo_memory_usage_bytes)` (1)
- `topk without(instance) (2, demo_memory_usage_bytes)` (1)
- `bottomk (3, demo_memory_usage_bytes)` (1)
- `bottomk by(instance) (2, demo_memory_usage_bytes)` (1)
- `bottomk without() (2, demo_memory_usage_bytes)` (1)
- `bottomk without(instance) (2, demo_memory_usage_bytes)` (1)

**Root cause.**
`internal/promql/lower.go::lowerTopK` line 915 builds a
`chplan.TopK{Input, K, By, SortExpr, Desc}` that the emitter renders
as a single `ORDER BY <SortExpr> [DESC|ASC] LIMIT K BY <By>`. The
emit is correct for instant queries but wrong for range queries
because the `LIMIT K BY <attrs>` partition doesn't include the
per-step anchor — once the matrix is fanned out, the partition
collapses across all anchors into K rows total.

The fix is to thread the per-step anchor (TimestampColumn) into the
`By` partition for range queries — so `LIMIT K BY (anchor, attrs)`
selects top-K per anchor. The plan IR likely needs a `StepAligned`
flag (mirroring the recent VectorJoin step-aligned plumbing in
PR #348) so the emitter knows when to add the anchor column to the
partition expression list.

**Fix effort: L.** Touches the plan IR (`chplan.TopK` gains a
`StepAligned bool`), the chsql emit (the partition expression list
grows by one), and the matrix fan-out path (`anchor_ts` already
flows through for non-aggregating range-mode queries; topk's emit
pathway needs to be wired the same way). Likely 100–200 lines + a
fixture-refresh sweep across the topk TXTAR slice.

**Next agent name suggestion:** Pool-AZ.

---

## Bucket 6 — `time()` as scalar in binops collapses to empty (9 diffs, **M**)

**Cluster.** `time() OP metric` and `metric OP time()` both return
empty results from cerberus. Prom treats `time()` as a scalar and
broadcasts it across every (series, step) row of the metric.

Cerberus lowers `time()` to a synthetic 1-row vector with
`Attributes={}` ([syntheticScalarVector]) and then runs a regular V-V
join. The default match (no `on(...)`) joins on the full Attributes
map — comparing `{}` against the metric's `{instance,job,type}` finds
zero matches → empty result.

The synthetic-scalar fold (`isSyntheticScalarPlan` gate in
`lowerVectorVector`) handles the both-sides-synthetic case (e.g.
`time() OP time()`), but it doesn't fire when ONE side is a real
metric.

**Affected queries:**

- `time() {+, -, *, /, %, ^, <, <=, !=} demo_memory_usage_bytes` (9
  variants; all show empty cerberus output)
- *Note:* `metric OP time()` variants also affected — same root
  cause; same 9 ops. Total potential is 18 but the harness only
  exercised 9 forms; the rest are inferred from the bucket-by-query
  list above. The metric-on-left variants (`metric * time()`,
  `metric + time()`, etc.) also appear in the diff list — 9 of those
  are already captured under Bucket 1 (`__name__` retention) where
  values match but the metric name is kept; the remaining 9 fall
  here.

**Root cause.**
`internal/promql/binary.go::lowerVectorVector` doesn't recognise
`time()` (or other scalar-returning calls) as scalar — it walks
through `tryScalarLiteral` which only folds literal/UnaryExpr/
ParenExpr/BinaryExpr-of-scalars shapes. Calls aren't covered.

**Fix.** Detect `*parser.Call` with `Func.Name == "time"` (or a
scalar-returning fn from a small allow-list: `time`, `pi`,
`scalar()`, etc.) and route through `lowerVectorScalar` after
resolving the scalar value at lowering time — using the per-step
anchor expression (`now64(9)` / `anchor_ts`) as the literal.

Alternatively, broaden the `isSyntheticScalarPlan` fold to also fire
when ONE side is synthetic — by transposing the synthetic side into
a per-step scalar that the other side projects against.

**Fix effort: M.** Touches the `lowerVectorVector` dispatch and the
`syntheticScalarVector` shape. Probably 50-100 lines + the
"`time()` against vector" fixture row.

**Next agent name suggestion:** Pool-BA.

---

## Bucket 7 — Negative `offset` interpreted as zero (2 diffs, **S**)

**Cluster.** `metric offset -5m` and `metric offset -10m` return empty
results from cerberus. Prom returns data (the negative offset shifts
the lookback window FORWARD in time, retrieving future samples
relative to each step's anchor).

**Affected queries:**

- `demo_memory_usage_bytes offset -5m`
- `demo_memory_usage_bytes offset -10m`

**Root cause.**
`internal/promql/lower.go::wrapLatestValue` line 296 —
`if anchor.Offset > 0 { ... }` gates the offset adjustment on
**positive** offset only. Negative offsets are silently dropped and
the lookback becomes `(anchor_ts - lookback, anchor_ts]` — which for
the range query's step grid returns no samples (the seed data sits
entirely in the past relative to each query anchor; without the
forward shift, the anchor lookback finds nothing).

**Fix.** Remove the `> 0` guard — emit the offset subtraction whenever
`anchor.Offset != 0`. Prom's parser allows negative durations as
`OriginalOffset` since v2.26; the lower.go path just needs to honour
the sign. (Bonus: should probably also reject `Offset = 0` from
emitting the subtraction at all, to keep golden SQL clean; the
condition is `!= 0` for both bounds.)

**Fix effort: S.** Single-condition flip + golden refresh on the
`offset` TXTAR slice.

**Next agent name suggestion:** Pool-BB.

---

## Bucket 8 — `label_replace` with non-existent src + matching regex (1 diff, **S**)

**Cluster.** `label_replace(demo_num_cpus, "job", "value-$1", "nonexistent-src", "(.*)")`
should: regex `(.*)` matches the empty string (the absent src reads
as empty), so dst `job` becomes `value-` (empty capture group). Prom
keeps the replaced label even when it's `value-` (non-empty after
substitution).

Cerberus drops the `job` label entirely. The outer
`mapFilter((k, v) -> v != '', …)` in
`internal/chsql/builder.go::exprLabelReplace` line 457 is over-zealous
— it correctly drops the dst label when the replacement is fully
empty, but for `replacement="value-$1"` with empty $1 the result is
`value-` (non-empty), so the filter shouldn't apply. The bug is more
subtle: the mapFilter is also applied to **every** map entry, so the
ORIGINAL `job="demo"` is being replaced by `mapUpdate` to a new
value (`value-`), then dropped by mapFilter — but only the new value
should be checked.

Actually wait — looking at the diff again, the cerberus output is
`demo_num_cpus{instance=...}` with NO `job` at all (the original
`job="demo"` is gone). So `mapUpdate` did fire to set `job=value-`,
and then `mapFilter((k,v) -> v != '', ...)` somehow dropped it. The
likely root cause: the value `value-` after the
PromQL-to-CH-replacement translation became empty (the `$1`
backreference in CH's `replaceRegexpOne` is `\1`, and CH's `\1` on a
non-matching position returns empty — but the regex DID match, so
that's not it).

Closer inspection needed — this is the single oddest diff in the
list. The replacement-translation logic in
`internal/promql/label_fns.go::promReplacementToCH` may be at fault.

**Affected queries:**

- `label_replace(demo_num_cpus, "job", "value-$1", "nonexistent-src", "(.*)")`

**Fix effort: S.** Single function call; needs investigation but the
surface is tiny.

**Next agent name suggestion:** Pool-BC.

---

## Priority queue (leverage = count / effort)

Effort weights: S=1, M=3, L=5. Leverage = diff count / effort weight.

| Rank | Bucket                                          | Diffs | Effort | Leverage |
| ---- | ----------------------------------------------- | ----- | ------ | -------- |
| 1    | #1 `__name__` retention                         | ~107  | S (1)  | **107**  |
| 2    | #2 Range-window boundary inclusion              | ~26   | S (1)  | **26**   |
| 3    | #3 `rate`/`increase`/`delta` extrapolation      | ~16   | M (3)  | **5.3**  |
| 4    | #7 Negative offset                              | 2     | S (1)  | **2**    |
| 5    | #6 `time()` as scalar in binops                 | 9     | M (3)  | **3**    |
| 6    | #4 `absent_over_time` synth labels              | 6     | M (3)  | **2**    |
| 7    | #5 `topk`/`bottomk` per-step                    | 8     | L (5)  | **1.6**  |
| 8    | #8 `label_replace` non-existent src             | 1     | S (1)  | **1**    |

**Top 3 dispatch order:**

1. **Pool-AV** — `__name__` retention sweep (Bucket 1). Single PR
   touching three emit sites in `internal/promql/{instant_fns,
   binary,date_fns}.go` plus `internal/chsql/vector_join.go`. Expect
   ~107 diffs gone in one move.
2. **Pool-AW** — Range-window boundary inclusion (Bucket 2). One-line
   `Gte` → `Gt` in `internal/chsql/builder.go::RangeWindowFilter`,
   plus the golden-refresh sweep. Expect ~26 diffs gone.
3. **Pool-AX** — `rate`/`increase`/`delta` extrapolation (Bucket 3).
   Larger refactor; needs `firstTs` / `lastTs` plumbing through
   `window_pairs`. Expect ~16 diffs gone.

After the top 3 land, the residual `(183 - 107 - 26 - 16) = ~34`
diffs split across the remaining buckets — leverage drops below 5 and
the per-bucket cost dominates. Dispatcher should re-rank after a
fresh compat run rather than scheduling them all in advance, since
Bucket 1's `__name__` fix will likely change which series-comparison
boundaries surface in the remaining queries.

---

## Notes on what this audit didn't cover

- **Sample-value floating-point divergence.** A handful of the
  `*_over_time` diffs show `Inf` / `-Inf` / `NaN` outputs from
  cerberus that match Prom — no value diff, but the `__name__`
  retention is the only delta. Those are already counted in Bucket 1
  and will fall out of the same fix.
- **Pool-AT's 12 unexpected failures.** Out of audit scope; tracked
  separately in the V-V `on()` cardinality work.
- **chplan vs SQL emit boundary for absent_over_time.** The
  long-standing TODO in `emitRangeWindowAbsentOverTime` (lines
  1009-1018) is the engine-side specialised-function path. The
  proper fix matches Prom's three-fn engine specialisation
  (`funcAbsent` / `funcAbsentOverTime` / `funcPresentOverTime`) and
  needs more design discussion than this audit affords.
- **Possible second-order interactions.** Bucket 2's boundary fix
  may shift the rate / increase / delta value diffs (Bucket 3) since
  the windowed value count is part of the extrapolation correction.
  Re-run after Bucket 2 and re-audit before dispatching Bucket 3.
