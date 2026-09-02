// This file documents a gremlins mutant reported LIVED against
// histogram_native_binop.go from a phase4-promql-h mutation run
// (mutation.yml, PR #2727) that is provably EQUIVALENT, not a coverage
// gap — no test is added here, following the same "equivalent, not a gap"
// convention gremlins_kill_histogram_binop_count_test.go's own header
// documents for its sibling file's analogous guards. See
// gremlins_kill_test.go for the shared file-header convention this file
// otherwise follows.
//
//   - histogram_native_binop.go:expHistogramDroppingHistogramBinop:`if s.ExpHistogramTable == "" || ctx.metadataFullRange`, INVERT_LOGICAL (`||` -> `&&`)
//     inside expHistogramDroppingHistogramBinop:
//
//     if s.ExpHistogramTable == "" || ctx.metadataFullRange {
//     return nil, nil, false
//     }
//     ...
//     if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
//     return nil, nil, false
//     }
//
// isExpHistogramValuedShape is a pure dispatcher with no guard of its
// own — it delegates to leaf recognizers (bareExpHistogramSelector,
// sumOrAvgOverExpHistogram, rangeFnOverExpHistogram, and every other
// producer isExpHistogramValuedShape tries), and EVERY one of those
// leaves carries the identical `s.ExpHistogramTable == "" ||
// ctx.metadataFullRange` guard (see e.g. bareExpHistogramSelector,
// histogram_native_bare.go:68). So isExpHistogramValuedShape(expr, s,
// ctx) is unconditionally false for ANY expr whenever ctx.metadataFullRange
// is true and s.ExpHistogramTable is non-empty — exactly the one
// scenario where this function's own early `||`-vs-`&&` guard could
// possibly differ (with an empty ExpHistogramTable the two branches never
// disagree either, since the `==""` operand alone already satisfies both
// `||` and `&&` is never satisfied by ExpHistogramTable alone). With the
// mutant `&&` skipping the early return, execution falls through to
// `!isExpHistogramValuedShape(b.LHS, s, ctx)`, which is unconditionally
// true under metadataFullRange=true regardless of b.LHS — so the `||`
// a few lines down is unconditionally true too, and the function still
// returns ok=false. No input can make the mutant disagree with the
// original: verified directly by applying the mutation by hand and
// confirming `go test ./internal/promql/...` (this package) stays green.
package promql
