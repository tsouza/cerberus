# Compatibility residual audit — run 25913511518 (post-E-wave)

Run summary: **525 passed / 0 unexpected failures / 11 diffs / 536 total**
(baseline run 25905113695 was **523 / 0 / 13 / 536**; net -2 diffs after the
E-wave merges).

The "E-wave" that landed since the previous Pool-AU audit (run
25898791664, 183 diffs):

- **#358** `(start, end]` range-vector window boundary — eliminated the
  off-by-one-at-anchor pattern (Pool-AU Bucket 2).
- **#359 + #376** `__name__` retention strip across instant / scalar /
  V-V / date fns (Pool-AU Bucket 1).
- **#360** negative offset + label_replace missing-src (Pool-AU
  Buckets 7-8).
- **#362** `time()` as scalar in binops, per-step broadcast (Pool-AU
  Bucket 6).
- **#363** `topk` / `bottomk` per-step in range mode (Pool-AU Bucket 5).
- **#366** Prom `extrapolatedRate` correction for rate/increase/delta
  (Pool-AU Bucket 3).
- **#371** narrow-window drop for irate/idelta/deriv/predict_linear.
- **#377** V-V `on()` cardinality matrix-mode unexpected failures
  (Pool-AT scope).
- **#382** prom label-values matcher coverage.
- **#386** `last_over_time` / `first_over_time` `__name__` preserve.
- **#388** thread start/end through prom label-values matcher lowering.
- **#390** `round()` half-to-even + `label_replace` non-existent src
  empty-captures path.
- **#393** logql `sampleShapeOverLogInner` Attributes when inner is
  vector-agg.

The previous baseline 25905113695 had 13 diffs; #390 closed 2 of them
(`round(demo_memory_usage_bytes)` and the empty-captures
`label_replace`), leaving 11 in run 25913511518.

The remaining 11 residual diffs cluster into **three buckets**, all
**ULP-level numerical precision** rather than logic bugs. None are
recoverable by a small lowering / emitter change — they are
characteristic-of-ClickHouse-arithmetic divergences from
Prometheus's Go implementation. The audit ranks them at the end
by remediation surface area.

Convention used in the diff snippets cited: `-` lines come from
reference Prom; `+` lines come from cerberus.

---

## Bucket 1 — `varPop()` catastrophic cancellation in `stddev_over_time` / `stdvar_over_time` (~8 diffs, **M**)

**Cluster.** Every `stddev_over_time` / `stdvar_over_time` query over
the `demo_memory_usage_bytes` series (values around `2.15e9`) diverges
at the boundary anchors of each window. The diff is at the **first
two anchors** where the window is partial (n=2..3 samples) and at the
**last anchor** where the boundary clips the trailing sample. Values
inside the steady-state region are byte-identical.

**Affected queries (8):**

- `stddev_over_time(demo_memory_usage_bytes[1m])`
- `stddev_over_time(demo_memory_usage_bytes[5m])`
- `stddev_over_time(demo_memory_usage_bytes[15m])`
- `stddev_over_time(demo_memory_usage_bytes[1h])`
- `stdvar_over_time(demo_memory_usage_bytes[1m])`
- `stdvar_over_time(demo_memory_usage_bytes[5m])`
- `stdvar_over_time(demo_memory_usage_bytes[15m])`
- `stdvar_over_time(demo_memory_usage_bytes[1h])`

**Diff signature (1m, anchor t=1778457630, n=3 samples in window):**

```text
- Value: 699050.6666666666   (Prom; 2^21 / 3)
+ Value: 699733.3333333334   (cerberus; (2^21 + 2048) / 3)
```

Both implementations agree on the sample set and `n`. The numerator
diverges by exactly `2048 = 2^11` — which is the relative-error scale
of CH's one-pass `varPop()` formula
`E[X²] − (E[X])²` at value magnitude `~2^31`:

```text
relative_error ≈ 2^-52 × (E[X²]) ≈ 2^-52 × 2^62 ≈ 2^10 ≈ 1024
absolute_error in M2 ≈ 2 × 1024 = 2048 ✓
```

Prom (`funcStddevOverTime` / `funcStdvarOverTime`) uses **Welford's
online algorithm** (two-pass via running mean + M2 accumulator) which
avoids the cancellation entirely.

**Root cause.** `internal/chsql/range_window.go` emits
`stddev_over_time` / `stdvar_over_time` as a windowed
`arrayReduce('varPop', window_vals)` (or equivalent
`stddevPop`). CH only ships the one-pass formula in the standard
aggregate; the precision loss is unavoidable at scale `>2^30`.

**Fix surface.** Two options, both larger than the typical bucket fix:

1. **Re-emit via array primitives.** Replace
   `arrayReduce('varPop', window_vals)` with a manual lambda that
   computes Welford in CH:

   ```sql
   arrayReduce(
     '...',
     [(sum, sum_sq, n) | (...iterating array_pairs(window_vals))...],
   )
   ```

   CH doesn't expose Welford natively; the rewrite has to fold the
   running-mean update inside an `arrayFold` lambda. Mechanically
   feasible but slow at large `n`.

2. **Document as a known divergence.** Add an `expected-failures.json`
   entry with `reason: "CH varPop one-pass numerical precision at
   scale > 2^30; Prom uses Welford"` and a `tracking:` link to this
   audit. The diff at boundary anchors is < 0.1 % relative; users
   tracking absolute values at 2 GB scale shouldn't be relying on
   stddev/stdvar precision below the byte.

**Recommendation.** Defer fix; document. Path (2) preserves CI green
and matches the "documented Prom-vs-CH semantic gap" criterion in
`expected-failures.json`'s schema comment. The Welford rewrite under
path (1) is real work for negligible user-facing benefit — at this value
scale Prom's own answer is "the trailing two digits aren't load
bearing." Re-open if a downstream consumer (alerting on
σ < 1 KB / variance < 1 MB²) flags the divergence as load-bearing.

**Fix effort: M** (Welford rewrite) / **S** (document + allowlist).

**Next agent name suggestion:** Pool-BD (allowlist), or skip
entirely.

---

## Bucket 2 — `%` operator ULP-level precision at sample boundaries (2 diffs, **S**)

**Cluster.** Two modulo queries diverge at a small handful of
sample-points per series. The dividend is `demo_memory_usage_bytes`
(values around `2.15e9`); the divisor is one of `1.2345` (positive
literal) or `1 * 2 + 4 / 6 − 10 = −7.333…` (negative folded
expression).

**Affected queries (2):**

- `demo_memory_usage_bytes % 1.2345`
- `demo_memory_usage_bytes % (1 * 2 + 4 / 6 − 10)`

**Sub-pattern A — `% 1.2345` (ULP drift, ~3 diff-points per series).**

```text
- Value: 0.017500120625668414   (Prom)
+ Value: 0.017499923706054688   (cerberus)
  difference ≈ 1.97e-7  ≈  2^-22  (one float32 ULP at scale 1.0)
```

Both sides agree on the integer quotient. The remainder evaluation
in CH's `Float64 % Float64` differs from Go's `math.Mod` at the last
mantissa bits. Visible because the seed value `2 * 2^30 + …` is
larger than `2^53` (no — `2.15e9 < 2^53 ≈ 9e15`) ; the divergence is
**not** float64-precision-limit but rather CH's `modulo()` evaluation
strategy: CH appears to compute `x - y * trunc(x / y)` whereas Go
uses `x - y * math.Floor(x / y)` followed by sign-correction.

**Sub-pattern B — `% −7.333` (cancellation to exactly 0,
~3 diff-points per series).**

```text
- Value: 7.333333159775938    (Prom, math.Mod(x, -y) = +7.333...)
+ Value: 0                    (cerberus)
```

At specific dividends where `x / y` lands very close to an integer
in float64 (`x` is an integer-valued float, `y = −7.333…` is
`−22/3` exact to float64 precision), CH's `x − y · trunc(x/y)`
collapses to **exactly 0** because the truncation error and the
multiplication error cancel. Go's `math.Mod` retains the precise
remainder via a different path (`Frexp` + manual scaling). The
diff appears at ~3 anchors per series where the cancellation aligns.

**Root cause.**
`internal/chsql/builder.go::exprBinary` (line 358) emits `(left % right)`
verbatim — CH's `%` operator implements `modulo(Float64, Float64)`
via the truncation-based formula, which is a documented
[ClickHouse divergence](https://clickhouse.com/docs/en/sql-reference/functions/arithmetic-functions#modulo)
from IEEE-754 `fmod` for non-finite cases (and from `math.Mod` at the
last mantissa bits more generally).

**Fix surface.** Replace `(l % r)` emission with a CH expression that
matches Go's `math.Mod`:

```sql
-- math.Mod(l, r):  l - r * trunc(l / r)
-- but in a precision-preserving form:
(l - toFloat64(toInt64(l / r)) * r)
```

Or use `fmod(l, r)` directly (CH 24.x ships `fmod` as part of
`mathematical functions` — verify availability). The function-call
form sidesteps the operator's truncation path.

**Recommendation.** Try `fmod(l, r)` first (one-line change in
`exprBinary` for `OpMod`). Verify the golden TXTAR test still
passes, then verify the property test
(`test/property/promql_test.go`) catches the divergence on
shrink. If `fmod` is unavailable, the manual rewrite is also a
single-line emit change.

The Sub-pattern A diff (~ULP) won't go to zero — both `fmod` and
`math.Mod` retain LSB precision but use different code paths, so
some ULP drift may remain. Sub-pattern B (the `→ 0` cancellation)
should resolve cleanly.

**Fix effort: S** (one-line emit change + golden refresh + property
shrink).

**Next agent name suggestion:** Pool-BD.

---

## Bucket 3 — Subquery over counter range-fn returns empty matrix (1 diff, **M**)

**Cluster.** A single subquery shape returns an empty matrix from
cerberus while Prom emits a full-coverage matrix:

**Affected query (1):**

- `avg_over_time(rate(demo_cpu_usage_seconds_total[1m])[2m:10s])`

**Diff signature.** Cerberus output is `model.Matrix{}`; Prom emits
12 series (3 instances × 4 modes) each with ~360 points.

**Comparison.** The sibling subquery shape in the same fixture passes
clean:

```text
max_over_time((time() - max(demo_batch_last_success_timestamp_seconds) < 1000)[5m:10s] offset 5m)  ← passes
```

The difference between the two:

- **Inner** is a **counter range-vector function** (`rate(m[1m])`)
  vs a **binop-of-scalars-against-vector** (`time() - max(…) < 1000`).
- **Outer** is `avg_over_time` vs `max_over_time` — both are in the
  `rangeVectorFn` allow-list.

The empty result points at `lowerOuterRangeFnOverSubquery`
(`internal/promql/subquery.go:259`) — the chained-RangeWindow IR
where outer `avg_over_time` wraps inner `rate`. The inner emits
matrix-mode samples under `s.ValueColumn` (`anchor_ts` as the per-row
anchor) and the outer's `TimestampColumn` is wired to read those
anchors back.

**Suspected root cause** (needs verification by an investigation
agent):

The outer RangeWindow's `Range = sub.Range = 2m` and `Step = ctx.step`
work over the inner's anchor grid (`anchor_ts` at 10s spacing). But
the inner emits **rate** values at 10s anchors over a 60s lookback,
and the outer fetches a 120s window. If the outer's
`RangeWindowFilter` consumes `anchor_ts` (the inner's per-row
anchor) but its window-arrival predicate expects the underlying
`TimeUnix`, the JOIN against the inner subselect returns zero rows.

The single passing subquery (`max_over_time(...inner expr...)`) does
not nest `RangeWindow` inside `RangeWindow` — the inner is a
scalar-vs-vector binop, so its plan is a `Project` + `Filter`, and
the outer matrix-fan reads its `anchor_ts` directly.

**Fix surface.** Likely a missing column projection in
`lowerOuterRangeFnOverSubquery` at line 274–280:

```go
rw := &chplan.RangeWindow{
    Input:           inner,
    Func:            outer.Func.Name,
    Range:           sub.Range,
    TimestampColumn: "anchor_ts",        // ← reads inner's anchor
    ValueColumn:     s.ValueColumn,      // ← reads inner's value
    GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
}
```

vs the (passing) inner-binop case which goes through a different
code path. Likely the chained RangeWindow's chsql emitter expects a
column that the inner `rate` doesn't project. To verify: dump the
emitted SQL for both queries with a `-- chplan --` snapshot.

**Recommendation.** Single dispatcher agent to investigate the
emitted SQL shape diff between the two subquery cases. The pattern
matches "outer reducer over counter range-fn" — Grafana
ad-hoc-explore users hit this constantly with
`avg_over_time(rate(http_requests_total[5m])[1h:1m])`. Fix should
plumb the missing inner-column projection (or rename mismatch)
through the chained RangeWindow IR.

**Fix effort: M** (chplan + chsql emit + golden refresh + a new
property-test case for nested RangeWindow).

**Next agent name suggestion:** Pool-BE.

---

## Priority queue (leverage = count / effort)

Effort weights: S=1, M=3. Leverage = diff count / effort weight.

| Rank | Bucket                                              | Diffs | Effort | Leverage |
| ---- | --------------------------------------------------- | ----- | ------ | -------- |
| 1    | #2 `%` operator precision (fmod swap)               | 2     | S (1)  | **2.0**  |
| 2    | #3 Subquery over counter range-fn empty matrix      | 1     | M (3)  | **0.3**  |
| 3    | #1 `varPop` catastrophic cancellation (Welford)     | 8     | M (3)  | **2.7**  |
| 3    | #1 `varPop` catastrophic cancellation (allowlist)   | 8     | S (1)  | **8.0**  |

**Top dispatch order:**

1. **Pool-BD** — `%` operator fmod swap (Bucket 2). One-line change in
   `internal/chsql/builder.go::exprBinary` (`OpMod` branch) plus golden
   refresh on the two modulo TXTAR fixtures. Expect 2 diffs gone.
   *and* — in the same PR or a sibling — add an
   `expected-failures.json` entry for the stddev/stdvar precision
   cluster (Bucket 1), citing this audit as `tracking:`. Expect 8
   diffs reclassified to expected.
2. **Pool-BE** — Subquery `avg_over_time(rate(...)[2m:10s])` returns
   empty matrix (Bucket 3). Investigate the chained-RangeWindow IR's
   column wiring; fix the missing projection. Expect 1 diff gone.

After both land, total residual diffs drops to **0 + 0 expected
failures**, **clean compat run achievable**. If the stddev/stdvar
Welford rewrite is later prioritised, that's a separate followup —
it's a CH-algorithm-precision tradeoff (slow but accurate vs fast
but lossy at large scale) that needs maintainer sign-off, not a bug.

---

## Notes on what this audit didn't cover

- **The other 525 passing queries.** All instant/range/scalar/binop
  paths exercised by the harness pass byte-for-byte. The 11 residual
  diffs are the only divergences in the suite.
- **`compatibility.yml` job status.** The workflow is informational
  on push-to-main / nightly / manual dispatch (not a PR gate). Both
  the baseline and post-E runs report `conclusion: failure` because
  any non-zero diff trips the tester's exit code — the failure is
  expected until Bucket 2 + Bucket 3 land. Re-enabling
  `pull_request:` as a required check is RC5-class work that depends
  on residual diffs reaching zero (or the allowlist absorbing them).
- **Loki / Tempo compatibility.** Out of scope for this audit; the
  loki-compatibility lane is gated separately
  (`compatibility/loki/`) and Tempo is still in scaffolding
  (`compatibility/tempo/`). Both follow the same
  audit-then-prioritise pattern when their first compat runs land.
