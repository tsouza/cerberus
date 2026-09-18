package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// isMixedRelationShape reports whether expr lowers to a LIVE mixed
// float/histogram relation: a mixed `or` ([mixedExpHistogramSetOp]) or one
// of the wrappers that carry its two row shapes through — the static
// mirror, for mixed relations, of what [histogramValuedProducerCall] is for
// histogram-valued ones. The two histogram-vs-float-vector recognisers
// ([expHistogramDroppingVectorBinop], [expHistogramFloatVectorScalingBinop])
// consult it so a mixed operand is never read as a float vector: they
// decline, and [lowerVectorVector] joins the pair through the
// discriminator-aware fold instead (cerberus issue #3562).
//
// The arms follow the policy table's own preserve/bespoke rows at an
// existing plan, wrapper by wrapper: the payload-neutral wrappers
// (sort_by_label, limitk/limit_ratio, label rewrites, info, unary `+`,
// `and`/`unless` forwarding a mixed LHS, `or` with a mixed arm) keep the
// relation as it is; sum/avg, the type-preserving range functions over a
// subquery of it, the scale operators (`* k`, `/ k`, unary `-`) and the
// vector-vector operators without `bool` re-fold it into the same
// fourteen-column contract. A scalar-side drop-family arithmetic or
// comparison narrows to float rows, and a `bool` comparison answers
// floats, so those are not mixed. A shape this function does not name
// lowers through the generic paths and is refused at the lowering by
// [requireFloatVectorBinopOperand] rather than answered wrongly;
// TestMixedRelationShapeAgreesWithEveryPolicyConsumer pins the predicate
// and the lowerings in agreement over the policy dispatch inventory.
func isMixedRelationShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) bool {
	if _, ok := mixedExpHistogramSetOp(expr, s, ctx); ok {
		return true
	}
	switch e := peelWrappers(expr).(type) {
	case *parser.Call:
		if len(e.Args) == 0 {
			return false
		}
		switch e.Func.Name {
		case "sort_by_label", "sort_by_label_desc", fnLabelReplace, fnLabelJoin, "info":
			return isMixedRelationShape(e.Args[0], s, ctx)
		}
		if expHistogramTypePreservingWindowFn(e.Func.Name) {
			sub, ok := peelWrappers(e.Args[0]).(*parser.SubqueryExpr)
			return ok && isMixedRelationShape(sub.Expr, s, ctx)
		}
	case *parser.AggregateExpr:
		switch e.Op {
		case parser.SUM, parser.AVG, parser.LIMITK, parser.LIMIT_RATIO:
			return isMixedRelationShape(e.Expr, s, ctx)
		}
	case *parser.UnaryExpr:
		return isMixedRelationShape(e.Expr, s, ctx)
	case *parser.BinaryExpr:
		return isMixedRelationBinary(e, s, ctx)
	}
	return false
}

// isMixedRelationBinary is [isMixedRelationShape]'s binary-operator arm.
func isMixedRelationBinary(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) bool {
	switch b.Op {
	case parser.LAND, parser.LUNLESS:
		return isMixedRelationShape(b.LHS, s, ctx)
	case parser.LOR:
		return isMixedRelationShape(b.LHS, s, ctx) || isMixedRelationShape(b.RHS, s, ctx)
	}
	op, err := promBinaryOp(b.Op)
	if err != nil {
		return false
	}
	if !isMixedRelationShape(b.LHS, s, ctx) && !isMixedRelationShape(b.RHS, s, ctx) {
		return false
	}
	lhsScalar := isScalarTypedOperand(b.LHS)
	rhsScalar := isScalarTypedOperand(b.RHS)
	switch {
	case lhsScalar || rhsScalar:
		// A scalar side scales the relation in place only under the
		// scale family; every other scalar operator narrows to floats.
		return mixedScalarBinaryFamily(op, lhsScalar) == mixedScaleFamily
	case isComparison(op):
		return !b.ReturnBool
	default:
		return true
	}
}

// isScalarTypedOperand reports whether a binary operand is a scalar: a
// literal (or a literal-only tree) or a scalar-typed expression such as
// `scalar(...)` or `time()`.
func isScalarTypedOperand(e parser.Expr) bool {
	if _, ok := tryScalarLiteral(e); ok {
		return true
	}
	return e.Type() == parser.ValueTypeScalar
}

// expHistogramTypePreservingWindowFn names the SELECT/FOLD-family window
// functions whose answer over a histogram or mixed subquery keeps the
// input's own sample kinds — the seven folds
// [lowerFurtherWrapMixedOrSubqueryFoldFn] answers and the two selects
// [lowerMixedOrSubqueryLastFirstInput] answers. count_over_time,
// present_over_time, resets, changes and the ts_of selects reduce to
// floats and are not in this set.
func expHistogramTypePreservingWindowFn(name string) bool {
	switch name {
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn,
		lastOverTimeWindowFn, firstOverTimeWindowFn:
		return true
	}
	return false
}
