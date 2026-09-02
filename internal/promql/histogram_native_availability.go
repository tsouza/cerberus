package promql

import "github.com/tsouza/cerberus/internal/schema"

// expHistogramLoweringAvailable reports whether this lowering may answer
// from the schema's exponential-histogram table at all. It is the single
// statement of a rule with two independent reasons to say no:
//
//   - The schema declares no exp-histogram table, so there is nothing to
//     read from.
//   - This lowering is a Prometheus metadata full-range walk
//     (`ctx.metadataFullRange`), which must never be answered from the
//     exp-histogram table.
//
// WHY THIS IS A FUNCTION (cerberus issue #2963). The rule used to be
// written out as `s.ExpHistogramTable == "" || ctx.metadataFullRange` at
// 31 sites across 24 files, and nothing kept the 31 in agreement: an edit
// that tightened the condition at one site and missed the other 30 would
// have produced a silent inconsistency in exactly the code path deciding
// whether a query lowers as an exp-histogram. One rule, one statement of
// it, twelve callers — tightening it now means editing this function.
//
// WHERE IT IS CALLED, AND WHY ONLY THERE. The exp-histogram recognizers
// split into two kinds, and only one of them needs to ask:
//
//   - A LEAF recognizer decides a shape by asking the SCHEMA whether the
//     selected metric is an exp-histogram
//     ([schema.Metrics.IsExpHistogramMetric], which consults the metric
//     name and the schema's suffix and reads nothing from `ctx` and
//     nothing from `ExpHistogramTable`). Nothing downstream of a leaf
//     re-derives this rule, so the leaf must apply it itself. The ten
//     leaves are [bareExpHistogramSelector],
//     [bareExpHistogramMatrixSelector], [rangeFnOverExpHistogram],
//     [overTimeOverExpHistogram], [lastFirstOverExpHistogram],
//     [tsOfFirstLastOverExpHistogram], [countPresentOverExpHistogram],
//     [resetsOrChangesOverExpHistogram], [sumOrAvgOverExpHistogram] and
//     [countOverExpHistogram].
//
//   - A COMPOSITE recognizer decides by asking
//     [isExpHistogramValuedShape] or [isExpHistogramDroppingShape] about
//     its operands, and every one of its success paths runs through such
//     an ask. Those two predicates are pure dispatchers: each of their
//     arms either recurses on a STRICT sub-expression or calls a leaf. By
//     induction on AST size, both answer false for every input whenever
//     this function answers false — so a composite copy of the rule
//     decides nothing, and the twenty-one that existed were deleted.
//
// The two remaining callers are [isExpHistogramValuedShape] and
// [isExpHistogramDroppingShape] themselves, where the check is a COST
// gate rather than a correctness one: it is what makes the induction above
// terminate in O(1) instead of by walking the whole AST. Each predicate's
// own doc comment carries the measurement.
//
// That asymmetry is also why the old inline form pinned two mutation legs
// below their floor: gremlins' INVERT_LOGICAL rewrote each `||` to `&&`,
// and at a composite the rewrite was PERMANENTLY equivalent — no test
// could ever kill it — while consuming denominator forever. The rule now
// has one mutation point, in this function's body, and it is killable
// from either direction (see
// gremlins_kill_metadata_full_range_test.go).
func expHistogramLoweringAvailable(s schema.Metrics, ctx lowerCtx) bool {
	return s.ExpHistogramTable != "" && !ctx.metadataFullRange
}
