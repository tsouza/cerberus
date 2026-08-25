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
// Widened by cerberus issue #2581 to also admit sub.Expr shaped as
// label_replace/label_join DIRECTLY wrapping [mixedExpHistogramSetOp]'s own
// bare shape — `<fn>((label_replace((a) or (b), ...))[range:step])` — via
// [wrapMixedOrSubqueryInner]'s rebuild closure, below. Still narrower than
// #2569's own pure-histogram-inner sibling in two respects, both
// deliberate, not oversights:
//
//   - sum()/avg() [by/without] wrapping the mixed `or`
//     (histogram_native_mixed_or_aggregate.go) — see
//     [wrapMixedOrSubqueryInner]'s own doc for why that wrapper cannot be
//     admitted through this same distribute-then-recombine mechanism
//     without silently disagreeing with reference on a colliding group.
//   - A further `and`/`or`/`unless` wrapping the mixed `or`
//     ([mixedExpHistogramSetOp]'s own doc names this as the THIRD wrapper
//     family, cerberus issues #2346/#2449/#2555). Investigated for THIS
//     file's own composition and found already fully accounted for by
//     cerberus issue #2589's fix (PR #2597), needing no new recognizer
//     here at all:
//     TestFurtherSetOpRHS_AlreadyComposes
//     (histogram_native_mixed_or_subquery_further_setop_test.go) proves
//     the mixed `or` on the RIGHT of `and`/`unless` already composes —
//     `and`/`unless` forward the LEFT operand's row shape unconditionally,
//     so the subquery inner is always plain float regardless of the mixed
//     `or`'s own type. TestFurtherSetOpLHS_CleanlyRejects proves the mixed
//     `or` on the LEFT still rejects cleanly, for the identical reason PR
//     #2597's own TestOuterRangeFnOverAndUnlessMixedSubquery_CleanRejection
//     (subquery_and_unless_mixed_histogram_outer_test.go) already pins for
//     a bare-selector `and`/`unless`-forwarded histogram subquery inner —
//     none of the fifteen SELECT/FOLD-family names has a dedicated
//     recognizer for an `and`/`unless`-FORWARDED (as opposed to directly)
//     histogram-valued subquery inner, and widening
//     [rangeFnOverExpHistogramSubquery] / [selectFnOverExpHistogramSubquery]
//     to add one would reverse that already-merged decision rather than
//     extend it. A further `or` (either order) rejects too — `or`, unlike
//     `and`/`unless`, genuinely propagates the mixed `or`'s own type, so
//     there is no plain-float escape hatch (TestFurtherSetOpOr_CleanlyRejects).
//
// Both stay unrecognised here and continue to reject through
// [lowerOuterRangeFnOverSubquery]'s existing guard. Tracked as an open
// divergence under #2581 in test/rejection-parity/catalogue.
func mixedOrSubqueryOuterFn(c *parser.Call, s schema.Metrics, ctx lowerCtx) (*parser.SubqueryExpr, *parser.BinaryExpr, func(parser.Expr) parser.Expr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, nil, false
	}
	if len(c.Args) != 1 || !isHistogramSubqueryOuterFnName(c.Func.Name) {
		return nil, nil, nil, false
	}
	sub, ok := peelWrappers(c.Args[0]).(*parser.SubqueryExpr)
	if !ok || sub.Range <= 0 || !subqueryHasEvalAnchor(sub, ctx) {
		return nil, nil, nil, false
	}
	b, rebuild, ok := wrapMixedOrSubqueryInner(sub.Expr, s, ctx)
	if !ok {
		return nil, nil, nil, false
	}
	return sub, b, rebuild, true
}

// wrapMixedOrSubqueryInner recognises inner — a subquery's own Expr — as
// EITHER a bare mixed float/histogram `or` ([mixedExpHistogramSetOp]'s own
// shape) OR that same shape wrapped in a DIRECT label_replace/label_join
// call ([labelCallOverMixedExpHistogramSetOp]'s shape, cerberus issue
// #2449) — the first of the three wrapper families
// histogram_native_mixed_or.go's own doc comment names that cerberus issue
// #2581 threads through this file's subquery composition. rebuild takes one
// of b's two operands and re-applies whatever wrapper matched (identity for
// the bare case, the SAME label_replace/label_join call with its first
// argument replaced for the wrapped case) — [lowerMixedOrSubqueryOuterFn]
// calls it once per synthetic arm so each arm keeps the wrapper the
// original expression had.
//
// label_replace/label_join is a sound choice for this distribute-then-
// recombine mechanism because it is a per-ROW rewrite that never reads or
// combines across rows: `<fn>((label_replace((a) or (b), args))[5m:1m])`
// distributes to `<fn>((label_replace(a, args))[5m:1m]) or
// <fn>((label_replace(b, args))[5m:1m])` for the identical reason the bare
// (unwrapped) case already does — inserting a stateless label rewrite
// inside each per-anchor evaluation commutes with both the subquery's own
// per-anchor re-evaluation and the outer window fold. `sum`/`avg`
// [by/without] (histogram_native_mixed_or_aggregate.go, cerberus issue
// #2346) is deliberately NOT admitted here despite being the OTHER
// wrapper family with an existing root-only recognizer: distributing a
// GROUPING aggregate per arm and recombining with `mixedExpHistogramSetOp`'s
// ordinary shadow-priority `or` (LHS wins on a shared group) does not
// reproduce reference's own `sum`/`avg` semantics — a group that receives
// contributions from BOTH the histogram arm and the float arm at the SAME
// subquery anchor must be DROPPED entirely (a
// `MixedFloatsHistogramsAggWarning`, [combineMixedAggregateBranches]'s own
// doc), not resolved by picking one side, and a `series`-only (or any
// non-injective) `by`/`without` clause can absolutely put histogram- and
// float-typed rows from the two DIFFERENT source metrics into the same
// group despite the two metrics never sharing a full label signature. A
// correct composition needs the outer window fold to run directly over the
// already-per-anchor-correct Mixed relation [lowerHistogramNativeSubqueryInner]
// already builds via [lowerSumOrAvgOverMixedExpHistogramSetOp], not a
// second, independent per-arm aggregation — real new machinery, not a cheap
// extension of this file's existing mechanism, so it stays an open
// divergence under cerberus issue #2581 rather than a silently-wrong
// recognizer.
func wrapMixedOrSubqueryInner(inner parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, func(parser.Expr) parser.Expr, bool) {
	if b, ok := mixedExpHistogramSetOp(inner, s, ctx); ok {
		return b, func(operand parser.Expr) parser.Expr { return operand }, true
	}
	if call, b, ok := labelCallOverMixedExpHistogramSetOp(inner, s, ctx); ok {
		return b, func(operand parser.Expr) parser.Expr {
			rebuilt := *call
			args := append(parser.Expressions(nil), call.Args...)
			args[0] = operand
			rebuilt.Args = args
			return &rebuilt
		}, true
	}
	return nil, nil, false
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
// float/histogram `or`, optionally wrapped by rebuild (identity for the
// bare case; re-applies label_replace/label_join for the wrapped case,
// cerberus issue #2581 — see [wrapMixedOrSubqueryInner]'s own doc). It
// rewrites the expression into `<fn>(<subquery over
// rebuild(b.LHS)>) or <fn>(<subquery over rebuild(b.RHS)>)` — a synthetic
// AST, never seen by the parser — and hands it to the SAME recombination
// logic [mixedExpHistogramSetOp] / [lowerMixedExpHistogramSetOp] use for a
// bare mixed `or`, deriving "which side is histogram" the identical way:
// from the ACTUAL [chplan.RowShapeOf] of each arm's own lowering, not a
// second static judgement that could disagree with the first. b.LHS / b.RHS
// keep their SOURCE order (not reordered by which side is histogram) so a
// genuine same-series overlap between the two arms resolves through the
// shadow rule exactly as the un-rewritten expression would.
func lowerMixedOrSubqueryOuterFn(c *parser.Call, sub *parser.SubqueryExpr, b *parser.BinaryExpr, rebuild func(parser.Expr) parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	synthetic := &parser.BinaryExpr{
		Op:             parser.LOR,
		LHS:            &parser.Call{Func: c.Func, Args: parser.Expressions{subqueryWithExpr(sub, rebuild(b.LHS))}},
		RHS:            &parser.Call{Func: c.Func, Args: parser.Expressions{subqueryWithExpr(sub, rebuild(b.RHS))}},
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
