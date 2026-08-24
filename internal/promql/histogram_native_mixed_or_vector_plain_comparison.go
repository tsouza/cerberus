package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_vector_plain_comparison.go lowers a
// vector-vector COMPARISON binop (`==`, `!=`, `<`, `<=`, `>`, `>=`, with or
// without `bool`) where exactly ONE operand is a mixed float/histogram
// `or` (histogram_native_mixed_or.go's own #2330/#2335 shape) and the
// OTHER is an ordinary, non-mixed, non-histogram-valued vector — the
// comparison half of cerberus issue #2449's final wrapper family,
// mirroring histogram_native_mixed_or_vector_plain_arithmetic.go's own
// "the plain side is the degenerate always-float-discriminator case"
// argument for histogram_native_mixed_or_vector_comparison.go's own
// four-combination fold ([lowerMixedVVCompareFilter] /
// [lowerMixedVVCompareBool]): both already read purely off
// [chplan.MixedVectorJoin]'s `_mvj_L_*`/`_mvj_R_*` columns and a per-row
// discriminator, so [widenPlainVectorToMixedShape]'s literal
// discriminator=0 / placeholder Histogram* columns make the plain side
// indistinguishable, at that join, from a mixed operand whose every row
// happens to resolve float — both fold functions are reused verbatim
// below.
//
// One case worth naming explicitly: a bare (non-`bool`) comparison keeps
// only float,float and histogram,histogram pairs (this file's sibling's
// own header has the full four-combination accounting, verified against
// the vendored `tsouza/prometheus:cerberus-parser` fork's
// `vectorElemBinop`). Since the plain side's discriminator is always 0,
// histogram,histogram can never occur here — a comparison against a plain
// vector only ever keeps the rows where the mixed side ALSO resolves
// float that pass the comparison, exactly matching reference (a histogram
// sample compared against an ordinary float sample is always
// `NewIncompatibleTypesInBinOpInfo`, dropped regardless of `bool`).
func comparisonVectorPlainOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (
	mixedSetOp *parser.BinaryExpr, plainExpr parser.Expr, mixedOnLeft bool, op chplan.BinaryOp,
	match chplan.VectorMatch, card chplan.VectorCard, include []string, returnBool, ok bool,
) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || !b.Op.IsComparisonOperator() {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}

	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}

	lhsMixed, lhsOk := mixedExpHistogramSetOp(b.LHS, s, ctx)
	rhsMixed, rhsOk := mixedExpHistogramSetOp(b.RHS, s, ctx)
	if lhsOk == rhsOk {
		// Both mixed is comparisonVectorVectorOverMixedExpHistogramSetOp's
		// own shape; neither mixed is the ordinary vector-vector path.
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}
	if lhsOk {
		mixedSetOp, plainExpr, mixedOnLeft = lhsMixed, b.RHS, true
	} else {
		mixedSetOp, plainExpr, mixedOnLeft = rhsMixed, b.LHS, false
	}
	if isExpHistogramValuedShape(plainExpr, s, ctx) {
		// The plain side is itself genuinely histogram-valued — a
		// different, not-yet-attempted shape (see
		// [lowerPlainOperandForMixedJoin]'s own doc), not this one.
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
	}

	card = chplan.CardOneToOne
	if b.VectorMatching != nil {
		switch b.VectorMatching.Card {
		case parser.CardOneToOne:
		case parser.CardManyToOne:
			card = chplan.CardManyToOne
		case parser.CardOneToMany:
			card = chplan.CardOneToMany
		default:
			// CardManyToMany: the parser only ever sets this for the
			// `and`/`or`/`unless` set operators, already excluded above
			// via !b.Op.IsComparisonOperator() — unreachable here in
			// practice, rejected defensively rather than silently
			// treated as one-to-one.
			return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false, false
		}
		if len(b.VectorMatching.Include) > 0 {
			include = append([]string(nil), b.VectorMatching.Include...)
		}
	}
	return mixedSetOp, plainExpr, mixedOnLeft, chOp, mixedExpHistogramMatch(b), card, include, b.ReturnBool, true
}

// lowerComparisonVectorPlainOverMixedExpHistogramSetOp lowers the shape
// [comparisonVectorPlainOverMixedExpHistogramSetOp] recognised: lower the
// mixed side, lower+widen the plain side
// ([lowerPlainOperandForMixedJoin]), place them on
// [chplan.MixedVectorJoin]'s Left/Right per mixedOnLeft (comparisons read
// L's own fields unconditionally for the non-`bool` output — see
// [lowerMixedVVCompareFilter]'s own doc for why that must be the
// operator's syntactic LHS regardless of which side is mixed), and
// dispatch to the SAME two fold functions
// [lowerComparisonVectorVectorOverMixedExpHistogramSetOp] already uses.
func lowerComparisonVectorPlainOverMixedExpHistogramSetOp(
	mixedSetOp *parser.BinaryExpr, plainExpr parser.Expr, mixedOnLeft bool, op chplan.BinaryOp,
	match chplan.VectorMatch, card chplan.VectorCard, include []string, returnBool bool,
	s schema.Metrics, ctx lowerCtx,
) (chplan.Node, error) {
	mixedNode, err := lowerMixedExpHistogramSetOp(mixedSetOp, s, ctx)
	if err != nil {
		return nil, err
	}
	plainNode, err := lowerPlainOperandForMixedJoin(plainExpr, s, ctx)
	if err != nil {
		return nil, err
	}

	leftNode, rightNode := plainNode, mixedNode
	if mixedOnLeft {
		leftNode, rightNode = mixedNode, plainNode
	}

	join := &chplan.MixedVectorJoin{
		Left:             leftNode,
		Right:            rightNode,
		Match:            match,
		Card:             card,
		Include:          include,
		StepAligned:      ctx.step > 0,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}

	if returnBool {
		return lowerMixedVVCompareBool(join, op, s), nil
	}
	return lowerMixedVVCompareFilter(join, op, s), nil
}
