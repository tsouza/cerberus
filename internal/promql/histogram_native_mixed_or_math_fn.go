package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_math_fn.go lowers a single-arg instant math
// function — every entry in instant_fns.go's instantFnCH table (abs(),
// ceil(), floor(), sqrt(), the log/trig/deg-rad families, sgn(), and
// round() called with its default 1-arg form) — directly wrapping a
// mixed float/histogram `or` (histogram_native_mixed_or.go's own
// #2330/#2335 shape). Cerberus issue #2449, the third wrapper family to
// compose over that shape after `sum`/`avg` (#2346) and
// `label_replace`/`label_join` (#2449's own first pass, PR #2476) — and
// the first whose composition actually needs to READ the payload rather
// than just forward it.
//
// Reference Prometheus semantics (verified against the vendored fork's
// promql/functions.go, NOT assumed from this issue's own acceptance
// text): funcAbs and every sibling in instantFnCH's dispatch table share
// one helper, simpleFloatFunc — "for _, el := range vectorVals[0] { if
// el.H == nil { // Process only float samples ... } }". A histogram-
// valued sample is silently skipped, never computed over; there is no
// reference notion of "the absolute value of a histogram". So
// `abs(a or b)` for a mixed `or` answers exactly the FLOAT rows of the
// union with abs() applied, and drops every histogram row outright —
// the SAME "drop" family sort()/sort_desc() (#2463), the clamp family
// (#2444) and absent() (#2457) already established for a BARE histogram
// shape via dropExpHistogramSamples. It is NOT the "recompute per-sample
// over both float and histogram values" shape this issue's own
// acceptance section speculated (a discriminator-keyed chplan.Case / CH
// if()) — that speculation predates the drop-semantics precedent
// #2456/#2462/#2474 established, and no such per-sample histogram
// semantics exists in reference for this function family. So this file
// needs no chplan.Case: it filters to the float-shaped rows and
// re-projects, exactly dropExpHistogramSamples's shape but keeping the
// float rows' real values instead of answering unconditionally empty.
//
// Deliberately its own sibling recognizer registered root-only in
// lowerRoot, not a widening of mixedExpHistogramSetOp's own
// registration — see that function's and
// histogram_native_mixed_or_label.go's header comments for why.
//
// round()'s 2-arg to_nearest form ALSO composes over this shape, since
// cerberus issue #2578 — [roundToNearestOverMixedExpHistogramSetOp] and
// [lowerRoundToNearestOverMixedExpHistogramSetOp] below are its own
// recognizer/lowering pair, reusing this file's
// [floatRowsOnlyOverMixedExpHistogramSetOp] scaffolding but applying
// instant_fns.go's round-to-nearest value rewrite instead of a plain
// chFn(Value) — round()'s to_nearest bound (Args[1]) is unrelated to the
// mixed shape and is lowered exactly as [lowerRoundToNearest] lowers it.
// The clamp family (further bound arguments of its own, lowered by
// lowerClamp) and arbitrary arithmetic binops directly wrapping a mixed
// `or` (`(a or b) + 1`, taught instead by
// histogram_native_mixed_or_arithmetic.go, cerberus issue #2449's fourth
// pass) are the remaining wrapper shapes this file does not attempt;
// anything else still falls through to internal/promql/binary.go's
// lowerVectorSetOp rejection unchanged — see
// test/rejection-parity/catalogue's tracking entry for that site.
func mathFnOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, chplan.Fn, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, "", false
	}
	chFn, known := instantFnCH[call.Func.Name]
	if !known || len(call.Args) != 1 {
		// round() called with 2 args (the to_nearest bound) takes a
		// further bound argument and needs its own value-rewrite —
		// [roundToNearestOverMixedExpHistogramSetOp] below handles that
		// shape instead of widening this recognizer. The clamp family
		// (further bound arguments of its own, lowered by lowerClamp)
		// remains unattempted here.
		return nil, nil, "", false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, "", false
	}
	return call, b, chFn, true
}

// lowerMathFnOverMixedExpHistogramSetOp lowers the shape
// [mathFnOverMixedExpHistogramSetOp] recognised: build the same Mixed
// [chplan.VectorSetOp] node the root-only leaf case does
// ([lowerMixedExpHistogramSetOp]), keep only its float-shaped rows (the
// [chplan.MixedDiscriminatorColumn] is 0 there — reference's
// simpleFloatFunc never re-admits a histogram sample), and re-project to
// the canonical float quartet with chFn(Value) applied via
// [mathFnValueExpr] (instant_fns.go) — the same rewrite
// [lowerInstantFn] uses for a bare/derived float input, dropping the
// nine Histogram*Column outputs and the discriminator column along the
// way since the result is unconditionally float-only.
//
// MetricName is forced to "" rather than forwarded, mirroring
// [projectValueOverInner]'s canonical-shape branch: every math function
// in instantFnCH derives a new sample and Prom's own DropName rule
// strips `__name__` from it (see that function's doc comment for the
// compat-lane history this matches).
func lowerMathFnOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, chFn chplan.Fn, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatRowsOnly, err := floatRowsOnlyOverMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	newValue := mathFnValueExpr(chFn, &chplan.ColumnRef{Name: s.ValueColumn})
	return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
}

// floatRowsOnlyOverMixedExpHistogramSetOp lowers the Mixed
// [chplan.VectorSetOp] node [mixedExpHistogramSetOp] recognises b as, and
// narrows it to its float-shaped rows (the [chplan.MixedDiscriminatorColumn]
// is 0 there — reference's simpleFloatFunc never re-admits a histogram
// sample). Shared by [lowerMathFnOverMixedExpHistogramSetOp] (chFn(Value))
// and [lowerRoundToNearestOverMixedExpHistogramSetOp] (round()'s
// to_nearest rewrite) — both value transforms read only Value, so both
// need the identical filtered-down-to-float-rows input before applying
// their own rewrite and re-projecting to the canonical float quartet.
func floatRowsOnlyOverMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.Filter{
		Input: inner,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: mixedDiscriminatorColumn},
			Right: &chplan.LitInt{V: 0},
		},
	}, nil
}

// projectCanonicalFloatValue re-projects a float-rows-only plan (an
// [floatRowsOnlyOverMixedExpHistogramSetOp] result) onto the canonical
// float-Sample quartet with newValue as Value. MetricName is forced to
// "" rather than forwarded, mirroring [projectValueOverInner]'s
// canonical-shape branch: every derived sample this file's callers build
// (a math function or round()'s to_nearest rewrite) matches Prom's own
// DropName rule stripping `__name__` from a derived sample (see that
// function's doc comment for the compat-lane history this matches).
func projectCanonicalFloatValue(floatRowsOnly chplan.Node, s schema.Metrics, newValue chplan.Expr) chplan.Node {
	return &chplan.Project{
		Input: floatRowsOnly,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}
}

// roundToNearestOverMixedExpHistogramSetOp recognises `round(v, n)` (the
// 2-arg to_nearest form) directly wrapping a mixed float/histogram `or`
// — the shape [mathFnOverMixedExpHistogramSetOp] above deliberately
// leaves unattempted since it takes a further bound argument (cerberus
// issue #2578). v is checked against [mixedExpHistogramSetOp]; n (the
// to_nearest bound) is unconstrained and lowered independently by
// [lowerRoundToNearestOverMixedExpHistogramSetOp] exactly as
// [lowerRoundToNearest] (instant_fns.go) lowers it for a bare/derived
// float vector argument.
func roundToNearestOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || call.Func.Name != "round" || len(call.Args) != 2 {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

// lowerRoundToNearestOverMixedExpHistogramSetOp lowers the shape
// [roundToNearestOverMixedExpHistogramSetOp] recognised: narrow the Mixed
// [chplan.VectorSetOp] node to its float-shaped rows exactly as
// [lowerMathFnOverMixedExpHistogramSetOp] does, then apply
// instant_fns.go's `round(Value / to_nearest) * to_nearest` rewrite
// ([roundToNearestValueExpr]) instead of a plain chFn(Value) — the
// to_nearest bound (call.Args[1]) is lowered via
// [lowerRoundToNearestBound], identical to the bare/derived-vector
// path's own literal/computed split.
func lowerRoundToNearestOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatRowsOnly, err := floatRowsOnlyOverMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	tn, err := lowerRoundToNearestBound(call.Args[1], s, ctx)
	if err != nil {
		return nil, err
	}

	newValue := roundToNearestValueExpr(&chplan.ColumnRef{Name: s.ValueColumn}, tn)
	return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
}
