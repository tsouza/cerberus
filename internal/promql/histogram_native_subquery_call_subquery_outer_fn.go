package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_native_subquery_call_subquery_outer_fn.go answers cerberus
// issue #2728: an outer range-vector function wrapping cerberus issue
// #2726's doubly-nested subquery composition DIRECTLY, with no bracket of
// its own — `<outer-fn2>(<fn>(<inner-sub>)[<outer-range>:<step>])`, a
// THIRD level of nesting, canonical example
// `sum_over_time(rate(((<exp-hist>) or (<gauge>))[3m:1m])[4m:1m])`.
//
// [lowerHistogramOrMixedSubqueryOuterFnInput] used to refuse this shape
// outright (falling through to the pre-#2726 float-only-drop / reject
// path). Nothing about the REDUCTION needed new machinery — every one of
// the fifteen SELECT/FOLD-family continuations folds "whatever samples
// sub's range vector holds", and for this shape those samples are the
// doubly-nested composition's own per-outer-anchor rows. Only the GRID
// did: [lowerSubqueryOverCallSubquery] deliberately leaves re-anchoring
// to its caller (see its own doc), so its output arrives anchored on
// sub's bracket alone and has to be re-anchored onto the enclosing
// reduction's window first.
//
// [widenNestedCallSubqueryInner] is that re-anchoring, and it is the
// same three-branch shape [lowerOuterRangeFnOverSubquery] already applies
// to the plain-float sibling of this exact composition.

// histogramSubqueryOuterFnName reports whether windowFn is one of the
// fifteen SELECT/FOLD-family names
// [lowerHistogramOrMixedSubqueryOuterFnInput]'s own switch answers.
// Kept as its own predicate rather than inlined into that switch because
// the triple-nesting arm has to know the answer BEFORE dispatching: its
// widening mutates the input relation, and an unmatched name must reach
// the caller's fallback with that relation untouched.
func histogramSubqueryOuterFnName(windowFn string) bool {
	switch windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn,
		tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn,
		lastOverTimeWindowFn, firstOverTimeWindowFn,
		resetsWindowFn, changesWindowFn,
		rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn,
		sumOverTimeWindowFn, avgOverTimeWindowFn:
		return true
	default:
		return false
	}
}

// widenNestedCallSubqueryInner re-anchors a doubly-nested composition's
// own outer-subquery grid onto the window an EVEN OUTER range-vector
// function reduces over, across the three grid modes
// [lowerOuterRangeFnOverSubquery] distinguishes — and it thread-for-
// thread mirrors that function's own three branches, because `inner`
// here plays exactly the role `inner` plays there: the relation whose
// anchors the enclosing reducer's per-anchor `(t - Offset - sub.Range,
// t - Offset]` window reads.
//
//   - `@`-pinned under query_range: the pin fixes the WHOLE subquery
//     evaluation, so the grid stays on the pinned window and the
//     continuation broadcasts the single per-series answer across the
//     step grid itself.
//   - query_range: the grid spans `[ctx.start - Offset - sub.Range,
//     ctx.end]`, the union of every output step's own window.
//   - instant: the grid spans that same window around the single
//     eval anchor.
//
// [widenSubquerySpine] does the walking, and its two gates are what make
// this safe on a plan this deep. Its `RangeBucketFanout` arm re-anchors
// only a fan-out already in OuterRange mode — which is precisely the one
// [buildOuterRangeSubqueryFanout] built for sub's own bracket — and
// leaves every CLASSIC ambient-grid fan-out alone. Those classic
// fan-outs are the ones [lowerSubqueryOverCallSubquery]'s wideInner is
// built from, and they were already gridded correctly at their own
// lowering time: it hands `lowerSubquery` a widened
// `innerSub.Range + sub.Range` copy under the ambient ctx, so
// [subqueryGridCtx] resolved them over `[ctx.start - sub.Range -
// innerSub.Range, ctx.end]` in range mode — exactly the span every
// re-anchored outer anchor's lookback needs. Re-gridding them here would
// overwrite that with values derived from an unrelated step/lookback.
// Its `VectorSetOp` arm walks BOTH arms, which is what reaches the
// histogram and float halves of a MixedRowShape composition.
func widenNestedCallSubqueryInner(inner chplan.Node, sub *parser.SubqueryExpr, ctx lowerCtx) error {
	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return err
	}
	switch {
	case ctx.rangeMode() && subqueryPinned(sub) && !anchor.End.IsZero():
		widenSubquerySpine(inner, anchor.End.Add(-anchor.Offset-sub.Range), anchor.End)
	case ctx.rangeMode():
		// NOT gated on a resolvable anchor.End: [subqueryAnchor] fills End
		// from `@` or the ambient eval instant, and an unpinned
		// query_range sub has neither, so gating here would silently skip
		// the widening every output step but the last depends on — every
		// earlier step would then reduce an empty window.
		widenSubquerySpine(inner, ctx.start.Add(-anchor.Offset-sub.Range), ctx.end)
	case !anchor.End.IsZero():
		widenSubquerySpine(inner, anchor.End.Add(-anchor.Offset-sub.Range), anchor.End)
	}
	// A bare [Lower] with no query time at all resolves no anchor and no
	// range: nothing to widen against, and the continuations below fall
	// back to their own now64() shapes exactly as they do for a
	// singly-nested sub.
	return nil
}
