# Pre-GA coverage audit — gap inventory + fill recommendations

Diagnostic-only audit of test coverage gaps as of `main`@`71d28e6`
(2026-05-15). Companion to [`docs/coverage-baseline.md`](coverage-baseline.md),
which establishes the GA-prep baseline number; this doc inventories
the **functions inside** the under-covered packages, prioritizes them
by user-visibility, and maps each to a fill recommendation against
the test-strategy layer map.

This is **not a GA gate** and **not a CI gate**. The
`.github/workflows/coverage.yml` workflow still runs informational
only. The goal of the audit is to give the v1.0.0 cut a prioritized
to-do list, not to set a numeric threshold.

## How this was generated

```bash
# Package-local lane (matches what coverage.yml + just coverage produce).
go test          -coverprofile=cover.out      ./internal/...
go test -tags chdb -coverprofile=cover-chdb.out ./internal/...

# Cross-package lane (true coverage when callers in api/* hit
# functions in chsql/promql/optimizer/logql/traceql).
go test          -coverpkg=./internal/... -coverprofile=cover-true.out      ./internal/...
go test -tags chdb -coverpkg=./internal/... -coverprofile=cover-true-chdb.out ./internal/...

# Mode-set merge per the in-repo `just coverage` recipe.
```

Both views are reported because the gap between them is itself a
signal: a function that appears 0% in the package-local view but
fully covered cross-package is **transitively covered** by handler
tests and doesn't need a direct fill — only its package-local
isolation harness is missing. A function that is 0% in *both* views
is a real, untested code path.

## Per-package coverage snapshot

| Package                   | LoC   | Package-local | Cross-package | Delta  |
| ------------------------- | ----: | ------------: | ------------: | -----: |
| `internal/api/httperr`    | 59    | 100.00%       | 100.00%       | 0.00   |
| `internal/api/format`     | 124   | 100.00%       | 100.00%       | 0.00   |
| `internal/qlcommon`       | -     | 100.00%       | 100.00%       | 0.00   |
| `internal/api/health`     | 181   | 100.00%       | 100.00%       | 0.00   |
| `internal/cerbtrace`      | 97    | 100.00%       | 100.00%       | 0.00   |
| `internal/config`         | 380   | 96.81%        | 96.81%        | 0.00   |
| `internal/api/admit`      | 166   | 93.94%        | 93.94%        | 0.00   |
| `internal/schema`         | 667   | 86.49%        | 100.00%       | +13.51 |
| `internal/api/tempo`      | 1449  | 86.46%        | 86.46%        | 0.00   |
| `internal/api/prom`       | 1609  | 85.11%        | 85.11%        | 0.00   |
| `internal/schema/ddl`     | 315   | 84.75%        | 84.75%        | 0.00   |
| `internal/api/loki`       | 2624  | 84.18%        | 84.18%        | 0.00   |
| `internal/telemetry`      | 585   | 82.71%        | 86.47%        | +3.76  |
| `internal/chplan`         | 1394  | 82.66%        | 83.90%        | +1.24  |
| `internal/promql`         | 2299  | 81.23%        | 82.78%        | +1.55  |
| `internal/optimizer`      | 1855  | 77.80%        | 82.34%        | +4.54  |
| `internal/traceql`        | 1084  | 75.87%        | 75.87%        | 0.00   |
| `internal/chsql`          | 5240  | 74.03%        | 92.20%        | +18.17 |
| `internal/promshim/local` | 383   | 67.19%        | 67.19%        | 0.00   |
| `internal/logql`          | 778   | 62.50%        | 70.83%        | +8.33  |
| `internal/engine`         | 360   | 51.56%        | 90.62%        | +39.06 |
| `internal/chclient`       | 610   | 47.58%        | 49.07%        | +1.49  |
| `internal/chclienttest`   | 617   | 28.16%        | 56.33%        | +28.17 |

**Top-level totals.** Package-local merged: **76.4%** (was 74.7% in
the prior baseline cut). Cross-package merged: **83.7%**. The
package-local view is what CI tracks; the cross-package view is the
"is the code reachable from a test at all?" view.

Five packages have a large package-local vs. cross-package delta
(`internal/engine`, `internal/chsql`, `internal/chclienttest`,
`internal/schema`, `internal/logql`). These are packages whose
functions are mostly exercised through callers in `internal/api/**`
rather than via package-local unit tests. **The cross-package view
is what matters for "is the code path actually run during tests?"**

## Functions truly uncovered (0% in BOTH views)

These 39 functions show no coverage even when handler / engine tests
are credited. They are the real holes.

### Sealed-interface markers — noise, not gaps (no fill)

26 of the 38 "marker" hits are sealed-interface tags
(`isStrategy()` / `isAnalyzerRule()` / `planNode()` / `exprNode()`).
Go's coverage tool counts the empty-body method as one statement,
which never instruments. They are *contracts*, not code paths.
**Action: none. Document in test-strategy as known noise.**

### Real production-code gaps

| Package                   | Function                                              | Pkg-local  | Cross-pkg  | Priority                 | Layer | Why                                                                                                                                         |
| ------------------------- | ----------------------------------------------------- | ---------: | ---------: | ------------------------ | ----- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/logql`          | `binary.go:lowerVectorVector`                         | 0.0%       | 0.0%       | **High**                 | 2b    | Vector ⊗ vector binary ops in metric LogQL. User-facing.                                                                                    |
| `internal/logql`          | `binary.go:vectorMatchingFromOpts`                    | 0.0%       | 0.0%       | **High**                 | 2b    | Translates parser `on()/ignoring()` modifiers into chplan match descriptors. Helper for `lowerVectorVector`.                                |
| `internal/logql`          | `binary.go:sampleShapeOverLogInner`                   | 0.0%       | 0.0%       | **High**                 | 2b    | Wrap-projection synthesizer used by `lowerVectorVector`.                                                                                    |
| `internal/logql`          | `literal.go:lowerVector`                              | 0.0%       | 0.0%       | **High**                 | 2b    | `vector(N)` literal — Loki metric query form.                                                                                               |
| `internal/logql`          | `vector_aggregation.go:buildVectorAggFunc`            | 46.2%      | 46.2%      | **High**                 | 2b    | Switch over agg-op names. Untested branches likely include `stdvar`, `stddev`, `count_values`.                                              |
| `internal/logql`          | `binary.go:logqlBinaryOp`                             | 21.4%      | 21.4%      | **High**                 | 2b    | Parser-op → chplan-op switch. The 78.6% gap = unmapped ops.                                                                                 |
| `internal/api/loki`       | `handler.go:toMatrixStepGrid`                         | 26.7%      | 26.7%      | **High**                 | 7     | Per-step bucketing for metric LogQL `/query_range`. Wire-format-visible. Most paths (cursor advance, lookback drop, multi-series) untested. |
| `internal/api/tempo`      | `handler.go:wrapWithSampleProjection`                 | 36.4%      | 36.4%      | **High**                 | 7     | TraceQL handler wrap. Untested branches likely include the structural-join `R.<col>` path.                                                  |
| `internal/api/tempo`      | `handler.go:splitTraceByIDLabels`                     | 35.7%      | 35.7%      | **High**                 | 7     | `/api/traces/{id}` label split for span-detail enrichment.                                                                                  |
| `internal/api/loki`       | `detected_fields.go:classifyJSON`                     | 56.2%      | 56.2%      | **Medium**               | 7     | `/api/v1/detected_fields` JSON-shape classifier.                                                                                            |
| `internal/api/loki`       | `detected_fields.go:classifyScalar`                   | 69.2%      | 69.2%      | **Medium**               | 7     | Companion to `classifyJSON` for scalar values.                                                                                              |
| `internal/api/loki`       | `detected_fields.go:isBytesLiteral`                   | 50.0%      | 50.0%      | **Medium**               | 7     | Bytes-literal pattern matcher for detected-fields type inference.                                                                           |
| `internal/api/loki`       | `handler.go:classifyEngineErr`                        | 66.7%      | 66.7%      | **Medium**               | 7     | Error-classification for engine result envelopes. Untested branches likely 5xx panics.                                                      |
| `internal/api/prom`       | `handler.go:synthesizedAnchor`                        | 0.0%       | 0.0%       | **Medium**               | 2b/7  | `now64(9) - 5s` anchor stamper. Live in instant range-window projection. Helper, indirectly exercised via `wrapWithSampleProjection`.       |
| `internal/api/prom`       | `metadata.go:truncateMetadata`                        | 0.0%       | 0.0%       | **Medium**               | 7     | Deterministic alphabetic truncation for `/api/v1/metadata?limit=N`.                                                                         |
| `internal/api/prom`       | `metadata.go:fetchLabelValuesMatched`                 | 0.0%       | 0.0%       | **Medium**               | 7     | `/api/v1/labels?match[]=` — multi-matcher union.                                                                                            |
| `internal/api/prom`       | `metadata.go:labelValuesForMatcher`                   | 0.0%       | 0.0%       | **Medium**               | 7     | Helper for `fetchLabelValuesMatched` — single-matcher case.                                                                                 |
| `internal/api/tempo`      | `handler.go:rQualifiedSampleProjections`              | 0.0%       | 0.0%       | **Medium**               | 2b/7  | `R.<col>` projection for structural-join shape.                                                                                             |
| `internal/api/tempo`      | `handler.go:classifySearchErr`                        | 44.4%      | 44.4%      | **Medium**               | 7     | Error classification for `/api/search`.                                                                                                     |
| `internal/api/tempo`      | `handler.go:classifyTraceByIDErr`                     | 57.1%      | 57.1%      | **Medium**               | 7     | Error classification for `/api/traces/{id}`.                                                                                                |
| `internal/api/tempo`      | `handler.go:isAggregateShape`                         | 50.0%      | 50.0%      | **Medium**               | 7     | Aggregate-vs-scalar shape classifier for metrics queries.                                                                                   |
| `internal/api/tempo`      | `metrics_query_range.go:classifyMetricsQueryRangeErr` | 60.0%      | 60.0%      | **Medium**               | 7     | Error classification for `/api/metrics/query_range`.                                                                                        |
| `internal/promql`         | `subquery.go:subqueryAnchor`                          | 33.3%      | 33.3%      | **Medium**               | 2b    | Subquery anchor-expression builder.                                                                                                         |
| `internal/promql`         | `subquery.go:lowerSubqueryInnerMatrix`                | 40.0%      | 40.0%      | **Medium**               | 2b    | Inner subquery → matrix lowering.                                                                                                           |
| `internal/promql`         | `subquery.go:lowerOuterRangeFnOverSubquery`           | 50.0%      | 50.0%      | **Medium**               | 2b    | Outer range-fn over subquery.                                                                                                               |
| `internal/promql`         | `subquery.go:buildSubqueryAggFunc`                    | 50.0%      | 50.0%      | **Medium**               | 2b    | Subquery agg-func builder.                                                                                                                  |
| `internal/promql`         | `subquery.go:lowerSubqueryOverCallSubquery`           | 69.2%      | 69.2%      | **Medium**               | 2b    | Nested-subquery lowering.                                                                                                                   |
| `internal/promql`         | `histogram_quantile.go:lowerHistogramQuantileNative`  | 53.3%      | 53.3%      | **Medium**               | 2b    | Native-histogram quantile lowering.                                                                                                         |
| `internal/promql`         | `histogram_quantile.go:peelWrappers`                  | 60.0%      | 60.0%      | **Medium**               | 2b    | Wrapper-strip helper for histogram_quantile arg.                                                                                            |
| `internal/promql`         | `histogram_quantile.go:unwrapVectorSelector`          | 66.7%      | 66.7%      | **Medium**               | 2b    | Vector-selector extraction helper.                                                                                                          |
| `internal/promql`         | `histogram_quantile.go:andExpr`                       | 50.0%      | 50.0%      | **Medium**               | 2b    | Conjunction-builder for native-histogram predicates.                                                                                        |
| `internal/promql`         | `lower.go:tryStringLiteral`                           | 50.0%      | 50.0%      | **Low**                  | 2b    | String-literal coercion helper.                                                                                                             |
| `internal/promql`         | `lower.go:plainAggCH`                                 | 66.7%      | 66.7%      | **Medium**               | 2b    | Plain aggregate lowering (PromQL `sum`/`avg`/...).                                                                                          |
| `internal/promql`         | `label_fns.go:lowerLabelReplace`                      | 68.4%      | 68.4%      | **Medium**               | 2b    | `label_replace()` lowering.                                                                                                                 |
| `internal/promql`         | `label_fns.go:projectAttributesOverInner`             | 66.7%      | 66.7%      | **Medium**               | 2b    | Attribute-projection helper.                                                                                                                |
| `internal/promql`         | `synthetic.go:isSyntheticScalarPlan`                  | 65.0%      | 65.0%      | **Medium**               | 2b    | Scalar-plan classifier — branches on plan shape.                                                                                            |
| `internal/optimizer`      | `constant_fold.go:foldFloatFloat`                     | 0.0%       | 0.0%       | **High**                 | 4     | Float-float binary-op constant folding. Real, never exercised.                                                                              |
| `internal/optimizer`      | `constant_fold.go:foldIntInt`                         | 21.4%      | 21.4%      | **High**                 | 4     | Int-int binary-op constant folding. Only a few ops covered.                                                                                 |
| `internal/optimizer`      | `constant_fold.go:identityForBool`                    | 37.5%      | 37.5%      | **Medium**               | 4     | Bool-op identity table.                                                                                                                     |
| `internal/optimizer`      | `mv_substitution.go:commutesWith`                     | 33.3%      | 33.3%      | **Medium**               | 4     | Rule-commutation predicate for MV substitution.                                                                                             |
| `internal/traceql`        | `lower.go:flipComparisonOp`                           | 0.0%       | 0.0%       | **High**                 | 2b    | Used when literal is on LHS of binary comparison (`5 > .duration`). User-facing, untested.                                                  |
| `internal/traceql`        | `lower.go:lowerFollowingElement`                      | 41.7%      | 41.7%      | **High**                 | 2b    | TraceQL `>>` (following) descendant operator.                                                                                               |
| `internal/traceql`        | `lower.go:lowerScalarExpr`                            | 50.0%      | 50.0%      | **High**                 | 2b    | Scalar-expression lowering — main dispatcher.                                                                                               |
| `internal/traceql`        | `lower.go:lowerNestedAttrBinary`                      | 47.1%      | 47.1%      | **High**                 | 2b    | Nested-attribute binary lowering (e.g., `.a > .b`).                                                                                         |
| `internal/traceql`        | `lower.go:nestedScopedAttr`                           | 50.0%      | 50.0%      | **High**                 | 2b    | Scoped-attribute resolver for nested expressions.                                                                                           |
| `internal/traceql`        | `lower.go:lowerPipelineElement`                       | 60.0%      | 60.0%      | **High**                 | 2b    | TraceQL pipeline-element dispatcher.                                                                                                        |
| `internal/traceql`        | `lower.go:lowerSpansetExpr`                           | 60.0%      | 60.0%      | **High**                 | 2b    | Spanset-expression dispatcher (`{}` body lowering).                                                                                         |
| `internal/traceql`        | `lower.go:nestedAttrTarget`                           | 62.5%      | 62.5%      | **Medium**               | 2b    | Helper for `nestedScopedAttr`.                                                                                                              |
| `internal/traceql`        | `lower.go:kindString`                                 | 62.5%      | 62.5%      | **Low**                  | 2b    | `kind` intrinsic literal-string mapping.                                                                                                    |
| `internal/traceql`        | `group_coalesce.go:lowerGroup`                        | 66.7%      | 66.7%      | **Medium**               | 2b    | `by()` clause coalescing.                                                                                                                   |
| `internal/chsql`          | `tableshape.go:IsSortColumn`                          | 0.0%       | 0.0%       | **Skip (dead code)**     | —     | No callers in repo. Remove or wire into ORDER BY emitter.                                                                                   |
| `internal/promshim/local` | `sample_store.go:LabelValues`                         | 0.0%       | 0.0%       | **Low**                  | 11    | Property-oracle backing storage. Not on the production request path.                                                                        |
| `internal/promshim/local` | `sample_store.go:LabelNames`                          | 0.0%       | 0.0%       | **Low**                  | 11    | Same — oracle helper.                                                                                                                       |
| `internal/promshim/local` | `sample_store.go:H/FH/Copy`                           | 0.0%       | 0.0%       | **Skip (interface)**     | —     | Histogram methods on Sample iterator; histogram path not yet wired into oracle (already tracked under `coverage-baseline.md` rec #2).       |
| `internal/schema/ddl`     | `ddl.go:applySignal`                                  | 0.0%       | 0.0%       | **Low (integration)**    | 8     | Only exercised under `-tags=integration` with a live testcontainer. Already gated by `just schema-ddl-test`.                                |
| `internal/telemetry`      | `middleware.go:Flush`                                 | 0.0%       | 0.0%       | **Low**                  | 7     | `http.Flusher` shim. Triggered by SSE / chunked-response paths only.                                                                        |
| `internal/chclient`       | `client.go:New/Conn/Close/Ping*/...`                  | 0.0% — 78% | 0.0% — 78% | **Skip (network-bound)** | 8/11  | Network-bound; see `coverage-baseline.md` rationale.                                                                                        |
| `internal/chclient`       | `progress.go:onProgress/flush/withRecorder`           | 0.0%       | 0.0%       | **Low**                  | 11    | Driver-callback paths fired by CH Progress packets. Fake-Conn chaos test would surface them.                                                |

### Mode notes

- All `Name()` accessors on optimizer rules (5 functions in
  `constant_fold.go`, `filter_fusion.go`, `projection_pushdown.go`,
  `rule_pattern.go`) are 0% because their only callers are panic
  messages in the rule dispatcher. Triggering them requires
  constructing an intentionally-malformed batch — value is minimal.
  **Action: leave as-is.**
- `maxIterations` on `analyzerStrategy` is 0% for the same reason.
  Analyzer batches are dispatched through the `Strategy` interface
  but the iteration count is read via a hot-path inlining that the
  coverage tool does not credit to the interface implementation.
  **Action: leave as-is.**

## Top 10 priority fills (ranked, GA recommendation)

1. **LogQL vector-vector binary ops** (5 functions, 0–46% covered).
   Real user-facing surface: `rate(...) / on(job) rate(...)` query
   shape. **Layer 2b lowering** + **Layer 6b chDB roundtrip**. Add a
   table-driven test file `internal/logql/binary_test.go` covering
   `lowerVectorVector`, `vectorMatchingFromOpts`, plus 2-3 TXTAR
   fixtures under `test/spec/logql/binary_vec_vec_*.txtar`. Single
   focused PR.
2. **TraceQL lowering dispatchers** (`lowerScalarExpr`,
   `lowerSpansetExpr`, `lowerPipelineElement`, `lowerFollowingElement`,
   `flipComparisonOp`, 0–60% covered). Branches in the main TraceQL
   lowerer that handle structural-recursive patterns. **Layer 2b** +
   TXTAR fixtures under `test/spec/traceql/`. Pair with Layer 6c
   roundtrips where reasonable.
3. **`api/loki/handler.go:toMatrixStepGrid` (26.7%).** Per-step
   bucketing for `/loki/api/v1/query_range`. Multi-series, lookback
   drop, cursor-advance paths all untested. **Layer 7 conformance
   test**: drop a case into `internal/api/loki/conformance_test.go`
   that feeds a multi-series sample stream through the handler and
   asserts matrix-step pivoting.
4. **`api/prom/metadata.go:fetchLabelValuesMatched` /
   `labelValuesForMatcher` / `truncateMetadata` (0%).**
   `/api/v1/labels?match[]=` is the Grafana label-selector dropdown.
   **Layer 7 conformance** — `internal/api/prom/conformance_test.go`
   case with `match[]` form parameter, asserting union semantics and
   `limit` truncation order.
5. **Optimizer constant-folding** (`foldFloatFloat`,
   `foldIntInt`, 0–21% covered). Constant folding is foundational —
   it's the analyzer-rule shim every other rule runs after.
   **Layer 4 property test**: rapid-generated literal pairs +
   operator matrix, asserting `r := fold(l, op, r)` matches a
   bigint/big.Float oracle. Drop into
   `internal/optimizer/property_extended_test.go`.
6. **PromQL subquery family** (5 functions, 33–69% covered). The
   gaps here line up with `coverage-baseline.md`'s
   "subquery-over-aggregate" callout. **Layer 2b lowering** +
   **Layer 6a chDB roundtrip**. Add fixtures under
   `test/spec/promql/subquery_*.txtar` with `-- seed --` and
   `-- expected_rows --` sections.
7. **PromQL `histogram_quantile` native-histogram path** (4
   functions, 50–67% covered). Native histograms are still
   incomplete. **Layer 2b** + Layer 6a roundtrip with a
   native-histogram-shaped seed.
8. **`api/tempo` error-classification + projection helpers**
   (`classifySearchErr`, `classifyTraceByIDErr`, `isAggregateShape`,
   `wrapWithSampleProjection`, `splitTraceByIDLabels`,
   `rQualifiedSampleProjections`, all 0–57%). **Layer 7
   conformance** — extend `internal/api/tempo/conformance_test.go`
   with cases driving the structural-join and trace-by-id paths.
9. **LogQL `buildVectorAggFunc` + `logqlBinaryOp` switches** (21–46%
   covered). Op-mapping switches with un-exercised cases. **Layer 2b**
   — single table-driven test enumerating every op.
10. **Test-suite hygiene: pin which 0% functions are "noise" so the
    audit doesn't recur.** Add a comment / sentinel to the
    `coverage.yml` step summary so sealed-interface markers and
    network-bound `chclient` entrypoints get filtered out of the
    "below-X%" alert list in future runs. **Documentation-only
    follow-up.**

## Out-of-scope (this audit)

- **`internal/chclient`** — network-bound. Already discussed in
  `coverage-baseline.md`. Lift via `chdb`-tagged chaos tests
  (`progress.go`) and integration-tagged smoke (`client.go:New/Conn`)
  if/when we want it; not a GA gate.
- **`internal/promshim/local` histograms** — already tracked as
  baseline rec #2.
- **`cmd/cerberus`** — entrypoint, expected. Already in baseline.
- **`internal/api/admit` (93.94%)**, **`config` (96.81%)** — already
  high; the remaining gap is unreachable validation branches.

## Total under-covered functions (by view)

| View          | Functions @ 0% | Functions <70% | "Real" gaps (excluding sealed markers + network-bound) |
| ------------- | -------------: | -------------: | -----------------------------------------------------: |
| Package-local | ~100           | ~140           | ~70                                                    |
| Cross-package | 83             | 113            | 39                                                     |

The 39-function "real gaps" list above is the actionable cut.

## What's NOT a gap (calling these out explicitly)

- All `planNode()` / `exprNode()` markers on `chplan.*` types. Sealed
  interface — never called explicitly.
- All `isStrategy()` / `isAnalyzerRule()` / `isOptimizerRule()`
  markers. Same.
- `chplan.<Node>.Equal` accessors at 0% on the `Aggregate`/
  `MetricsAggregate`/etc. types — these *are* called, but only
  during `chplan.Equal()` dispatch from a sibling test, which the
  coverage tool attributes to the dispatch site rather than the
  per-type method. Real coverage exists via
  `internal/chplan/equal_invariants_test.go`.
- `chclient.New` / `Conn` / `Close` / `Ping` / `Query*` family.
  Network-bound; the package's local tests use the breaker + cursor
  fakes, and integration-tagged tests reach the live CH layer.

## Recommended dispatch order

For an agent pool picking up the fills:

1. **Pool-CJ — LogQL vector-vector + literal-vector** (fill #1).
   Smallest, highest user-visibility lift. ~5 functions, one PR.
2. **Pool-CK — TraceQL lowering dispatchers** (fill #2). Five
   functions, table-driven; pair with 6c fixtures where the seeded
   shape lands cleanly.
3. **Pool-CL — Loki matrix step-grid + Prom label-values-matched**
   (fills #3 + #4). Both are handler-conformance; one PR covering
   each handler.
4. **Pool-CJ — Optimizer constant-folding property** (fill #5).
   Single property-test file extension.
5. **(After) Pool-CK — PromQL subquery + histogram_quantile native**
   (fills #6 + #7). Heavier; can be split across two PRs.
6. **Pool-CL — Tempo error / projection helpers** (fill #8).
7. **Final / documentation — pin the noise list** (fill #10).

After fills 1–7 land, the package-local merged baseline should clear
**80%** total and put every production package over **70%** —
sufficient for the GA cut without needing to invent a numeric gate.
