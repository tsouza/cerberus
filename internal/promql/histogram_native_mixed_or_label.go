package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_label.go lowers `label_replace`/`label_join`
// directly wrapping a mixed float/histogram `or`
// (histogram_native_mixed_or.go's #2330/#2335 shape) — cerberus issue
// #2449, the first NON-aggregation wrapper family to compose over that
// shape since cerberus issue #2346 taught `sum`/`avg` to
// (histogram_native_mixed_or_aggregate.go).
//
// Why this composition needed no new reduction machinery, unlike #2346's:
// `sum`/`avg` had to decompose the mixed `or` into two independently
// reduced branches (a native-histogram SUM/AVG, a float SUM/AVG) and
// recombine them with reference's own "drop a mixed-type output group"
// rule, because summing floats and summing histograms are genuinely
// different SQL machinery. A label rewrite touches ONLY the Attributes
// column — every row's Attributes rewrites identically whether that row
// carries a real Value/placeholder-histogram payload or a real-histogram/
// placeholder-Value payload, because the rewrite never reads the payload
// at all. So this file's lowering does not decompose the mixed `or`; it
// routes the SAME [chplan.VectorSetOp] node [lowerMixedExpHistogramSetOp]
// builds for the root-only leaf case through [projectAttributesOverInner]
// (label_fns.go), which histogram_shape_guard.go's doc comment and this
// change teach a [chplan.MixedRowShape] branch: forward all thirteen
// other columns (the quartet's MetricName/Timestamp/Value, the nine
// Histogram*Column outputs, and the trailing discriminator) unchanged,
// rewrite only Attributes. No `chplan.Case` / CH `if()` keyed on the
// discriminator is needed for THIS wrapper family, because Attributes
// does not vary by payload — that per-row conditional remains what
// `projectValueOverInner`'s (still unimplemented) MixedRowShape branch
// would need, since a value transform genuinely differs by payload.
//
// Deliberately its own sibling recognizer, not a widening of
// [mixedExpHistogramSetOp]'s own registration: that leaf recognizer
// stays root-only (its own doc comment's "impossible state" argument for
// [assertValueShapedInput] still holds for every wrapper this file does
// NOT recognise), and every OTHER wrapper around a mixed `or` (`abs(a or
// b)`, `(a or b) + 1`, and so on) still falls through to
// internal/promql/binary.go's lowerVectorSetOp rejection unchanged — see
// test/rejection-parity/catalogue's tracking entry for that site, still
// open under this same issue.
func labelCallOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, false
	}
	switch call.Func.Name {
	case fnLabelReplace:
		if len(call.Args) != 5 {
			return nil, nil, false
		}
	case fnLabelJoin:
		if len(call.Args) < 3 {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

// lowerLabelCallOverMixedExpHistogramSetOp lowers the shape
// [labelCallOverMixedExpHistogramSetOp] recognised. See this file's
// header for why no bespoke reduction is needed: the mixed `or` lowers
// exactly as [lowerMixedExpHistogramSetOp]'s root-only leaf case does,
// and the label rewrite forwards over its result via
// [projectAttributesOverInner]'s new [chplan.MixedRowShape] branch —
// guarded by the SAME [guardLabelRewriteCollision] every other
// label_replace/label_join lowering uses, which this change also teaches
// to preserve (rather than drop) the nine Histogram*Column outputs and
// the trailing discriminator through its duplicate-labelset Aggregate.
func lowerLabelCallOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	var (
		attrs chplan.Expr
		err   error
	)
	switch call.Func.Name {
	case fnLabelReplace:
		attrs, err = labelReplaceAttributes(call, s)
	case fnLabelJoin:
		attrs, err = labelJoinAttributes(call, s)
	default:
		return nil, fmt.Errorf("promql: internal invariant violated: %s is not a label-only mixed set-op consumer", call.Func.Name)
	}
	if err != nil {
		return nil, err
	}

	inner, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	return guardLabelRewriteCollision(projectAttributesOverInner(inner, s, attrs), s), nil
}
