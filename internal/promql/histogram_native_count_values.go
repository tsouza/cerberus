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
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || agg.Op != parser.COUNT_VALUES {
		return nil, false
	}
	// A rejection answers the zero-value tuple, never a
	// partially-populated one — the contract every sibling exp-histogram
	// recognizer keeps. Until cerberus issue #2963 the copied availability
	// guard this function opened with kept it here by accident; the
	// explicit rejection keeps it on purpose.
	if !isExpHistogramValuedShape(agg.Expr, s, ctx) {
		return nil, false
	}
	return agg, true
}

func lowerExpHistogramCountValuesOverPlan(agg *parser.AggregateExpr, input chplan.Node, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	label, ok := tryStringLiteral(agg.Param)
	if !ok {
		return nil, fmt.Errorf("promql: count_values requires a string-literal label name as the first arg")
	}
	if label == "" {
		return nil, fmt.Errorf("promql: count_values requires a non-empty label name")
	}
	if kind := input.RowType().SampleKind(); kind != chplan.SampleKindHistogram {
		return nil, fmt.Errorf("promql: internal invariant violated: count_values native-histogram input is %T with %s sample kind", input, kind)
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
// adding it to Offset recovers the exponent of the bucket's upper edge.
// Negative buckets are
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
		index,
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
	const thresholdStringParam = "hgts"
	zeroCount := &chplan.ColumnRef{Name: s.ZeroCountColumn}
	threshold := &chplan.ColumnRef{Name: s.ZeroThresholdColumn}
	// The threshold's formatted string is read twice — once for the "[-"
	// edge, once for the "," edge — and nativeHistogramFloatString's own
	// tree is large even after nativeHistogramShortestGString's hqLet
	// binding, so hqLet it here too rather than rendering two independent
	// copies of the same value's formatting.
	value := hqLet(thresholdStringParam, nativeHistogramFloatString(threshold), func(thresholdStr chplan.Expr) chplan.Expr {
		return histStringCall(
			chplan.FnConcat,
			&chplan.InlineString{V: "[-"},
			thresholdStr,
			&chplan.InlineString{V: ","},
			thresholdStr,
			&chplan.InlineString{V: "]:"},
			nativeHistogramFloatString(zeroCount),
		)
	})
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
// `value`: `fmt.Sprintf("%g", value)` — the hardcoded NaN/±Inf cases, then
// `strconv.FormatFloat(f, 'g', -1, 64)` with no further embellishment (no
// OpenMetrics ".0" suffix; see openMetricsFloatExpr for that variant).
// Finite values borrow ClickHouse's shortest round-trip digits and relay
// them out under Go's layout rule via nativeHistogramShortestGString.
func nativeHistogramFloatString(value chplan.Expr) chplan.Expr {
	// `value` is read six times below (isNaN, isInfinite twice, the sign
	// compare, and twice more inside nativeHistogramShortestGString's own
	// outer binding) — cheap when a caller passes a plain ColumnRef, but
	// callers like nativeHistogramBucketStrings pass lowerBound/upperBound,
	// each its own two-level `pow(pow(...), ...)` expression
	// (nativeHistogramBoundExpr). Left unbound, six mentions of THAT
	// expression is six independent copies of it in the emitted SQL — see
	// nativeHistogramShortestGString's own doc on why that class of
	// repetition matters here. hqLet binds it once instead.
	const floatValueParam = "hgfv"
	return hqLet(floatValueParam, value, func(v chplan.Expr) chplan.Expr {
		return histStringCall(
			chplan.FnMultiIf,
			histStringCall(chplan.FnIsNaN, v), &chplan.InlineString{V: "NaN"},
			histStringBinary(
				chplan.OpAnd,
				histStringCall(chplan.FnIsInfinite, v),
				histStringBinary(chplan.OpGt, v, &chplan.LitFloat{V: 0}),
			), &chplan.InlineString{V: "+Inf"},
			histStringCall(chplan.FnIsInfinite, v), &chplan.InlineString{V: "-Inf"},
			nativeHistogramShortestGString(v),
		)
	})
}

// nativeHistogramShortestGString renders the finite-value spelling of Go's
// `strconv.FormatFloat(f, 'g', -1, 64)` from ClickHouse's own shortest
// round-trip digits — the layout half of [shortestGExpr], with no suffix
// on the fixed branch.
//
// Zero is steered into the fixed branch before the digit-parsing logic
// ever sees it: its digit string is all zeros, which the decomposition's
// leading/trailing-zero strip reduces to "", and answering "0" here is
// cheaper than special-casing an empty digit string inside the mantissa.
func nativeHistogramShortestGString(value chplan.Expr) chplan.Expr {
	return shortestGExpr(value, value, func(g shortestGParts) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnMultiIf, Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpEq, Left: g.value, Right: &chplan.LitFloat{V: 0}},
			&chplan.InlineString{V: "0"},
			g.useSci, g.sci,
			&chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{g.sign, g.rendered}},
		}}
	})
}

func histStringCall(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: args}
}

func histStringBinary(op chplan.BinaryOp, left, right chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: op, Left: left, Right: right}
}
