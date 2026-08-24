package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_vector_plain_arithmetic.go lowers a
// vector-vector arithmetic binop (`+`, `-`, `*`, `/`, `^`, `%`, `atan2`)
// where exactly ONE operand is a mixed float/histogram `or`
// (histogram_native_mixed_or.go's own #2330/#2335 shape) and the OTHER is
// an ORDINARY, non-mixed, non-histogram-valued vector — cerberus issue
// #2449's tenth and final wrapper family, and the one every prior pass on
// this issue named as its own remaining scope under the rejection-parity
// catalogue's `internal/promql/binary.go:lowerVectorSetOp#4924e0ba` entry
// (trigger query `(demo_latency_exp_hist or histogram_quantile(0.5,
// demo_latency_exp_hist)) * demo_num_cpus`).
//
// # Why this is NOT a new four-combination fold
//
// histogram_native_mixed_or_vector_arithmetic.go's own header derives the
// four float/histogram combinations a [chplan.MixedVectorJoin] can produce
// when BOTH sides carry their own per-row discriminator, and builds the
// per-op fold ([lowerMixedVVAdditiveArithmetic] / [lowerMixedVVScaledArithmetic]
// / [lowerMixedVVFloatOnlyArithmetic]) against that discriminator pair —
// verified directly against the vendored `tsouza/prometheus:cerberus-
// parser` fork's `vectorElemBinop`, which treats a vector-vector binop
// uniformly regardless of whether a given operand's OWN samples are
// homogeneously one type or a per-row mix: `hlhs`/`hrhs` are simply
// whichever payload THIS row's THIS operand happens to carry.
//
// An ordinary (non-mixed) vector is exactly the DEGENERATE case of that
// same per-row discriminator where every row's own discriminator is
// statically 0 (float) — it has no histogram payload, ever.
// [widenPlainVectorToMixedShape] makes that degeneracy literal: it wraps
// the plain side's own lowered Node in a Project publishing the identical
// fourteen-column Mixed contract [chplan.MixedVectorJoin] expects from
// BOTH sides — the plain side's real canonical quartet, forwarded
// unchanged, plus the same typed-zero / empty-array Histogram*Column
// placeholders and discriminator=0 the ROOT mixed `or` lowering's own
// float arm already publishes for a float-shaped row (chsql's
// [mixedVectorSetOpHistogramPlaceholderCols], internal/chsql/
// vector_set_op.go) — so [chplan.MixedVectorJoin] cannot tell the
// difference between "the other side's own per-row discriminator happens
// to be 0 on this row" and "the other side has no discriminator at all".
//
// That means EVERY per-op fold function
// histogram_native_mixed_or_vector_arithmetic.go already built —
// [lowerMixedVVAdditiveArithmetic], [lowerMixedVVScaledArithmetic],
// [lowerMixedVVFloatOnlyArithmetic] — answers this shape CORRECTLY with
// zero changes: reused verbatim below, keyed off the widened join exactly
// as the two-mixed-operand shape already keys off its own. Worked example
// for `*` ([lowerMixedVVScaledArithmetic]): the plain side's discriminator
// is always 0, so `bothHist` (its own MUL keep predicate) can never be
// true purely from the plain side, and [mixedVVFlipSource]'s per-row
// `rIsHist` pick correctly always reads the histogram-shaped fields off
// the MIXED side's own columns, whichever row-by-row shape they carry —
// exactly reference's `hlhs.Copy().Mul(rhs)` / a genuine `lhs * rhs` per
// row, decided per row rather than assumed uniform for the whole operand.
//
// # Left/Right placement matters for non-commutative ops
//
// [vectorPlainArithmeticOverMixedExpHistogramSetOp] preserves which
// operand was syntactically LHS vs RHS onto [chplan.MixedVectorJoin]'s own
// Left/Right — required for `-`/`/`/`^`/`%`/`atan2`, none of which are
// commutative, and for group_left()/group_right() cardinality (PromQL
// defines "many" relative to the operator's own LHS/RHS, not to which side
// happens to be mixed).
func widenPlainVectorToMixedShape(node chplan.Node, s schema.Metrics) chplan.Node {
	zeroFloat := func() chplan.Expr { return &chplan.LitFloat{V: 0} }
	zeroInt := func() chplan.Expr { return &chplan.LitInt{V: 0} }
	emptyBuckets := func() chplan.Expr { return &chplan.FuncCall{Fn: chplan.FnEmptyArrayFloat64} }

	return &chplan.Project{
		Input: node,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
			{Expr: zeroFloat(), Alias: chplan.HistogramCountColumn},
			{Expr: zeroFloat(), Alias: chplan.HistogramSumColumn},
			{Expr: zeroInt(), Alias: chplan.HistogramScaleColumn},
			{Expr: zeroFloat(), Alias: chplan.HistogramZeroThresholdColumn},
			{Expr: zeroFloat(), Alias: chplan.HistogramZeroCountColumn},
			{Expr: zeroInt(), Alias: chplan.HistogramPositiveOffsetColumn},
			{Expr: emptyBuckets(), Alias: chplan.HistogramPositiveBucketCountsColumn},
			{Expr: zeroInt(), Alias: chplan.HistogramNegativeOffsetColumn},
			{Expr: emptyBuckets(), Alias: chplan.HistogramNegativeBucketCountsColumn},
			{Expr: zeroInt(), Alias: mixedDiscriminatorColumn},
		},
	}
}

// lowerPlainOperandForMixedJoin lowers plainExpr through the ordinary
// [lower] path and widens it to the Mixed fourteen-column contract via
// [widenPlainVectorToMixedShape] — shared by this file's arithmetic fold
// and histogram_native_mixed_or_vector_plain_comparison.go's comparison
// fold. Only the three row shapes [lowerMixedExpHistogramOperands] itself
// accepts for the ROOT mixed `or`'s own float arm — canonical-shape,
// matrix RangeWindow, or instant derived-shape (cerberus issue #2333) —
// are accepted here for the identical reason: those are the shapes chsql's
// per-side aggregation ([emitter.histogramFieldsJoinSideFrag], reused
// unchanged by [chplan.MixedVectorJoin]) already knows how to collapse to
// one row per matching key. A genuinely histogram-valued or already-Mixed
// plainExpr is rejected here rather than silently mishandled — that
// combination is a DIFFERENT, not-yet-attempted shape
// ([vectorPlainArithmeticOverMixedExpHistogramSetOp]'s own recognizer
// already excludes it via [isExpHistogramValuedShape] / [mixedExpHistogramSetOp],
// so reaching this error would indicate a recognizer/lowering mismatch).
func lowerPlainOperandForMixedJoin(plainExpr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	plainNode, err := lower(plainExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	switch shape := chplan.RowShapeOf(plainNode); shape {
	case chplan.SampleRowShape, chplan.GridWindowRowShape, chplan.ReducedWindowRowShape:
		return widenPlainVectorToMixedShape(plainNode, s), nil
	default:
		return nil, fmt.Errorf(
			"promql: a mixed float/histogram 'or' operand paired with a %s-shaped operand "+
				"is not supported", shape,
		)
	}
}

// vectorPlainArithmeticOverMixedExpHistogramSetOp recognises `<mixed or>
// <arithmetic op> <plain vector>` / `<plain vector> <arithmetic op> <mixed
// or>` — exactly one side [mixedExpHistogramSetOp]'s shape, the other
// neither a scalar literal (that's [arithmeticOverMixedExpHistogramSetOp]
// / [mulOrDivScaleOverMixedExpHistogramSetOp]'s shape) nor itself
// histogram-valued (that combination remains unattempted, see
// [lowerPlainOperandForMixedJoin]'s own doc) nor itself a mixed `or`
// (that's [vectorVectorArithmeticOverMixedExpHistogramSetOp]'s shape,
// checked immediately before this one in lowerMixedExpHistogramFamily so
// both-mixed always wins the ambiguity).
func vectorPlainArithmeticOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (mixedSetOp *parser.BinaryExpr, plainExpr parser.Expr, mixedOnLeft bool, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || b.Op.IsSetOperator() || b.Op.IsComparisonOperator() {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	switch b.Op {
	case parser.ADD, parser.SUB, parser.MUL, parser.DIV, parser.POW, parser.MOD, parser.ATAN2:
	default:
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}

	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
	}

	lhsMixed, lhsOk := mixedExpHistogramSetOp(b.LHS, s, ctx)
	rhsMixed, rhsOk := mixedExpHistogramSetOp(b.RHS, s, ctx)
	if lhsOk == rhsOk {
		// Both mixed is vectorVectorArithmeticOverMixedExpHistogramSetOp's
		// own shape; neither mixed is the ordinary vector-vector path.
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
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
		return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
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
			// via b.Op.IsSetOperator() — unreachable here in practice,
			// rejected defensively rather than silently treated as
			// one-to-one.
			return nil, nil, false, "", chplan.VectorMatch{}, chplan.CardOneToOne, nil, false
		}
		if len(b.VectorMatching.Include) > 0 {
			include = append([]string(nil), b.VectorMatching.Include...)
		}
	}
	return mixedSetOp, plainExpr, mixedOnLeft, chOp, mixedExpHistogramMatch(b), card, include, true
}

// lowerVectorPlainArithmeticOverMixedExpHistogramSetOp lowers the shape
// [vectorPlainArithmeticOverMixedExpHistogramSetOp] recognised: lower the
// mixed side through [lowerMixedExpHistogramSetOp] (unchanged), lower and
// widen the plain side through [lowerPlainOperandForMixedJoin], place them
// on [chplan.MixedVectorJoin]'s Left/Right per mixedOnLeft (preserving the
// operator's own syntactic LHS/RHS for non-commutative ops and for
// group_left()/group_right()), and dispatch to the SAME per-op fold
// [lowerVectorVectorArithmeticOverMixedExpHistogramSetOp] already uses —
// see this file's header for why that fold needs no change at all for a
// statically-float-discriminator side.
func lowerVectorPlainArithmeticOverMixedExpHistogramSetOp(mixedSetOp *parser.BinaryExpr, plainExpr parser.Expr, mixedOnLeft bool, op chplan.BinaryOp, match chplan.VectorMatch, card chplan.VectorCard, include []string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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

	switch op {
	case chplan.OpAdd, chplan.OpSub:
		return lowerMixedVVAdditiveArithmetic(join, op, s), nil
	case chplan.OpMul, chplan.OpDiv:
		return lowerMixedVVScaledArithmetic(join, op, s), nil
	default:
		// POW, MOD, ATAN2: only float,float survives — the plain side's
		// static discriminator=0 already guarantees the "other" half of
		// that pair whenever the mixed side's own row resolves float too.
		return lowerMixedVVFloatOnlyArithmetic(join, op, s), nil
	}
}
