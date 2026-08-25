package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_range_fn.go composes the SELECT/FOLD
// family of range-vector functions — count_over_time, present_over_time,
// last_over_time, first_over_time, resets, changes, ts_of_first_over_time,
// ts_of_last_over_time, rate, increase, delta, irate, idelta,
// sum_over_time, avg_over_time — over a subquery whose own inner is a
// MIXED float/histogram `or` shape ([chplan.MixedRowShape], cerberus issue
// #2330's family) rather than a PURE histogram-native one (cerberus issue
// #2577, the mixed-or-inner sibling of #2545/#2569's pure-histogram-inner
// composition — [selectFnOverExpHistogramSubquery] /
// [rangeFnOverExpHistogramSubquery], histogram_native_subquery_select.go /
// histogram_native_range_fn.go).
//
// Reference Prometheus evaluates `<fn>(((a) or (b))[5m:1m])` by
// re-evaluating `(a) or (b)` at every subquery anchor and folding the
// resulting per-series Matrix — the SAME window reduction it applies to a
// bare selector's matrix, type-blind to whether a given series' samples
// are float or histogram. Because `or`'s default match key spans every
// label including `__name__`, and the two arms here are drawn from
// different metrics, no series is ever produced by both arms: each
// series' own window is homogeneously float-valued (from `b`) or
// homogeneously histogram-valued (from `a`). Under that (the only sane,
// and only reachable, shape — see [mixedExpHistogramSetOp]'s own doc for
// why a genuine same-series type flip is unreachable from cerberus's
// disjoint float/histogram table split) the outer function DISTRIBUTES
// over the union:
//
//	<fn>(((a) or (b))[5m:1m])  ==  <fn>((a)[5m:1m]) or <fn>((b)[5m:1m])
//
// which is exactly [lowerMixedExpHistogramSetOp]'s own algebraic shape,
// one level up. This file exploits that identity rather than teaching
// every one of the fifteen functions' own window-fold machinery to read a
// [chplan.MixedRowShape] input directly (which would additionally require
// each of them to publish a MIXED output for the nine names whose result
// preserves the operand's own value type — last_over_time, first_over_time,
// rate, increase, delta, irate, idelta, sum_over_time, avg_over_time):
// [mixedOrSubqueryOuterFn] recognises the shape and
// [lowerMixedOrSubqueryOuterFn] rewrites it into two synthetic
// `<fn>(<subquery over one arm>)` calls, lowers each through the ORDINARY
// `lower()` dispatcher (which already answers `<fn>(<histogram-native
// subquery>)` via #2569's own recognizers, and `<fn>(<float subquery>)`
// via the pre-existing, wholly unrelated float path), and only then
// decides how to recombine them:
//
//   - The six names whose result is always a plain FLOAT sample
//     regardless of the operand's own value type (count_over_time,
//     present_over_time, resets, changes, ts_of_first_over_time,
//     ts_of_last_over_time) recombine through the ordinary, non-Mixed
//     `or` path — both synthetic arms lower Sample-shaped, so
//     [mixedExpHistogramSetOp] does not match the synthetic `or` and
//     [lowerMixedOrSubqueryOuterFn] falls through to the plain [lower]
//     dispatcher, identical to any other float `or`.
//   - The remaining nine names preserve the operand's own value type
//     (last_over_time / first_over_time select a published sample
//     verbatim; rate / increase / delta / irate / idelta / sum_over_time /
//     avg_over_time fold a window of histograms into one). Their
//     histogram-arm synthetic call lowers Histogram-shaped, so
//     [mixedExpHistogramSetOp] matches the synthetic `or` and
//     [lowerMixedOrSubqueryOuterFn] routes through
//     [lowerMixedExpHistogramSetOp] directly — the SAME construction
//     [mixedExpHistogramSetOp] itself builds for a bare `(a) or (b))`, one
//     level up.
//
// Deliberately narrower than #2569's own pure-histogram-inner sibling in
// one respect: this recognizer only admits sub.Expr shaped exactly as
// [mixedExpHistogramSetOp] itself matches — a BARE `X or Y` with exactly
// one side histogram-valued (or forwarded through and/unless, cerberus
// issue #2571). A subquery inner that is some OTHER mixed-or composition
// (wrapped in sum()/avg(), label_replace/label_join, or nested under a
// further and/or/unless — histogram_native_mixed_or_aggregate.go /
// _label.go / [mixedExpHistogramSetOp]'s own doc) stays unrecognised here
// and continues to reject through [lowerOuterRangeFnOverSubquery]'s
// existing guard, exactly as it did before this file existed.
func mixedOrSubqueryOuterFn(c *parser.Call, s schema.Metrics, ctx lowerCtx) (*parser.SubqueryExpr, *parser.BinaryExpr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false
	}
	if len(c.Args) != 1 || !isHistogramSubqueryOuterFnName(c.Func.Name) {
		return nil, nil, false
	}
	sub, ok := peelWrappers(c.Args[0]).(*parser.SubqueryExpr)
	if !ok || sub.Range <= 0 || !subqueryHasEvalAnchor(sub, ctx) {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(sub.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return sub, b, true
}

// isHistogramSubqueryOuterFnName reports whether name is one of the
// fifteen SELECT/FOLD-family names [mixedOrSubqueryOuterFn] and its
// pure-histogram-inner sibling ([selectFnOverExpHistogramSubquery] /
// [rangeFnOverExpHistogramSubquery]) both admit — cerberus issues #2545 /
// #2569 named the vocabulary; this file only widens which subquery INNER
// shape composes with it.
func isHistogramSubqueryOuterFnName(name string) bool {
	switch name {
	case countOverTimeWindowFn, presentOverTimeWindowFn,
		lastOverTimeWindowFn, firstOverTimeWindowFn,
		resetsWindowFn, changesWindowFn,
		tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn,
		rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn,
		sumOverTimeWindowFn, avgOverTimeWindowFn:
		return true
	default:
		return false
	}
}

// lowerMixedOrSubqueryOuterFn lowers the shape [mixedOrSubqueryOuterFn]
// recognised: c applied to a subquery whose own inner is b, a mixed
// float/histogram `or`. It rewrites the expression into
// `<fn>(<subquery over b.LHS>) or <fn>(<subquery over b.RHS>)` — a
// synthetic AST, never seen by the parser — and hands it to the SAME
// recombination logic [mixedExpHistogramSetOp] /
// [lowerMixedExpHistogramSetOp] use for a bare mixed `or`, deriving
// "which side is histogram" the identical way: from the ACTUAL
// [chplan.RowShapeOf] of each arm's own lowering, not a second static
// judgement that could disagree with the first. b.LHS / b.RHS keep their
// SOURCE order (not reordered by which side is histogram) so a genuine
// same-series overlap between the two arms resolves through the shadow
// rule exactly as the un-rewritten expression would.
func lowerMixedOrSubqueryOuterFn(c *parser.Call, sub *parser.SubqueryExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	synthetic := &parser.BinaryExpr{
		Op:             parser.LOR,
		LHS:            &parser.Call{Func: c.Func, Args: parser.Expressions{subqueryWithExpr(sub, b.LHS)}},
		RHS:            &parser.Call{Func: c.Func, Args: parser.Expressions{subqueryWithExpr(sub, b.RHS)}},
		VectorMatching: b.VectorMatching,
	}
	if mb, ok := mixedExpHistogramSetOp(synthetic, s, ctx); ok {
		return lowerMixedExpHistogramSetOp(mb, s, ctx)
	}
	return lower(synthetic, s, ctx)
}

// subqueryWithExpr copies sub with its own Expr replaced by expr — used to
// build the two single-arm synthetic subqueries
// [lowerMixedOrSubqueryOuterFn] folds the outer function over. sub's
// remaining fields (Range, Step, Timestamp, StartOrEnd, Offset, …) carry
// over unchanged: both arms share the SAME grid the original mixed-or
// subquery would have used.
func subqueryWithExpr(sub *parser.SubqueryExpr, expr parser.Expr) *parser.SubqueryExpr {
	cp := *sub
	cp.Expr = expr
	return &cp
}
