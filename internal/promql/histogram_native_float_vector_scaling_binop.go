package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_float_vector_scaling_binop.go answers MUL (either
// operand order) and histogram-left DIV histogram-SCALING by a genuine
// per-series float-VECTOR operand, under default (full-Attributes)
// one-to-one vector matching (cerberus issue #2339) — the gap
// histogram_native_float_vector_binop.go's own header doc calls out as
// deliberately left unmatched when that file shipped (#2331/#2332):
// [expHistogramScalarBinop] (histogram_native_scalar_binop.go, #2087)
// already scales a histogram by a compile-time scalar LITERAL
// multiplier/divisor; this file supplies the missing row-by-row MATCH a
// genuine per-series float-VECTOR operand needs before that SAME
// per-bucket fold can run, via [chplan.HistogramFloatVectorJoin]
// (internal/chplan/histogram_float_vector_join.go) — a real INNER JOIN
// keyed on Attributes, rather than a compile-time constant riding along
// for free.
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
// Only DEFAULT (full-Attributes) one-to-one vector matching is
// recognised: on()/ignoring() reduced-key matching and
// group_left()/group_right() many-to-one broadcast for this
// histogram/float-vector scaling shape fall through to
// [expHistogramSelectorRouting]'s pre-existing catch-all rejection,
// same as before this file existed — tracked as follow-up work (see the
// #2339 PR body for the filed issue).
//
// Wired at [lowerRoot] ahead of [expHistogramDroppingVectorBinop] — the
// same TOP-LEVEL-only dispatch point as its siblings, mirroring how the
// literal-scalar scaling recognizer precedes its own dropping sibling
// so a scalable shape keeps its value rather than being dropped by a
// broader, later-checked recognizer.
func expHistogramFloatVectorScalingBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide, floatSide parser.Expr, op chplan.BinaryOp, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, "", false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.MUL && b.Op != parser.DIV) {
		return nil, nil, "", false
	}
	if !isDefaultMatching(b.VectorMatching) {
		return nil, nil, "", false
	}
	// A compile-time scalar literal on either side is
	// [expHistogramScalarBinop]'s own shape — excluding it here keeps
	// the two recognisers disjoint instead of double-matching the same
	// operator pair, mirroring [expHistogramDroppingVectorBinop]'s
	// identical exclusion.
	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		return nil, nil, "", false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, "", false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, "", false
	}
	lhsHist := isExpHistogramValuedShape(b.LHS, s, ctx)
	rhsHist := isExpHistogramValuedShape(b.RHS, s, ctx)
	if b.Op == parser.MUL {
		switch {
		case lhsHist && !rhsHist:
			return b.LHS, b.RHS, chOp, true
		case rhsHist && !lhsHist:
			return b.RHS, b.LHS, chOp, true
		}
		return nil, nil, "", false
	}
	// DIV is not commutative — only histogram-left DIV is a supported
	// scaling shape (reference's own switch keys off which OPERAND
	// carries the histogram, and a histogram can only be the numerator
	// here), mirroring [expHistogramScalarBinop]'s identical DIV
	// restriction for the literal case.
	if lhsHist && !rhsHist {
		return b.LHS, b.RHS, chOp, true
	}
	return nil, nil, "", false
}

// lowerExpHistogramFloatVectorScalingBinop lowers the shape
// [expHistogramFloatVectorScalingBinop] recognised: joins histSide's own
// HistogramProjection against floatSide's ordinary lowering via
// [chplan.HistogramFloatVectorJoin], then applies the SAME per-bucket
// scale-fold [scaleHistogramProjection] applies for a literal scalar,
// reading the scale factor off the join's own ValueColumn instead of a
// constant.
func lowerExpHistogramFloatVectorScalingBinop(histSide, floatSide parser.Expr, op chplan.BinaryOp, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
	return scaleHistogramProjection(join, op, &chplan.ColumnRef{Name: s.ValueColumn}, s), nil
}
