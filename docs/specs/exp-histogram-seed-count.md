# Exponential-Histogram Seed Count

## Problem

Issue #2023 identified a test-harness split-brain: `internal/testsql/seed_ddl.go`
backfills an omitted `Count` on both histogram tables as `UInt64 DEFAULT 0`. For
`otel_metrics_exponential_histogram`, fixtures commonly provide non-empty bucket
arrays but omit `Count`, producing an OTel-invalid stored row. The native parity
reader in `test/spec/parity_native_histogram_chdb.go` correctly derives an omitted
count as `ZeroCount + sum(PositiveBucketCounts) + sum(NegativeBucketCounts)` before
seeding Prometheus. Consequently Prometheus and Cerberus receive different
histograms.

This disables Cerberus's native `durationToZero` path: the lowering gathers
`Count` in `internal/promql/histogram_quantile_native_window.go`, and the shared
extrapolation expression enables its zero-crossing clamp only when the folded
count increase is positive. The issue's
`histogram_quantile_native_agg_at_pinned_range` example demonstrates the result:
the current Cerberus golden is `7.249566537724973`; an internally consistent seed
engages the clamp and matches Prometheus at `7.21000370088664`.

## Scope

Included:

- Change missing-`Count` backfill only for
  `otel_metrics_exponential_histogram` to derive the value from its zero and
  bucket-count columns.
- Retain the literal-zero backfill for the classic histogram table.
- Add focused harness coverage and regenerate affected PromQL expected rows.
- Enroll the demonstrated pinned-range fixture in Prometheus parity.

Excluded:

- Production PromQL lowering, schema, SQL emission, and HTTP behavior.
- Changing the parity oracle's missing-`Count` derivation.
- Repairing classic-histogram fixtures that omit `Count`; that is a distinct
  seed-modeling concern noted in #2023.

## Design

Extend the test-only backfill registry so its `Count` definition can be selected
per physical table. The classic `otel_metrics_histogram` continues to receive
`Count UInt64 DEFAULT 0`. When `otel_metrics_exponential_histogram` omits
`Count`, inject a `UInt64` default expression equivalent to:

```sql
ZeroCount + arraySum(PositiveBucketCounts) + arraySum(NegativeBucketCounts)
```

The existing create/positional-insert rewrite remains responsible for omitting
backfilled columns from INSERT lists, allowing ClickHouse to evaluate the
default. A declared `Count` remains untouched. The expression uses exactly the
same OTel count invariant and source columns as `totalObservations`; the parity
reader therefore needs no semantic change.

The implementation must confirm every exponential-histogram seed eligible for
this default declares the three source columns. If a supported minimal seed can
omit one, the registry must preserve a valid schema while maintaining the OTel
invariant; it must not introduce a default that references an absent column.

## Verification

- Add unit coverage in `internal/testsql/backfill_columns_test.go` that verifies
  the exponential table receives the derived default, while the classic table
  retains its zero default and an explicitly declared `Count` is not duplicated.
- Regenerate, never hand-edit, affected PromQL TXTAR outputs with
  `just update-golden promql`; inspect the resulting `test/spec/promql/` diff,
  particularly native-quantile `expected_rows`.
- Add `-- parity --` Prometheus enrollment to
  `test/spec/promql/histogram_quantile_native_agg_at_pinned_range.txtar` and
  verify it exercises the corrected clamp result.
- Run `just test` after implementation to cover the seed backfill, spec runner,
  and tagged compilation lanes.
- Run `just compat-promql` against the real reference backend to validate the
  enrolled fixture and the native-histogram corpus.

## Risks

- ClickHouse default expressions are validated against the final table schema;
  a fixture missing a referenced bucket column would fail at CREATE time. Audit
  those fixture shapes before finalizing the registry representation.
- The backfill utility is shared by spec, strict-scan, and chDB client seed
  paths. A registry change can alter any exponential-histogram fixture that
  relies on an omitted `Count`, so regenerated goldens must be reviewed as
  behavioral corrections rather than accepted mechanically.
- The compatibility lane requires real ClickHouse/Prometheus infrastructure and
  is the final arbiter for the newly enrolled parity fixture.

## Task List

1. Refactor the test-only `Count` backfill selection in
   `internal/testsql/seed_ddl.go` so exponential histograms derive an omitted
   count from `ZeroCount` and both bucket arrays, without changing classic
   histogram behavior. Verify with focused `internal/testsql` unit coverage.
2. Extend `internal/testsql/backfill_columns_test.go` to pin the derived
   exponential default, classic zero default, and declared-column passthrough.
   Verify the test distinguishes the two table definitions.
3. Add Prometheus parity metadata to
   `test/spec/promql/histogram_quantile_native_agg_at_pinned_range.txtar`, then
   regenerate affected PromQL artifacts with `just update-golden promql`.
   Review every changed native expected row and confirm the pinned fixture is
   `7.21000370088664`.
4. Run `just test` and `just compat-promql`; resolve any seed-shape or parity
   failure before submitting the implementation for review.
