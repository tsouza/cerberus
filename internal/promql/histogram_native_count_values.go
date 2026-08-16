package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// countValuesOverExpHistogramValue recognizes count_values over any expression
// whose result has already crossed the native-histogram row boundary.
// Prometheus's aggregationCountValues stringifies histogram samples with
// FloatHistogram.String rather than dropping them, so this is a value-aware
// consumer rather than another presence-only count.
func countValuesOverExpHistogramValue(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, false
	}
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || agg.Op != parser.COUNT_VALUES {
		return nil, false
	}
	return agg, isExpHistogramValuedShape(agg.Expr, s, ctx)
}

func lowerExpHistogramCountValuesOverPlan(agg *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	label, ok := tryStringLiteral(agg.Param)
	if !ok {
		return nil, fmt.Errorf("promql: count_values requires a string-literal label name as the first arg")
	}
	if label == "" {
		return nil, fmt.Errorf("promql: count_values requires a non-empty label name")
	}
	if chplan.RowShapeOf(input) != chplan.HistogramRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: count_values native-histogram input is %T with %s row shape", input, chplan.RowShapeOf(input))
	}
	return lowerCountValuesOverPlan(agg, label, input, nativeHistogramStringExpr(s), s, ctx), nil
}

// nativeHistogramStringExpr mirrors histogram.FloatHistogram.String:
//
//	{count:<count>, sum:<sum>, <non-empty negative buckets>,
//	 <non-empty zero bucket>, <non-empty positive buckets>}
//
// HistogramProjection has already normalized every producer onto contiguous
// OTel exponential ladders. arrayEnumerate supplies the 1-based ladder index;
// adding Offset-1 recovers Prometheus's bucket index. Negative buckets are
// reversed after mapping because their numeric order runs from the most
// negative interval back toward zero.
func nativeHistogramStringExpr(s schema.Metrics) chplan.Expr {
	// clickhouse-go treats ANY `{...:...}` substring in SQL text as native
	// named-parameter syntax before it considers positional `?` bindings. A
	// literal `'{count:'` therefore makes the driver reject every ordinary
	// positional argument as an unsupported query-parameter type. Produce the
	// opening brace via char(123) so the SQL text never contains the trigger;
	// the returned value remains byte-identical to FloatHistogram.String.
	const histogramOpenBraceCode = 123

	h := histogramProjectionSchema(s)
	parts := &chplan.FuncCall{
		Fn: chplan.FnArrayConcat,
		Args: []chplan.Expr{
			nativeHistogramBucketStrings(
				&chplan.ColumnRef{Name: h.NegativeBucketCountsColumn},
				&chplan.ColumnRef{Name: h.NegativeOffsetColumn},
				&chplan.ColumnRef{Name: h.ScaleColumn},
				false,
			),
			nativeHistogramZeroBucketString(h),
			nativeHistogramBucketStrings(
				&chplan.ColumnRef{Name: h.PositiveBucketCountsColumn},
				&chplan.ColumnRef{Name: h.PositiveOffsetColumn},
				&chplan.ColumnRef{Name: h.ScaleColumn},
				true,
			),
		},
	}
	return histStringCall(
		chplan.FnConcat,
		histStringCall(chplan.FnChar, &chplan.LitInt{V: histogramOpenBraceCode}),
		&chplan.InlineString{V: "count:"},
		nativeHistogramFloatString(&chplan.ColumnRef{Name: h.CountColumn}),
		&chplan.InlineString{V: ", sum:"},
		nativeHistogramFloatString(&chplan.ColumnRef{Name: h.SumColumn}),
		histStringCall(
			chplan.FnIf,
			histStringBinary(chplan.OpGt, histStringCall(chplan.FnLength, parts), &chplan.LitInt{V: 0}),
			histStringCall(
				chplan.FnConcat,
				&chplan.InlineString{V: ", "},
				histStringCall(chplan.FnArrayStringConcat, parts, &chplan.InlineString{V: ", "}),
			),
			&chplan.InlineString{V: ""},
		),
		&chplan.InlineString{V: "}"},
	)
}

func nativeHistogramBucketStrings(buckets, offset, scale chplan.Expr, positive bool) chplan.Expr {
	const bucketIndexParam = "hbi"
	index := &chplan.BareIdent{Name: bucketIndexParam}
	count := &chplan.Subscript{Container: buckets, Key: index}
	idx := histStringBinary(
		chplan.OpAdd,
		offset,
		histStringBinary(chplan.OpSub, index, &chplan.LitInt{V: 1}),
	)
	lowerBound := nativeHistogramBoundExpr(
		histStringBinary(chplan.OpSub, idx, &chplan.LitInt{V: 1}), scale,
	)
	upperBound := nativeHistogramBoundExpr(idx, scale)
	if !positive {
		lowerBound, upperBound = histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, upperBound),
			histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, lowerBound)
	}
	left, right := "(", "]"
	if !positive {
		left, right = "[", ")"
	}
	body := histStringCall(
		chplan.FnConcat,
		&chplan.InlineString{V: left},
		nativeHistogramFloatString(lowerBound),
		&chplan.InlineString{V: ","},
		nativeHistogramFloatString(upperBound),
		&chplan.InlineString{V: right + ":"},
		nativeHistogramFloatString(count),
	)
	keptIndices := histStringCall(
		chplan.FnArrayFilter,
		&chplan.Lambda{
			Params: []string{bucketIndexParam},
			Body: histStringBinary(
				chplan.OpNe,
				&chplan.Subscript{Container: buckets, Key: &chplan.BareIdent{Name: bucketIndexParam}},
				&chplan.LitFloat{V: 0},
			),
		},
		histStringCall(chplan.FnArrayEnumerate, buckets),
	)
	mapped := histStringCall(
		chplan.FnArrayMap,
		&chplan.Lambda{Params: []string{bucketIndexParam}, Body: body},
		keptIndices,
	)
	if positive {
		return mapped
	}
	return histStringCall(chplan.FnArrayReverse, mapped)
}

func nativeHistogramZeroBucketString(s schema.Metrics) chplan.Expr {
	zeroCount := &chplan.ColumnRef{Name: s.ZeroCountColumn}
	threshold := &chplan.ColumnRef{Name: s.ZeroThresholdColumn}
	value := histStringCall(
		chplan.FnConcat,
		&chplan.InlineString{V: "[-"},
		nativeHistogramFloatString(threshold),
		&chplan.InlineString{V: ","},
		nativeHistogramFloatString(threshold),
		&chplan.InlineString{V: "]:"},
		nativeHistogramFloatString(zeroCount),
	)
	return histStringCall(
		chplan.FnIf,
		histStringBinary(chplan.OpNe, zeroCount, &chplan.LitFloat{V: 0}),
		histStringCall(chplan.FnArray, value),
		histStringCall(chplan.FnArray),
	)
}

func nativeHistogramBoundExpr(index, scale chplan.Expr) chplan.Expr {
	base := histStringCall(
		chplan.FnPow,
		&chplan.LitFloat{V: 2},
		histStringCall(chplan.FnPow, &chplan.LitFloat{V: 2}, histStringBinary(chplan.OpMul, &chplan.LitFloat{V: -1}, scale)),
	)
	return histStringCall(chplan.FnPow, base, index)
}

// nativeHistogramFloatString is the FloatHistogram.String `%g` spelling for
// the special values ClickHouse's toString names differently. Finite values
// use ClickHouse's shortest round-trip representation, which matches Go's
// shortest `%g` representation over the OTel exponential-histogram domain.
func nativeHistogramFloatString(value chplan.Expr) chplan.Expr {
	return histStringCall(
		chplan.FnMultiIf,
		histStringCall(chplan.FnIsNaN, value), &chplan.InlineString{V: "NaN"},
		histStringBinary(
			chplan.OpAnd,
			histStringCall(chplan.FnIsInfinite, value),
			histStringBinary(chplan.OpGt, value, &chplan.LitFloat{V: 0}),
		), &chplan.InlineString{V: "+Inf"},
		histStringCall(chplan.FnIsInfinite, value), &chplan.InlineString{V: "-Inf"},
		histStringCall(chplan.FnToString, value),
	)
}

func histStringCall(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: args}
}

func histStringBinary(op chplan.BinaryOp, left, right chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: op, Left: left, Right: right}
}
