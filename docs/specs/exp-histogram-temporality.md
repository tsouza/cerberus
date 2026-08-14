# Spec: Exponential-histogram window temporality

Tracks issue #2115.

## Problem

`rate()` and `increase()` over an exponential histogram always use the cumulative
counter-reset numerator, even when the table row declares OTLP DELTA temporality.

`internal/promql/histogram_quantile_native_window.go:84` builds the per-series
exponential-histogram aggregate list without `any(AggregationTemporality)`. Its
instant window fold at line 314 consequently supplies no temporality expression.
The corresponding range quantile fold does the same in
`internal/promql/histogram_quantile_range.go:675-693`. Histogram-valued native
`rate()` / `increase()` repeat the omission through
`expHistogramValuedWindowAggs` and `expHistogramValuedWindowFold` in
`internal/promql/histogram_native_rate.go:230-265` and `:309-320`.

For DELTA rows, each sample is an increment for its preceding interval. The
correct numerator is the raw sample sum excluding the earliest in-window sample,
not the counter-reset-aware consecutive difference. `counterIncreaseFold` already
implements that branch when it receives `_hq_temporality`; classic histogram
windows use it through `classicBucketWindowAggs` and
`classicBucketWindowTemporalityExpr` in
`internal/promql/histogram_quantile_window.go:722-764`.

## Scope

Included:

- PromQL lowering for native exponential-histogram `rate()` and `increase()` in
  instant and range-query forms, including native `histogram_quantile` window
  lowering and histogram-valued results.
- A per-series `any(AggregationTemporality)` aggregate only for those counter
  window functions and only when the configured schema names the column.
- TXTAR coverage with DELTA data and a CUMULATIVE control, plus regeneration of
  affected PromQL goldens after implementation.
- The spec-test seed DDL backfill needed for exponential-histogram fixtures to
  declare or default `AggregationTemporality`.

Excluded:

- Float-counter, classic-histogram, `irate`, `delta`, `sum_over_time`,
  `resets`, `changes`, and `count` semantics. Existing paths already have their
  own behavior; the added aggregate must not appear for non-counter windows.
- Schema migrations, configuration changes, ClickHouse SQL-emitter changes, and
  compatibility corpus changes.

## Design

Make the exponential-histogram aggregate helpers accept `windowFn`, mirroring
the classic helper's conditional aggregate policy. When `needsTemporalityAgg`
is true and `s.AggregationTemporalityColumn` is non-empty,
`expHistogramWindowAggs` appends
`any(AggregationTemporality) AS _hq_temporality`. Add a matching exponential
histogram temporality-expression helper that returns the alias only under the
same condition; otherwise it returns nil.

Thread `windowFn` through every paired aggregate-list consumer so the aggregate
used to build an `Aggregate` or `RangeBucketFanout` is identical to the alias
list forwarded into `expHistogramWindowReshape`. This includes the instant and
range native-quantile paths, both histogram-valued paths, and the
`resets()` / `changes()` aggregate selector. The latter must pass its function
through solely to keep the helper contract consistent and must not collect a
dead temporality column.

Pass the helper result into all three affected `histogramWindowFold` builders:
the instant quantile path, range quantile path, and histogram-valued fold. The
existing `counterIncreaseFold` DELTA branch then applies unchanged to Count,
Sum, ZeroCount, and every bucket. CUMULATIVE, unspecified, absent-column, and
non-counter paths retain the existing counter-reset behavior and emitted shape.

Extend `internal/testsql`'s temporality backfill table set to include
`otel_metrics_exponential_histogram`, with its existing CUMULATIVE default. This
allows old exponential-histogram fixtures to retain their behavior while new
fixtures can seed DELTA explicitly.

## Verification

1. Add generated PromQL TXTAR fixtures for a DELTA exponential histogram and a
   CUMULATIVE control using the same timestamps and bucket shape. The DELTA
   fixture must make the raw-sample numerator observably different from the
   cumulative reset-rule numerator and assert the full histogram-valued row,
   including Count, Sum, ZeroCount, and bucket counts.
2. Cover both instant and range-query lowering. At least one fixture must drive
   the native `histogram_quantile(... rate/increase(...))` path and one must
   drive the histogram-valued `rate()` / `increase()` path; together they must
   exercise the three fold call sites. Use the CUMULATIVE control pattern from
   `rate_delta_temporality_cumulative_control.txtar` to pin the unchanged
   branch.
3. Run `just update-golden promql` after the implementation and review every
   changed `test/spec/promql/*.txtar` section. The expected plan/SQL additions
   are the conditional `any(AggregationTemporality)` aggregate and its DELTA
   conditional numerator only on native counter windows.
4. Run the focused PromQL spec test covering the new fixtures through the
   repository's established spec-test invocation. Its chDB execution must prove
   the expected rows, not only the generated SQL. Run the targeted
   `internal/promql` unit tests that cover the changed lowering helpers.
5. Run `just test` once the implementation and generated artifacts are ready.
   Run `just compat-promql` in CI or an environment with real ClickHouse; the
   reference Prometheus side has no OTLP-temporality field, so the fixture's
   parity setup must continue to provide its cumulative equivalent.

## Risks

- The aggregate list and reshape alias list are paired contracts. Updating one
  without the other produces invalid SQL or a column-reference failure. The
  function parameter must be threaded through all consumers in lockstep.
- `any(AggregationTemporality)` is valid only because a physical OTel series has
  one temporality. Mixed-temporality rows within one identity remain malformed
  input and are not normalized here.
- Adding the test-DDL backfill can change generated fixture SQL broadly. The
  CUMULATIVE default and full golden review protect existing fixtures from a
  semantic change.
- Recent #2112 work changed these same native-histogram helper call sites.
  Rebase onto current `origin/main` before implementation and preserve its
  `resets()` / `changes()` aggregate selection.

## Task List For Owner Review

Each task is independently reviewable and names its verification.

1. Update the exponential-histogram aggregate and temporality-expression
   helpers in `internal/promql/histogram_quantile_native_window.go`; thread
   `windowFn` through its instant quantile caller. Verify focused lowering tests
   show `any(AggregationTemporality)` only for `rate` and `increase`.
2. Thread the same paired aggregate list and temporality expression through the
   range native-quantile path in
   `internal/promql/histogram_quantile_range.go`. Verify a range fixture's plan
   references only aliases produced by its fanout aggregate list.
3. Update `internal/promql/histogram_native_rate.go` and
   `internal/promql/histogram_native_resets.go` so histogram-valued instant and
   range paths receive the same conditional aggregate and fold expression, while
   `resets()` and `changes()` retain no unused temporality aggregate. Verify
   focused PromQL lowering tests for all four functions.
4. Add exponential histograms to the temporality seed-DDL backfill in
   `internal/testsql/seed_ddl.go`. Add DELTA and CUMULATIVE-control native
   histogram TXTAR fixtures under `test/spec/promql/` covering instant,
   range, quantile, and histogram-valued counter windows. Verify the fixtures'
   expected rows differ as designed between the two temporalities.
5. Regenerate with `just update-golden promql`; review the generated TXTAR
   changes; run the focused fixture and `internal/promql` tests, then `just
   test`. Verify `just compat-promql` through CI or real ClickHouse.
