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
// gate rather than a correctness one. It does not make the induction above
// terminate — every arm recurses on a strict sub-expression, so it
// terminates either way. What it bounds is the WORK done reaching the
// answer: O(1) instead of a walk that branches once per arm per binary
// node. [isExpHistogramValuedShape]'s doc comment carries the measurement
// for both, since the two predicates share the shape.
//
// That asymmetry is also why the old inline form pinned two mutation legs
// below their floor. The `||` hosted two mutators, and they behaved
// differently: gremlins' INVERT_LOGICAL rewrote it to `&&`, which at a
// composite was PERMANENTLY equivalent — no test could ever kill it —
// while consuming denominator forever; CONDITIONALS_NEGATION rewrote
// `s.ExpHistogramTable == ""` to `!= ""`, which makes the guard fire on
// every configured deployment and was therefore killable everywhere. The
// copies hosted one of each, so deleting them removes killed mutants as
// well as unkillable ones.
//
// What survives is this body's own two mutation points, and both are
// killable: `&&` -> `||` fails 13 tests, `!=` -> `==` fails 210. Neutering
// the function outright — `return true`, `return false` — fails those same
// two sets, and [TestExpHistogramRecognizersRejectWhenLoweringUnavailable]
// is among the failures in all four cases, so it alone catches every one.
// See gremlins_kill_metadata_full_range_test.go for the per-leaf pins.
func expHistogramLoweringAvailable(s schema.Metrics, ctx lowerCtx) bool {
	return s.ExpHistogramTable != "" && !ctx.metadataFullRange
}
