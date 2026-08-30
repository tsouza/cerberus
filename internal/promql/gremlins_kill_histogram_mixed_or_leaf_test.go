// This file documents a gremlins mutant reported LIVED against
// histogram_native_mixed_or.go from a phase4-promql-h mutation run
// (mutation.yml, PR #2727) that is provably EQUIVALENT, not a coverage
// gap — no test is added here, following the same "equivalent, not a gap"
// convention gremlins_kill_histogram_binop_count_test.go's own header
// documents for its sibling file's analogous guards. Distinct from
// gremlins_kill_histogram_mixed_or_test.go, which covers
// histogram_native_mixed_or.go's siblings (histogram_native_mixed_or_label.go,
// histogram_native_mixed_or_vector_comparison.go,
// histogram_native_value_producing_call.go), not this ROOT-only leaf
// recognizer itself. See gremlins_kill_test.go for the shared file-header
// convention this file otherwise follows.
//
//   - histogram_native_mixed_or.go:123:31, INVERT_LOGICAL (`||` -> `&&`)
//     inside mixedExpHistogramSetOp:
//
//     if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//     return nil, false
//     }
//     ...
//     lhsHist := isExpHistogramValuedOrForwarded(b.LHS, s, ctx)
//     rhsHist := isExpHistogramValuedOrForwarded(b.RHS, s, ctx)
//     if lhsHist == rhsHist {
//     return nil, false
//     }
//
// isExpHistogramValuedOrForwarded(expr, s, ctx) is
// `isExpHistogramValuedShape(expr, s, ctx) ||
// isExpHistogramForwardedThroughSetOp(expr, s, ctx)`
// (histogram_native_set_op.go). isExpHistogramValuedShape is a pure
// dispatcher with no guard of its own, delegating entirely to leaf
// recognizers (bareExpHistogramSelector and every other producer it
// tries) that EACH carry the identical `s.ExpHistogramTable == "" ||
// ctx.metadataFullRange` guard — so isExpHistogramValuedShape(expr, s,
// ctx) is unconditionally false for ANY expr whenever ctx.metadataFullRange
// is true and s.ExpHistogramTable is non-empty.
// isExpHistogramForwardedThroughSetOp recurses purely through
// isExpHistogramValuedShape (also unconditionally false under the same
// condition) or itself, bottoming out at a non-`and`/`unless` node with a
// plain `false` — so it, too, is unconditionally false under
// metadataFullRange=true. Both lhsHist and rhsHist therefore collapse to
// `false` whenever ctx.metadataFullRange is true, making `lhsHist ==
// rhsHist` (false == false) unconditionally true — the SAME rejection the
// mutated `&&` would have skipped past. So the later, independent
// lhsHist/rhsHist check rejects every input the mutant's skipped early
// return would otherwise have let through, regardless of what b.LHS/b.RHS
// actually are. No input can make the mutant disagree with the original:
// verified directly by applying the mutation by hand and confirming
// `go test ./internal/promql/...` (this package) stays green.
package promql
