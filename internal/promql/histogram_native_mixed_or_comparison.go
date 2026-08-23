package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_comparison.go lowers a scalar comparison
// binop (`==`, `!=`, `<`, `<=`, `>`, `>=`, with or without the `bool`
// modifier) directly wrapping a mixed float/histogram `or`
// (histogram_native_mixed_or.go's own #2330/#2335 shape). Cerberus issue
// #2449, the fifth wrapper family to compose over that shape after
// `sum`/`avg` (#2346), `label_replace`/`label_join` (#2449's first pass,
// PR #2476), single-arg instant math functions (#2449's second pass, PR
// #2479) and drop-family arithmetic binops (#2449's third pass, PR
// #2483) — and the shape histogram_native_mixed_or_arithmetic.go's own
// header explicitly named as deliberately unattempted, because
// `internal/promql/binary.go`'s [lowerVectorScalar] answers a comparison
// through a structurally different Filter / bool-Project shape than the
// single arithmetic Project that file's recognizer builds.
//
// Reference Prometheus semantics (verified against the vendored fork's
// promql/engine.go, NOT assumed from this issue's own acceptance text):
// `vectorElemBinop`'s `hlhs != nil && hrhs == nil` switch answers EVERY
// comparison operator (`EQLC`, `NEQ`, `GTR`, `LSS`, `GTE`, `LTE`) with
// `NewIncompatibleTypesInBinOpInfo` and `keep=false` — a histogram-valued
// sample is dropped outright for a comparison regardless of the `bool`
// modifier, exactly like the drop-family arithmetic ops #2483 already
// answers. `histogram_native_scalar_binop.go`'s
// [expHistogramScalarOpDropsSample] already classifies comparisons as
// drop unconditionally (its switch lists all six comparison ItemTypes in
// the same case as ADD/SUB/POW/MOD/ATAN2); this file reuses it unchanged
// for the identical "single source of truth" reason #2483's own
// recognizer does.
//
// The `bool` modifier only changes what happens to the SURVIVING float
// rows, mirroring [lowerVectorScalar]:
//   - without `bool`, the comparison FILTERS — surviving rows keep every
//     column of the underlying float side unchanged (including its own
//     `__name__`, per that function's own doc comment on why the filter
//     path never drops the name);
//   - with `bool`, the comparison TRANSFORMS every surviving row's Value
//     to 1.0/0.0, and `__name__` is dropped exactly like every other
//     value-deriving mixed-`or` wrapper in this file's siblings.
//
// Either way the histogram-shaped rows are dropped BEFORE the predicate
// ever reads Value, the same "filter to the float-shaped rows first" step
// histogram_native_mixed_or_arithmetic.go and
// histogram_native_mixed_or_math_fn.go both take — reading a
// histogram row's placeholder Value into a live comparison would silently
// evaluate against a meaningless number instead of being rejected or
// dropped, which is exactly the hazard histogram_shape_guard.go's
// [assertValueShapedInput] exists to catch for the two GENERIC
// forwarders. This file avoids it the same way its two siblings do: by
// never reaching a generic forwarder with a Mixed node in the first
// place.
func comparisonOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || !b.Op.IsComparisonOperator() {
		return nil, "", 0, false, false, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, "", 0, false, false, false
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
		// Neither/both sides fold to a scalar — a vector-vector
		// comparison over a mixed `or` is a further, unattempted shape,
		// the same exclusion arithmeticOverMixedExpHistogramSetOp makes
		// for vector-vector arithmetic.
		return nil, "", 0, false, false, false
	}

	// expHistogramScalarOpDropsSample's histogramOnLeft parameter means
	// "the histogram-payload operand sits on the LHS of the comparison" —
	// the mirror image of scalarLeft. Every comparison op answers true
	// here regardless of side, but routing through the shared helper
	// keeps this recognizer from re-deriving the same classification
	// arithmeticOverMixedExpHistogramSetOp already centralises.
	if !expHistogramScalarOpDropsSample(b.Op, !scalarLeft) {
		return nil, "", 0, false, false, false
	}

	inner, matched := mixedExpHistogramSetOp(vecSide, s, ctx)
	if !matched {
		return nil, "", 0, false, false, false
	}
	return inner, chOp, scalarVal, scalarLeft, b.ReturnBool, true
}

// lowerComparisonOverMixedExpHistogramSetOp lowers the shape
// [comparisonOverMixedExpHistogramSetOp] recognised: build the same
// Mixed [chplan.VectorSetOp] node the root-only leaf case does
// ([lowerMixedExpHistogramSetOp]), keep only its float-shaped rows (the
// [chplan.MixedDiscriminatorColumn] is 0 there — reference's
// `vectorElemBinop` never re-admits the dropped histogram sample for any
// comparison), and answer the two shapes [lowerVectorScalar] answers for
// a bare/derived float input:
//
//   - without `bool`: a further [chplan.Filter] on the comparison
//     predicate, forwarding the float side's own canonical quartet
//     unchanged (including its `__name__` — comparison-without-bool
//     never derives a new sample, it only filters which ones survive);
//   - with `bool`: a [chplan.Project] mapping every surviving float row's
//     Value to `toFloat64(predicate)`, with `__name__` forced to ""
//     exactly like [lowerVectorScalar]'s own bool-comparison branch.
func lowerComparisonOverMixedExpHistogramSetOp(setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool bool, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
	var predicate chplan.Expr
	if scalarOnLeft {
		predicate = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		predicate = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
	}

	if !returnBool {
		filtered := &chplan.Filter{Input: floatRowsOnly, Predicate: predicate}
		return &chplan.Project{
			Input: filtered,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
				{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
				{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
				{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
			},
		}, nil
	}

	newValue := &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{predicate}}
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
