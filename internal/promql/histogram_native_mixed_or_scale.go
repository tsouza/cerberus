package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_scale.go lowers a scalar `*` or histogram-left
// `/` directly wrapping a mixed float/histogram `or`
// (histogram_native_mixed_or.go's own #2330/#2335 shape) — the sub-family
// of scalar arithmetic ops histogram_native_mixed_or_arithmetic.go's own
// header explicitly named as out of its scope, because those two ops
// SCALE a histogram-valued sample rather than dropping it. Cerberus issue
// #2449, its sixth wrapper family and the first to touch a histogram-
// shaped ROW's actual payload instead of either forwarding it unread
// (`label_replace`/`label_join`, #2449's first pass) or dropping it
// (single-arg math functions and the ADD/SUB/POW/MOD/ATAN2/scalar-left-DIV
// arithmetic ops, #2449's second and third passes).
//
// Reference Prometheus semantics (verified against the vendored fork's
// promql/engine.go, NOT assumed from this issue's own acceptance text):
// vectorElemBinop's `hlhs != nil && hrhs == nil` switch answers
//
//	case parser.MUL: return 0, hlhs.Copy().Mul(rhs).Compact(0), true, nil, nil
//	case parser.DIV: return 0, hlhs.Copy().Div(rhs).Compact(0), true, nil, nil
//
// for MUL (either operand order) and histogram-left DIV — `FloatHistogram
// .Mul`/`.Div` scale the five COUNT-bearing fields (Count, Sum, ZeroCount,
// both signed bucket ladders) and leave Scale, ZeroThreshold and both
// bucket offsets alone, exactly the split
// histogram_native_scalar_binop.go's [scaleHistogramProjection] already
// applies for a BARE histogram-valued operand (cerberus issue #2087).
// Scalar-left DIV (`2 / (a or b)`) is NOT this shape — division is not
// commutative, and reference's own switch keys off which OPERAND carries
// the histogram — so it stays classified as drop-family and handled by
// histogram_native_mixed_or_arithmetic.go's existing recognizer
// (`expHistogramScalarOpDropsSample(parser.DIV, histogramOnLeft=false)`
// answers true).
//
// Why this needs its own lowering rather than reusing
// [scaleHistogramProjection] directly: that helper's Project reads the
// nine Histogram*Column fields off ITS OWN Input by name and republishes
// exactly thirteen columns, capped by a fresh [chplan.HistogramProjection]
// — the correct shape for a BARE histogram result, but the Mixed
// VectorSetOp this file scales publishes those same nine names ALONGSIDE
// a live Value and the trailing [chplan.MixedDiscriminatorColumn] (see
// mixedExpHistogramSetOp's own Mixed contract), and the decode side
// ([internal/chclient/cursor.go]'s shapeSampleMixed) requires that exact
// fourteen-column, fourteen-name shape back — capping the scale with a
// [chplan.HistogramProjection] would drop Value and the discriminator and
// resolve to the wrong probe shape entirely. This lowering builds its own
// Project instead, in the fourteen-column order shapeSampleMixed's scan
// pins, reusing [scaleHistogramScalarExpr] / [scaleHistogramLadderExpr]
// (histogram_native_scalar_binop.go) unchanged for the per-field fold so
// the two lowerings can never drift on the five-vs-four field split.
//
// Why applying the scale expression to BOTH arms unconditionally is
// correct rather than a hazard: unlike a drop-family wrapper (which must
// filter out the histogram-shaped rows before a predicate ever reads
// Value — see histogram_native_mixed_or_arithmetic.go /
// histogram_native_mixed_or_comparison.go's own headers for exactly that
// hazard), scaling touches every row's OWN payload and leaves the OTHER
// payload's placeholder untouched in shape, only in magnitude:
//   - On a float-shaped row, Value is real and gets `scalar OP Value` /
//     `Value OP scalar` for real — exactly [lowerVectorScalar]'s own
//     rewrite for a bare/derived float input — while the nine
//     Histogram*Column placeholders
//     ([mixedVectorSetOpHistogramPlaceholderCols]: zero floats/ints, empty
//     arrays) get scaled too, but decode never reads them on a
//     discriminator-0 row, so a scaled placeholder is exactly as
//     meaningless as an unscaled one.
//   - On a histogram-shaped row, the nine Histogram*Column fields are real
//     and get scaled for real; Value is the [HistogramProjection]
//     placeholder ([histogramSampleValuePlaceholder]) and gets scaled into
//     a DIFFERENT placeholder value, but decode never reads Value on a
//     discriminator-1 row either.
//
// So no [chplan.Case]/CH `if()` keyed on the discriminator is needed here
// — the issue's own suggested mechanism for a fully general passthrough
// forwarder — because the two column sets this recognizer touches
// (Value; the nine Histogram*Column fields) are already disjoint in which
// row-shape reads them for real. A discriminator-keyed conditional would
// still be needed for a wrapper whose transform reads the SAME output
// column under two different real interpretations depending on payload
// (vector-vector arithmetic/comparisons — #2449's other remaining piece,
// deliberately not attempted here; see this issue's own tracking in
// test/rejection-parity/catalogue).
func mulOrDivScaleOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.MUL && b.Op != parser.DIV) {
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
		// Neither/both sides fold to a scalar — a vector-vector `*`/`/`
		// over a mixed `or` is the further, unattempted shape this file's
		// header names.
		return nil, "", 0, false, false
	}

	if b.Op == parser.DIV && scalarLeft {
		// `<scalar> / (a or b)` — scalar-left DIV is drop-family
		// (histogram_native_mixed_or_arithmetic.go), not this scaling
		// shape: DIV only scales when the histogram/vector operand is the
		// numerator.
		return nil, "", 0, false, false
	}

	inner, matched := mixedExpHistogramSetOp(vecSide, s, ctx)
	if !matched {
		return nil, "", 0, false, false
	}
	return inner, chOp, scalarVal, scalarLeft, true
}

// lowerMulOrDivScaleOverMixedExpHistogramSetOp lowers the shape
// [mulOrDivScaleOverMixedExpHistogramSetOp] recognised: build the same
// Mixed [chplan.VectorSetOp] node the root-only leaf case does
// ([lowerMixedExpHistogramSetOp]), then re-project ALL fourteen of its
// columns in the exact order [internal/chclient/cursor.go]'s
// shapeSampleMixed scan pins — MetricName, Attributes, Timestamp, Value,
// the nine Histogram*Column fields, the discriminator — scaling Value by
// `scalar OP Value` / `Value OP scalar` (mirrors [lowerVectorScalar]) and
// the nine histogram fields by [scaleHistogramScalarExpr] /
// [scaleHistogramLadderExpr]'s five-vs-four field split (mirrors
// [scaleHistogramProjection]), and forwarding Attributes, Timestamp and
// the discriminator unchanged.
//
// MetricName is forced to "" rather than forwarded: reference's
// `changesMetricSchema` answers true for MUL and DIV, so Prom's own
// DropName rule always strips `__name__` from an arithmetic-derived
// sample — mirrors [lowerArithmeticOverMixedExpHistogramSetOp]'s
// identical projection.
func lowerMulOrDivScaleOverMixedExpHistogramSetOp(setOp *parser.BinaryExpr, op chplan.BinaryOp, scalar float64, scalarOnLeft bool, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lowerMixedExpHistogramSetOp(setOp, s, ctx)
	if err != nil {
		return nil, err
	}

	scale := chplan.Expr(&chplan.LitFloat{V: scalar})
	valueRef := chplan.Expr(&chplan.ColumnRef{Name: s.ValueColumn})
	var valueExpr chplan.Expr
	if scalarOnLeft {
		valueExpr = &chplan.Binary{Op: op, Left: scale, Right: valueRef}
	} else {
		valueExpr = &chplan.Binary{Op: op, Left: valueRef, Right: scale}
	}

	scalarField := func(col string) chplan.Projection {
		return chplan.Projection{
			Expr:  scaleHistogramScalarExpr(op, &chplan.ColumnRef{Name: col}, scale),
			Alias: col,
		}
	}
	ladderField := func(col string) chplan.Projection {
		return chplan.Projection{
			Expr:  scaleHistogramLadderExpr(op, &chplan.ColumnRef{Name: col}, scale),
			Alias: col,
		}
	}
	forwarded := func(col string) chplan.Projection {
		return chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col}
	}

	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			forwarded(s.AttributesColumn),
			forwarded(s.TimestampColumn),
			{Expr: valueExpr, Alias: s.ValueColumn},
			scalarField(chplan.HistogramCountColumn),
			scalarField(chplan.HistogramSumColumn),
			forwarded(chplan.HistogramScaleColumn),
			forwarded(chplan.HistogramZeroThresholdColumn),
			scalarField(chplan.HistogramZeroCountColumn),
			forwarded(chplan.HistogramPositiveOffsetColumn),
			ladderField(chplan.HistogramPositiveBucketCountsColumn),
			forwarded(chplan.HistogramNegativeOffsetColumn),
			ladderField(chplan.HistogramNegativeBucketCountsColumn),
			forwarded(mixedDiscriminatorColumn),
		},
	}, nil
}
