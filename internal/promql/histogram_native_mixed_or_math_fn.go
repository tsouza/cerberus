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
// histogram_native_mixed_or_label.go's header comments for why. Every
// OTHER wrapper around a mixed `or` this file does not recognise
// (arithmetic binops like `(a or b) + 1`, round()/clamp's bound-taking
// forms, and so on) still falls through to
// internal/promql/binary.go's lowerVectorSetOp rejection unchanged — see
// test/rejection-parity/catalogue's tracking entry for that site.
func mathFnOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, chplan.Fn, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, "", false
	}
	chFn, known := instantFnCH[call.Func.Name]
	if !known || len(call.Args) != 1 {
		// round() called with 2 args (the to_nearest bound) and the
		// clamp family take further arguments of their own and are
		// lowered by dedicated helpers (lowerRoundToNearest,
		// lowerClamp) that this recognizer does not attempt to
		// widen — see this file's header.
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
	inner, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	floatRowsOnly := &chplan.Filter{
		Input: inner,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: mixedDiscriminatorColumn},
			Right: &chplan.LitInt{V: 0},
		},
	}

	newValue := mathFnValueExpr(chFn, &chplan.ColumnRef{Name: s.ValueColumn})

	return &chplan.Project{
		Input: floatRowsOnly,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}, nil
}
