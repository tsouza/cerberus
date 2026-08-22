package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_float_vector_scaling_binop.go answers MUL (either
// operand order) and histogram-left DIV histogram-SCALING by a genuine
// per-series float-VECTOR operand (cerberus issue #2339) — the gap
// histogram_native_float_vector_binop.go's own header doc calls out as
// deliberately left unmatched when that file shipped (#2331/#2332):
// [expHistogramScalarBinop] (histogram_native_scalar_binop.go, #2087)
// already scales a histogram by a compile-time scalar LITERAL
// multiplier/divisor; this file supplies the missing row-by-row MATCH a
// genuine per-series float-VECTOR operand needs before that SAME
// per-bucket fold can run, via [chplan.HistogramFloatVectorJoin]
// (internal/chplan/histogram_float_vector_join.go) — a real INNER JOIN
// keyed on Match, rather than a compile-time constant riding along for
// free.
//
// Reference Prometheus answers this with the identical vectorElemBinop
// arms [expHistogramScalarBinop]'s own header doc quotes — `hlhs.Mul(rhs)`
// / `hlhs.Div(rhs)` — except `rhs` here is each matched pair's own
// joined float sample rather than a query-wide constant.
//
// Only MUL and histogram-left DIV are recognised here: the rest of the
// op family (`+`/`-`/`^`/`%`/every comparison/`atan2`, and float-vector-
// left DIV) already drops via [expHistogramDroppingVectorBinop]
// (histogram_native_float_vector_binop.go, #2331), unchanged by this
// file — [expHistogramScalarOpDropsSample] already excludes
// histogram-left DIV from its own drop family, and MUL was never in it,
// so the two recognisers stay disjoint without an explicit exclusion
// here.
//
// on()/ignoring() reduced-key matching and the "histogram is the many
// side" group_left()/group_right() broadcast are recognised as of
// cerberus issue #2342, by threading vm's Match/Card/Include straight
// onto [chplan.HistogramFloatVectorJoin] — that node's own Match field
// and the chsql emitter's shared join-key helpers (matchKeyGroupExprFrag,
// vectorMatchPredicateFrag, outputMatchSetFrag) already generalise over
// chplan.VectorMatch / chplan.VectorCard, so widening needed no new
// chplan/chsql join mechanics — see internal/chsql/histogram_float_
// vector_join.go's own header doc for the per-side role split this
// unlocks.
//
// A cardinality modifier's "many" side is defined relative to the
// PromQL AST's actual LHS/RHS (`group_left()` keeps the syntactic LHS
// many; `group_right()` keeps the syntactic RHS many), but
// chplan.HistogramFloatVectorJoin.Left is ALWAYS the histogram operand
// regardless of which side of `*`/`/` it was written on. The `histIsLHS`
// check below resolves that: only the shape where the histogram operand is
// the AST's "many" side is supported — `demo_latency_exp_hist * on(job)
// group_left() float_vec` (hist LHS, group_left) and its mirror-image
// operand order `float_vec * on(job) group_right() demo_latency_exp_hist`
// (hist RHS, group_right) both map onto chplan.CardManyToOne (Left many,
// Right one); the reverse cardinality — the histogram side broadcasting
// as the "one" against many float rows — is not supported and falls
// through to the pre-existing catch-all rejection rather than being
// silently mis-joined.
//
// CardOneToOne with an on()/ignoring() reduced key has the same
// asymmetry for a different reason: Prometheus's resultMetric reduces
// the ACTUAL SYNTACTIC LHS operand's own labels (Keep for on(),
// Del for ignoring()), and chplan.HistogramFloatVectorJoin always
// publishes the reduction over Left (the histogram side)'s own
// Attributes — see internal/chsql's histogramFloatVectorJoinOutputAttributesFrag.
// So a reduced-key CardOneToOne match is only recognised when the
// histogram operand IS the syntactic LHS; the mirror MUL order (`float_vec
// * on(job) demo_latency_exp_hist`, no group modifier) falls through to
// the catch-all rejection rather than reducing the wrong operand's
// labels. DEFAULT (full-Attributes) CardOneToOne matching is unaffected
// by this restriction — Keep/Del is then a no-op, so it makes no
// difference which operand's Attributes the (byte-identical) output
// derives from, and both MUL orders stay supported exactly as #2339
// shipped them.
//
// Wired at [lowerRoot] ahead of [expHistogramDroppingVectorBinop] — the
// same TOP-LEVEL-only dispatch point as its siblings, mirroring how the
// literal-scalar scaling recognizer precedes its own dropping sibling
// so a scalable shape keeps its value rather than being dropped by a
// broader, later-checked recognizer.
func expHistogramFloatVectorScalingBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide, floatSide parser.Expr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.MUL && b.Op != parser.DIV) {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	// A compile-time scalar literal on either side is
	// [expHistogramScalarBinop]'s own shape — excluding it here keeps
	// the two recognisers disjoint instead of double-matching the same
	// operator pair, mirroring [expHistogramDroppingVectorBinop]'s
	// identical exclusion.
	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	lhsHist := isExpHistogramValuedShape(b.LHS, s, ctx)
	rhsHist := isExpHistogramValuedShape(b.RHS, s, ctx)

	var hist, float parser.Expr
	switch {
	case b.Op == parser.MUL && lhsHist && !rhsHist:
		hist, float = b.LHS, b.RHS
	case b.Op == parser.MUL && rhsHist && !lhsHist:
		hist, float = b.RHS, b.LHS
	case b.Op == parser.DIV && lhsHist && !rhsHist:
		// DIV is not commutative — only histogram-left DIV is a
		// supported scaling shape (reference's own switch keys off
		// which OPERAND carries the histogram, and a histogram can
		// only be the numerator here), mirroring
		// [expHistogramScalarBinop]'s identical DIV restriction for
		// the literal case.
		hist, float = b.LHS, b.RHS
	default:
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	histIsLHS := hist == b.LHS

	vm := b.VectorMatching
	promCard := parser.CardOneToOne
	if vm != nil {
		promCard = vm.Card
	}

	switch promCard {
	case parser.CardOneToOne:
		if vm != nil && (len(vm.MatchingLabels) > 0 || vm.On) && !histIsLHS {
			// See this file's header doc: a reduced-key CardOneToOne
			// match must reduce the syntactic LHS operand's own
			// labels, which the emitter only knows how to do for the
			// histogram (Left) side.
			return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
		}
		m := chplan.VectorMatch{}
		if vm != nil {
			m.Labels = append([]string(nil), vm.MatchingLabels...)
			m.On = vm.On
		}
		return hist, float, chOp, m, chplan.CardOneToOne, nil, true
	case parser.CardManyToOne:
		// group_left() keeps the syntactic LHS "many" — only supported
		// when the histogram operand IS that LHS.
		if !histIsLHS {
			return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
		}
	case parser.CardOneToMany:
		// group_right() keeps the syntactic RHS "many" — only
		// supported when the histogram operand IS that RHS.
		if histIsLHS {
			return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
		}
	default:
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	// Both branches above land here only once confirmed hist-many:
	// chplan.HistogramFloatVectorJoin.Left is ALWAYS the histogram
	// operand, so CardManyToOne (hist LHS) and CardOneToMany (hist RHS)
	// both map onto the SAME chplan.CardManyToOne (Left many, Right
	// one) regardless of which PromQL cardinality keyword produced
	// them.
	m := chplan.VectorMatch{Labels: append([]string(nil), vm.MatchingLabels...), On: vm.On}
	inc := append([]string(nil), vm.Include...)
	return hist, float, chOp, m, chplan.CardManyToOne, inc, true
}

// lowerExpHistogramFloatVectorScalingBinop lowers the shape
// [expHistogramFloatVectorScalingBinop] recognised: joins histSide's own
// HistogramProjection against floatSide's ordinary lowering via
// [chplan.HistogramFloatVectorJoin] keyed on match/card/include, then
// applies the SAME per-bucket scale-fold [scaleHistogramProjection]
// applies for a literal scalar, reading the scale factor off the join's
// own ValueColumn instead of a constant.
func lowerExpHistogramFloatVectorScalingBinop(histSide, floatSide parser.Expr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	hp, err := lowerExpHistogramValuedOperand(histSide, s, ctx)
	if err != nil {
		return nil, err
	}
	floatNode, err := lower(floatSide, s, ctx)
	if err != nil {
		return nil, err
	}
	join := &chplan.HistogramFloatVectorJoin{
		Left:             hp,
		Right:            floatNode,
		Match:            match,
		Card:             card,
		Include:          include,
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
	return scaleHistogramProjection(join, op, &chplan.ColumnRef{Name: s.ValueColumn}, s), nil
}
