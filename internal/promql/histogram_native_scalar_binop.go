package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_scalar_binop.go answers every scalar binary operation
// over a native (OTel exponential) histogram shape. `*` and histogram-left
// `/` scale the histogram (#2087); incompatible operations drop the sample
// and produce an empty result (#2189), matching reference Prometheus.
//
// Reference Prometheus answers scalar `*` / `/` over a native-histogram
// sample histogram-valued (promql/engine.go's `vectorElemBinop`):
//
//	case hlhs != nil && hrhs == nil:
//	    case parser.MUL: return 0, hlhs.Copy().Mul(rhs).Compact(0), true, nil, nil
//	    case parser.DIV: return 0, hlhs.Copy().Div(rhs).Compact(0), true, nil, nil
//
// `FloatHistogram.Mul` / `.Div` scale exactly the five COUNT-bearing
// fields — Count, Sum, ZeroCount, and both signed bucket ladders — and
// leave Scale, ZeroThreshold and both bucket offsets alone: those
// describe where the buckets SIT on the value axis, not how much fell
// into them. [scaleHistogramProjection] is that same five-field split,
// generalised from [expHistogramAvgScaleProjections]'s "divide by the
// group's member count" to "scale by an arbitrary PromQL scalar and
// operator" — see that function's doc for why it is factored this way.
//
// In the same reference switch, `+` / `-` / `^` / `%`, the comparison
// family and `atan2` return NewIncompatibleTypesInBinOpInfo and drop the
// sample rather than answering a value. Scalar-left `/` is incompatible
// for the same reason. [expHistogramDroppingScalarBinop] recognises those
// shapes and [lowerExpHistogramScalarBinop]'s drop path preserves their
// own lowering errors before applying a constant-false filter.
//
// This shape composes with the three shipped histogram-VALUED
// lowerings — bare selector (histogram_native_bare.go), sum()/avg()
// (histogram_native_sum.go), and histogram-valued range functions
// (histogram_native_range_fn.go)
// — rather than replacing any of them: [lowerExpHistogramScalarBinop]
// lowers the histogram side through its own existing recognizer and
// lowering pair unchanged, then rewrites the capping
// [chplan.HistogramProjection]'s Input to scale it. `rate()` already
// scales a histogram by a scalar for its own per-second division, but
// does so by composing with the window fold
// (expHistogramValuedWindowFold in histogram_native_range_fn.go) rather than
// this mechanism: a bare selector and `sum()` have no fold in their
// plan for a PromQL-supplied scalar to ride along with, which is why
// this file exists as a THIRD, general-purpose scale layer instead of
// widening the fold.
//
// A `*parser.BinaryExpr` root needs its own dispatch line in
// [lowerRoot]: that function recognises a selector, an aggregation and a
// range-vector call, and a binary expression is none of those, so
// `latency_exp_hist * 2` previously descended through lowerBinary →
// lowerVectorSelector and was rejected before any histogram-aware code
// saw it.

// expHistogramScalarBinop recognises `<exp-hist shape> (*|/) <scalar
// literal>` or its commutative `<scalar> * <exp-hist shape>` sibling,
// and returns the histogram-side sub-expression, the chplan operator,
// and the scalar as a chplan literal.
//
// DIV is not commutative — `scalar / <exp-hist shape>` is not this
// shape's mirror (reference's own switch keys off which OPERAND carries
// the histogram, and a histogram can only be the numerator here) — so
// only the `<exp-hist shape> / <scalar>` order is recognised for it.
func expHistogramScalarBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide parser.Expr, op chplan.BinaryOp, scale chplan.Expr, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, "", nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.MUL && b.Op != parser.DIV) {
		return nil, "", nil, false
	}
	chOp, err := promBinaryOp(b.Op)
	if err != nil {
		return nil, "", nil, false
	}
	if b.Op == parser.MUL {
		if lv, isScalar := tryScalarLiteral(b.LHS); isScalar && isExpHistogramValuedShape(b.RHS, s, ctx) {
			return b.RHS, chOp, &chplan.LitFloat{V: lv}, true
		}
	}
	if rv, isScalar := tryScalarLiteral(b.RHS); isScalar && isExpHistogramValuedShape(b.LHS, s, ctx) {
		return b.LHS, chOp, &chplan.LitFloat{V: rv}, true
	}
	return nil, "", nil, false
}

// expHistogramDroppingScalarBinop recognises the scalar/histogram operand
// pairs for which reference Prometheus emits an incompatible-types info
// annotation and drops the histogram sample. The scalar may sit on either
// side; division is included only when the scalar is on the left because
// histogram/scalar division is the supported scaling shape above.
func expHistogramDroppingScalarBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide parser.Expr, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin {
		return nil, false
	}
	if _, isScalar := tryScalarLiteral(b.LHS); isScalar &&
		isExpHistogramValuedShape(b.RHS, s, ctx) && expHistogramScalarOpDropsSample(b.Op, false) {
		return b.RHS, true
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar &&
		isExpHistogramValuedShape(b.LHS, s, ctx) && expHistogramScalarOpDropsSample(b.Op, true) {
		return b.LHS, true
	}
	return nil, false
}

func expHistogramScalarOpDropsSample(op parser.ItemType, histogramOnLeft bool) bool {
	switch op {
	case parser.ADD, parser.SUB, parser.POW, parser.MOD,
		parser.EQLC, parser.NEQ, parser.GTR, parser.LSS, parser.GTE, parser.LTE,
		parser.ATAN2:
		return true
	case parser.DIV:
		return !histogramOnLeft
	default:
		return false
	}
}

// unwrapBinaryExpr peels the transparent ParenExpr / StepInvariantExpr
// wrappers the parser can put between the root and a binary expression —
// `(latency_exp_hist * 2)` — and reports the BinaryExpr underneath.
// Mirrors [unwrapVectorSelector] / [unwrapAggregateExpr], which do the
// same for a selector / an aggregation.
func unwrapBinaryExpr(e parser.Expr) (*parser.BinaryExpr, bool) {
	for {
		switch v := e.(type) {
		case *parser.BinaryExpr:
			return v, true
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return nil, false
		}
	}
}

// isExpHistogramValuedShape reports whether expr is one of the three shapes
// that answer histogram-valued: a bare exp-histogram selector,
// sum()/avg() over one, or a supported histogram-valued range function over
// one.
func isExpHistogramValuedShape(expr parser.Expr, s schema.Metrics, ctx lowerCtx) bool {
	if call, ok := labelCallOverExpHistogram(expr, s, ctx); ok {
		return isExpHistogramValuedShape(call.Args[0], s, ctx)
	}
	if _, ok := bareExpHistogramSelector(expr, s, ctx); ok {
		return true
	}
	if _, _, ok := sumOrAvgOverExpHistogram(expr, s, ctx); ok {
		return true
	}
	if _, ok := rangeFnOverExpHistogram(expr, s, ctx); ok {
		return true
	}
	if agg, ok := mergeableExpHistogramAggregate(expr); ok {
		return isExpHistogramValuedShape(agg.Expr, s, ctx)
	}
	if b, ok := unwrapBinaryExpr(expr); ok {
		if b.Op == parser.MUL {
			if _, scalar := tryScalarLiteral(b.LHS); scalar && isExpHistogramValuedShape(b.RHS, s, ctx) {
				return true
			}
		}
		if (b.Op == parser.MUL || b.Op == parser.DIV) && isExpHistogramValuedShape(b.LHS, s, ctx) {
			_, scalar := tryScalarLiteral(b.RHS)
			return scalar
		}
	}
	return false
}

// lowerExpHistogramScalarBinop lowers the shape either scalar-binop
// recognizer accepted. It defers entirely to the histogram side's own
// shipped lowering — unmodified, so a query without the scalar wrapper
// still lowers byte-for-byte the same way. drop selects reference's
// keep=false incompatible-types result; otherwise the capping
// HistogramProjection is rewritten to scale it.
//
// extraPassthrough only differs from empty for the bare-selector range
// shapes, which read the step-grid anchor off the row. sum()/avg() and the
// histogram-valued range functions already carry
// stepGridAnchorColumn in their own Input's output when it applies —
// [expHistogramGroupMerge] / [expHistogramWindowReshape] project it
// themselves — so neither needs it repeated here.
func lowerExpHistogramScalarBinop(histSide parser.Expr, op chplan.BinaryOp, scale chplan.Expr, s schema.Metrics, ctx lowerCtx, drop bool) (chplan.Node, error) {
	node, matched, err := lowerExpHistogramValuedShape(histSide, s, ctx)
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram scalar-binop matched no known histogram-valued shape for %v", histSide)
	}
	if err != nil {
		return nil, err
	}
	hp, ok := node.(*chplan.HistogramProjection)
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram lowering did not cap with *chplan.HistogramProjection, got %T", node)
	}
	if drop {
		// Reference returns keep=false plus an incompatible-types info
		// annotation. Cerberus has no info-annotation wire channel, but
		// matches the accepted empty result.
		return &chplan.Filter{Input: hp, Predicate: &chplan.LitBool{V: false}}, nil
	}
	return scaleHistogramProjection(hp, op, scale, s), nil
}

// scaleHistogramProjection rewrites hp's Input so the row it publishes
// is scaled by `scale` under `op` — the same five-field split
// [expHistogramAvgScaleProjections] applies for avg()'s "divide by the
// group's member count", generalised to an arbitrary scalar expression
// and operator (cerberus issue #2087).
//
// extraPassthrough names any columns beyond the closed set every
// HistogramProjection.Input is already guaranteed to expose — Attributes,
// Scale, ZeroThreshold (if the schema persists it), both bucket offsets,
// and the five count-bearing fields — that THIS caller's specific
// lowering shape also needs carried through unscaled (the step-grid
// anchor for the bare-selector range shape; see
// [lowerExpHistogramScalarBinop]'s doc).
func scaleHistogramProjection(hp *chplan.HistogramProjection, op chplan.BinaryOp, scale chplan.Expr, s schema.Metrics, extraPassthrough ...string) *chplan.HistogramProjection {
	histSchema := histogramProjectionSchema(s)
	passthroughCols := []string{
		s.AttributesColumn,
		s.TimestampColumn,
		histSchema.ScaleColumn,
		histSchema.ZeroThresholdColumn,
		histSchema.PositiveOffsetColumn,
		histSchema.NegativeOffsetColumn,
	}

	projs := make([]chplan.Projection, 0, len(passthroughCols)+1+5)
	for _, col := range passthroughCols {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: col})
	}
	for _, col := range []string{histSchema.CountColumn, histSchema.SumColumn, histSchema.ZeroCountColumn} {
		projs = append(projs, chplan.Projection{
			Expr:  scaleHistogramScalarExpr(op, &chplan.ColumnRef{Name: col}, scale),
			Alias: col,
		})
	}
	for _, col := range []string{histSchema.PositiveBucketCountsColumn, histSchema.NegativeBucketCountsColumn} {
		projs = append(projs, chplan.Projection{
			Expr:  scaleHistogramLadderExpr(op, &chplan.ColumnRef{Name: col}, scale),
			Alias: col,
		})
	}

	return nativeHistogramProjection(
		&chplan.Project{Input: hp, Projections: projs},
		&chplan.LitString{V: ""},
		&chplan.ColumnRef{Name: s.TimestampColumn},
		histSchema,
	)
}

// scaleHistogramScalarExpr renders `e OP scale` for one scalar
// count-bearing field (Count, Sum, ZeroCount).
func scaleHistogramScalarExpr(op chplan.BinaryOp, e, scale chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: op, Left: e, Right: scale}
}

// scaleHistogramLadderExpr renders an element-wise `arrayMap(b -> b OP
// scale, e)` for one signed bucket ladder — reference's `for i := range
// h.PositiveBuckets { h.PositiveBuckets[i] OP= scalar }`, expressed as a
// single array expression rather than a per-element loop.
func scaleHistogramLadderExpr(op chplan.BinaryOp, e, scale chplan.Expr) chplan.Expr {
	const paramBucket = "b"
	return &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramBucket},
				// BareIdent, not ColumnRef: a lambda parameter is bound by
				// the lambda, not read off a row — every other
				// exp-histogram bucket expression in this package spells
				// it that way (see expHistogramBucketRowContribExpr).
				Body: scaleHistogramScalarExpr(op, &chplan.BareIdent{Name: paramBucket}, scale),
			},
			e,
		},
	}
}
