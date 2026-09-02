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
// on()/ignoring() reduced-key matching and the group_left()/
// group_right() broadcast — in EITHER direction, regardless of which
// side of the PromQL expression the histogram operand was written on —
// are recognised as of cerberus issues #2342 and #2537, by threading
// vm's Match/Card/Include onto [chplan.HistogramFloatVectorJoin]. That
// node's own Match field and the chsql emitter's shared join-key helpers
// (matchKeyGroupExprFrag, vectorMatchPredicateFrag, outputMatchSetFrag)
// already generalise over chplan.VectorMatch / chplan.VectorCard, so
// this widening needed no new chplan/chsql join mechanics beyond the
// CardOneToMany role split — see internal/chsql/histogram_float_
// vector_join.go's own header doc for the per-side role split this
// unlocks.
//
// A cardinality modifier's "many" side is defined relative to the
// PromQL AST's actual LHS/RHS (`group_left()` keeps the syntactic LHS
// many; `group_right()` keeps the syntactic RHS many), but
// chplan.HistogramFloatVectorJoin.Left is ALWAYS the histogram operand
// regardless of which side of `*`/`/` it was written on. The `histLHS`
// flag computed below resolves that mismatch by picking the chplan Card
// that keeps the SAME physical role the PromQL cardinality keyword
// names: `demo_latency_exp_hist * on(job) group_left() float_vec` (hist
// LHS, group_left) and its mirror-image operand order `float_vec *
// on(job) group_right() demo_latency_exp_hist` (hist RHS, group_right)
// both keep the histogram "many", so both map onto chplan.CardManyToOne
// (Left many, Right one); `float_vec * on(job) group_left()
// demo_latency_exp_hist` (hist RHS, group_left) and `demo_latency_exp_hist
// * on(job) group_right() float_vec` (hist LHS, group_right) both keep
// the histogram "one" — broadcasting the single matched histogram across
// every matching float row — so both map onto chplan.CardOneToMany (Left
// one, Right many).
//
// CardOneToOne with an on()/ignoring() reduced key needs no such
// operand-order tracking, even though Prometheus's resultMetric reduces
// the ACTUAL SYNTACTIC LHS operand's own labels (Keep for on(), Del for
// ignoring()) while chplan.HistogramFloatVectorJoin.Left is always the
// histogram side: as [chplan.HistogramFloatVectorJoin]'s own doc proves,
// the join's ON-clause equality already forces both sides' reduced
// Attributes to be byte-identical for any row that joins at all, so the
// emitter reduces Left unconditionally and both MUL/DIV operand orders
// stay supported without the recognizer having to pick a side.
//
// Wired at [lowerRoot] ahead of [expHistogramDroppingVectorBinop] — the
// same TOP-LEVEL-only dispatch point as its siblings, mirroring how the
// literal-scalar scaling recognizer precedes its own dropping sibling
// so a scalable shape keeps its value rather than being dropped by a
// broader, later-checked recognizer.
func expHistogramFloatVectorScalingBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide, floatSide parser.Expr, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, ok bool) {
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
	histLHS := hist == b.LHS

	vm := b.VectorMatching
	promCard := parser.CardOneToOne
	if vm != nil {
		promCard = vm.Card
	}

	switch promCard {
	case parser.CardOneToOne:
		m := chplan.VectorMatch{}
		if vm != nil {
			m.Labels = append([]string(nil), vm.MatchingLabels...)
			m.On = vm.On
		}
		return hist, float, chOp, m, chplan.CardOneToOne, nil, true
	case parser.CardManyToOne:
		// group_left() keeps the syntactic LHS "many". When the
		// histogram operand IS that LHS, the histogram stays "many" —
		// chplan.CardManyToOne (Left/histogram many, Right/float one).
		// Otherwise the histogram operand is the RHS, which group_left()
		// keeps as the "one" side — chplan.CardOneToMany (Left/histogram
		// one, broadcasting across every matching Right/float row).
		m := chplan.VectorMatch{Labels: append([]string(nil), vm.MatchingLabels...), On: vm.On}
		inc := append([]string(nil), vm.Include...)
		chCard := chplan.CardOneToMany
		if histLHS {
			chCard = chplan.CardManyToOne
		}
		return hist, float, chOp, m, chCard, inc, true
	case parser.CardOneToMany:
		// group_right() keeps the syntactic RHS "many". When the
		// histogram operand is that RHS, the histogram stays "many" —
		// chplan.CardManyToOne (Left/histogram many, Right/float one).
		// Otherwise the histogram operand is the LHS, which
		// group_right() keeps as the "one" side — chplan.CardOneToMany
		// (Left/histogram one, broadcast).
		m := chplan.VectorMatch{Labels: append([]string(nil), vm.MatchingLabels...), On: vm.On}
		inc := append([]string(nil), vm.Include...)
		chCard := chplan.CardManyToOne
		if histLHS {
			chCard = chplan.CardOneToMany
		}
		return hist, float, chOp, m, chCard, inc, true
	default:
		return nil, nil, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
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
