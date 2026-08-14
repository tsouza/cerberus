# Spec: exponential-histogram interpolation ULP parity

Tracks issue #2024. Planning only; implementation awaits owner approval of the task list.

## Problem

Seven existing PromQL exponential-histogram fixtures cannot be enrolled in the reference parity
layer because their answers differ from Prometheus by 1-5 ULP in the final bits:

| Fixture                                       | Maximum measured distance |
| --------------------------------------------- | ------------------------- |
| `histogram_fraction_exp`                      | 2 ULP                     |
| `histogram_fraction_exp_negative_bounds`      | 1 ULP                     |
| `histogram_fraction_exp_negative_range`       | 2 ULP                     |
| `histogram_quantile_native_latest_sample`     | 5 ULP                     |
| `histogram_quantile_native_bare_offset_range` | 5 ULP                     |
| `histogram_quantile_native_multi_series`      | 1 ULP                     |
| `histogram_quantile_native_negative_p50`      | 3 ULP                     |

The issue records the exact reference and Cerberus values. The implementation explains the source:
`internal/promql/histogram_value_fns.go` calculates fraction ranks with ClickHouse `log2`, and
`internal/chsql/histogram_quantile_native.go` calculates native-bucket bounds with ClickHouse `pow`.
Prometheus evaluates the corresponding exponential interpolation with Go's `math` package. Separate
correctly rounded libm implementations need not select the same final `float64`.

The current parity runner, `test/spec/parity_chdb.go`, uses exact equality except for the documented
one-ULP `atan2` exception. As a result, the seven fixtures lack their hand-authored `-- parity --`
contracts despite otherwise having deterministic seeds and expected rows.

## Scope

Included:

- A named, measured comparator for float results of native exponential-histogram interpolation.
- Automatic selection only for an enrolled fixture whose seed targets
  `otel_metrics_exponential_histogram` and whose query invokes `histogram_fraction` or
  `histogram_quantile`.
- Enrolment of the seven issue fixtures with the existing full Prometheus oracle contract.

Excluded:

- Production lowering, ClickHouse SQL, schema, and response-path changes.
- Any general epsilon or tolerance applied to ordinary PromQL values, classic histograms, or other
  transcendental functions.
- Changes to `atan2ULPTolerance`, its comparator, or its query classifier.
- Generated-artifact edits by hand.

## Design

Keep production pushdown intact. Reimplementing Go's `math.Log2` and `math.Pow` bit-for-bit in
ClickHouse SQL is neither a reliable parity mechanism nor consistent with the pushdown architecture.

Add `EqualExponentialHistogramInterpolationValues` to
`test/spec/parityoracle/promql/oracle.go`. It will retain the existing NaN behavior, use the existing
sign-safe ULP-distance helper, and accept no more than a named five-ULP bound. Five is the smallest
bound covering the issue's measured corpus; a value six ULP away must fail.

Replace the single `atan2CompareValues` selector in `test/spec/parity_chdb.go` with a comparator
selector that receives the fixture and query. It will select the new comparator only when the complete
query is `histogram_fraction` or `histogram_quantile`, its histogram-data argument has at least one
selector and every such selector uses the exponential-histogram suffix, and the seed creates
`otel_metrics_exponential_histogram`. `atan2` retains its current one-ULP rule. Every other value
comparison remains `EqualValues` exact. Requiring a complete interpolation result prevents the
relaxation from reaching unrelated samples in a composed expression; checking the data argument keeps
a classic `histogram_quantile` exact even when the fixture seeds both table types.

Add direct oracle tests for all seven reference/Cerberus pairs from #2024, symmetry, the five-ULP
boundary, six-ULP rejection, NaN behavior, and exact-equality negative controls. Add selector tests
covering the positive native cases and negative controls for classic quantiles with a mixed-table
seed, a native quantile composed with an unrelated vector, ordinary `histogram_fraction`, unrelated
`pow`/`log2` expressions, and `atan2` to prove the exceptions do not overlap or broaden one another.

Finally add the standard full Prometheus `-- parity --` section to each of the seven existing TXTAR
fixtures. These contracts are hand-authored test inputs, not a tolerance field; live reference parity
continues to compute both answers and enforces the named comparator only for the narrow expression
and table class above.

## Verification

1. Run the focused parity-oracle unit tests in `test/spec/parityoracle/promql`, including all seven
   issue pairs, the five-ULP boundary, and the six-ULP rejection.
2. Run the focused `test/spec` selector tests, confirming only native exponential-histogram
   `histogram_fraction` and `histogram_quantile` queries receive the five-ULP comparator.
3. Run the chDB PromQL spec/parity lane for the seven enrolled fixtures. It must query real
   Prometheus through the existing parity oracle, accept each documented 1-5 ULP result, and still
   reject the negative controls. This requires `libchdb.so` and the reference-oracle test substrate.
4. Run `just update-golden promql` after the fixture contracts are added, then review the generated
   diff under `test/spec/` and `test/perf/`. Do not hand-edit any generated output.
5. Run `just compat-promql` against real ClickHouse and Prometheus. The differential harness must
   report full parity and the parity ratchet must accept the enlarged enrolled corpus.

## Risks

- A fixture can seed both histogram tables, and a composed result can contain native-interpolation and
  unrelated samples. Requiring a top-level interpolation call and exponential-only selectors in its
  data argument prevents either false scope expansion.
- Five ULP is larger than the existing `atan2` bound. The bound is justified solely by the seven
  measured seeds and is pinned by a six-ULP rejection test; any newly observed distance above five is
  a failure requiring investigation, not an automatic widening.
- The selector is test-only, but a classifier bug could mask a production arithmetic regression.
  Positive and negative selector tests, exact defaults, and real-reference execution mitigate this.
- The real compatibility run requires Docker, ClickHouse, and Prometheus; it cannot be established
  from chDB alone.

## Task List For Owner Review

Ordered so the tree is green after every task.

1. Add the named five-ULP exponential-histogram interpolation comparator and focused oracle tests in
   `test/spec/parityoracle/promql/oracle.go` and a new or extended adjacent test file. Verify all
   seven issue values, symmetry, NaN behavior, the five-ULP boundary, six-ULP rejection, and that
   `EqualValues` remains exact.
2. Change the parity comparator selection in `test/spec/parity_chdb.go` to use the query function
   plus the fixture's exponential-histogram seed table; add focused selector tests. Verify native
   `histogram_fraction` and native `histogram_quantile` select the new comparator, while classic
   quantiles, non-histogram `pow`/`log2`, and `atan2` retain their existing comparators.
3. Add the full Prometheus `-- parity --` contract to the seven named fixtures in
   `test/spec/promql/`. Verify their focused chDB reference-parity cases execute against the live
   reference and pass at their measured distances.
4. Regenerate required PromQL-derived artifacts with `just update-golden promql`; review every
   generated change and retain only expected fixture/performance updates. Verify the default spec
   lane and the chDB parity lane remain green.
5. Run `just compat-promql` and confirm the real ClickHouse/Prometheus differential report has no
   diffs and the Prometheus parity-ratchet roster is complete. Update a generated compatibility
   baseline only through its documented synchronizer if the harness produces a required roster delta.
