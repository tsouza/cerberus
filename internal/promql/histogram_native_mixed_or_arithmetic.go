package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_arithmetic.go lowers a scalar arithmetic
// binop (`+`, `-`, `*`, `/`, `%`, `^`, `atan2`) directly wrapping a mixed
// float/histogram `or` (histogram_native_mixed_or.go's own #2330/#2335
// shape), for the sub-family of those operators whose scalar/histogram
// pairing reference Prometheus DROPS rather than computes. Cerberus
// issue #2449, the fourth wrapper family to compose over that shape
// after `sum`/`avg` (#2346), `label_replace`/`label_join` (#2449's own
// first pass, PR #2476) and single-arg instant math functions (#2449's
// second pass, PR #2479) — and the issue's own originally-named example
// (`(a or b) + 1`).
//
// Reference Prometheus semantics (verified against the vendored fork's
// promql/engine.go, NOT assumed from this issue's own acceptance text):
// VectorscalarBinop → vectorElemBinop's `hlhs != nil && hrhs == nil`
// switch answers a histogram-valued sample paired with a scalar three
// ways: MUL and histogram-left DIV SCALE the histogram
// (`hlhs.Copy().Mul(rhs)` / `.Div(rhs)`, `keep=true` — the family
// histogram_native_scalar_binop.go's [expHistogramScalarBinop] already
// answers for a BARE histogram shape); ADD, SUB, POW, MOD, ATAN2 and
// scalar-left DIV return `NewIncompatibleTypesInBinOpInfo` and
// `keep=false` — the histogram-valued sample is dropped outright, the
// same "drop" family sort()/sort_desc() (#2463), clamp (#2444),
// absent() (#2457) and this issue's own math-fn pass (#2479) already
// established. histogram_native_scalar_binop.go's
// [expHistogramScalarOpDropsSample] is already the single source of
// truth cerberus uses to classify that split for a BARE histogram
// operand; this file reuses it unchanged rather than re-deriving the
// same op list, so the two recognizers can never drift apart.
//
// Comparison operators (`==`, `!=`, `<`, `<=`, `>`, `>=`) are
// deliberately OUT of this recognizer's scope even though
// [expHistogramScalarOpDropsSample] classifies their histogram/scalar
// pairing as "drop" too: internal/promql/binary.go's [lowerVectorScalar]
// answers a comparison through a structurally different shape (a bare
// Filter without the `bool` modifier, a toFloat64-wrapped Project with
// it) than the single Project every arithmetic op shares here, so
// widening this recognizer to comparisons is a second, separately-scoped
// composition rather than "the same shape, one more op" — left for a
// follow-on pass, same as this issue's own math-fn PR (#2479) left
// round()'s 2-arg to_nearest form unattempted. MUL and histogram-left
// DIV are likewise out of scope: they need the histogram side actually
// SCALED (all nine Histogram*Column outputs, à la
// [scaleHistogramProjection]) rather than dropped, a materially
// different lowering. Both stay tracked by
// test/rejection-parity/catalogue's rotated trigger query under this
// issue.
func arithmeticOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || b.Op.IsSetOperator() || b.Op.IsComparisonOperator() {
		return nil, "", 0, false, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, "", 0, false, false
	}

	lhsScalar, lhsIsScalar := tryScalarLiteral(b.LHS)
	rhsScalar, rhsIsScalar := tryScalarLiteral(b.RHS)
	var vecSide parser.Expr
	var scalarVal float64
	var scalarLeft bool
	switch {
	case lhsIsScalar && !rhsIsScalar:
		vecSide, scalarVal, scalarLeft = b.RHS, lhsScalar, true
	case rhsIsScalar && !lhsIsScalar:
		vecSide, scalarVal, scalarLeft = b.LHS, rhsScalar, false
	default:
		// Neither/both sides fold to a scalar — a vector-vector arithmetic
		// binop over a mixed `or` (`(a or b) + other_metric`) is a further,
		// unattempted shape; see this file's header.
		return nil, "", 0, false, false
	}

	// expHistogramScalarOpDropsSample's histogramOnLeft parameter means
	// "the histogram-payload operand sits on the LHS of the arithmetic
	// expression" — the mirror image of scalarLeft.
	if !expHistogramScalarOpDropsSample(b.Op, !scalarLeft) {
		return nil, "", 0, false, false
	}

	inner, matched := mixedExpHistogramSetOp(vecSide, s, ctx)
	if !matched {
		return nil, "", 0, false, false
	}
	return inner, chOp, scalarVal, scalarLeft, true
}

// lowerArithmeticOverMixedExpHistogramSetOp lowers the shape
// [arithmeticOverMixedExpHistogramSetOp] recognised: build the same
// Mixed [chplan.VectorSetOp] node the root-only leaf case does
// ([lowerMixedExpHistogramSetOp]), keep only its float-shaped rows (the
// [chplan.MixedDiscriminatorColumn] is 0 there — reference's
// VectorscalarBinop never re-admits the dropped histogram sample), and
// re-project through `scalar OP Value` / `Value OP scalar` — the same
// rewrite [lowerVectorScalar] uses for a bare/derived float input.
//
// MetricName is forced to "" rather than forwarded: reference's
// `changesMetricSchema` answers true for every arithmetic op this
// recognizer accepts (ADD, SUB, DIV, MUL, POW, MOD, ATAN2), so Prom's
// own DropName rule always strips `__name__` from an arithmetic-derived
// sample — mirrors [mathFnValueExpr]'s callers and
// [lowerVectorScalar]'s own projection.
func lowerArithmeticOverMixedExpHistogramSetOp(setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft bool, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lowerMixedExpHistogramSetOp(setOp, s, ctx)
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

	valueRef := chplan.Expr(&chplan.ColumnRef{Name: s.ValueColumn})
	scalarLit := chplan.Expr(&chplan.LitFloat{V: scalar})
	var opExpr chplan.Expr
	if scalarOnLeft {
		opExpr = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		opExpr = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
	}

	return &chplan.Project{
		Input: floatRowsOnly,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: opExpr, Alias: s.ValueColumn},
		},
	}, nil
}
