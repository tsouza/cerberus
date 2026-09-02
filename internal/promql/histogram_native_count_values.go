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
// round-trip digits (`toString(Float64)`). CH already produces the same
// digits Go's shortest mode does — the two disagree only on LAYOUT:
//
//   - CH switches to scientific notation at a different magnitude threshold
//     than Go does, and spells the exponent differently (`1e-7` / `1e21`
//     where Go writes `1e-07` / `1e+21`).
//   - Go's shortest `%g` (`internal/strconv/ftoa.go` pins the decision
//     precision `eprec` to 6 whenever the requested precision is "shortest")
//     uses scientific notation exactly when the decimal exponent falls
//     outside `[-4, 6)` — equivalently when the magnitude leaves
//     `[1e-4, 1e6)`. That is a narrower window than `[-4, 21)`, so numbers
//     CH still renders in fixed notation (e.g. `1009800`) must be relaid
//     out as scientific (`1.0098e+06`) here.
//
// The expression reads the digits and the decimal exponent back out of CH's
// rendering — exactly, from the string, never via log10 — so the `[-4, 6)`
// boundary cannot be misclassified by a log rounding error.
//
// `value` and CH's rendering of it are each mentioned once, as the two
// elements of a single-element `arrayMap`: every other mention below is a
// lambda parameter (`v`/`u`), the same binding trick openMetricsFloatExpr
// uses and for the same reason — `digits()` alone is referenced three times
// while building the scientific mantissa, and `mantRaw()`/`u()` are each
// referenced by several of the helpers above it, so inlining the raw
// sub-expressions instead of binding them re-expands the tree at every
// mention. Left unbound, that repetition compounds multiplicatively across
// the ~6 nested layers between `value` and the top-level `multiIf` — enough
// to blow ClickHouse's `max_query_size` on a single histogram bucket cell,
// which is why every intermediate quantity below is bound with [hqLet]
// exactly once rather than re-derived at each mention: hqLet renders
// `arrayMap(<param> -> <body>, array(<val>))[1]`, ClickHouse's spelling of a
// let-binding, so `mantRaw`, say, appears ONCE in the emitted SQL no matter
// how many of the helpers below read it. A caller that mentions this
// function's own result several times — nativeHistogramStringExpr calls it
// eleven times over across Count, Sum and every bucket's lower/upper/count —
// still renders eleven independent copies of the WHOLE chain; that
// duplication is inherent (each call receives a different `value`) and is
// not what this binding fixes. What it fixes is each of those eleven copies
// no longer being its own multiplicative blowup on top of that.
func nativeHistogramShortestGString(value chplan.Expr) chplan.Expr {
	const (
		sciLowerBound = 1e-4
		sciUpperBound = 1e6
		// Exponents below this get a leading zero: Go writes at least two
		// exponent digits ("1e-05", never "1e-5").
		expPadBelow = 10
		// Lambda parameter names: the value and CH's own string rendering
		// of its magnitude.
		valueParam  = "hgv"
		digitsParam = "hgu"
		// hqLet binding names for the intermediate quantities below.
		// Distinct from valueParam/digitsParam and from every other hqLet
		// binding in this package (see histogram_quantile_window.go's own
		// naming note) because these nest inside the value/digits lambda.
		posParam         = "hge"
		mantRawParam     = "hgm"
		digitsAllParam   = "hgda"
		digitsLeadParam  = "hgdl"
		digitsFinalParam = "hgd"
		pointPosParam    = "hgp"
		intLenParam      = "hgi"
		expValParam      = "hgx"
	)

	call := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: fn, Args: args}
	}
	str := func(v string) chplan.Expr { return &chplan.InlineString{V: v} }
	i := func(v int64) chplan.Expr { return &chplan.LitInt{V: v} }
	f := func(v float64) chplan.Expr { return &chplan.LitFloat{V: v} }
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}
	countOf := func(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
		return call(chplan.FnToInt64, call(fn, args...))
	}

	v := &chplan.BareIdent{Name: valueParam}
	u := &chplan.BareIdent{Name: digitsParam}

	// Position of the exponent marker in CH's rendering; 0 when CH chose
	// fixed notation.
	epos := countOf(chplan.FnStringPosition, u, str("e"))
	body := hqLet(posParam, epos, func(pos chplan.Expr) chplan.Expr {
		// The mantissa CH rendered — the whole string in fixed notation.
		mantRaw := call(chplan.FnIf, bin(chplan.OpGt, pos, i(0)),
			call(chplan.FnSubstring, u, i(1), bin(chplan.OpSub, pos, i(1))),
			u)
		return hqLet(mantRawParam, mantRaw, func(mr chplan.Expr) chplan.Expr {
			// Mantissa digits with the decimal point removed, then with
			// leading and trailing zeros stripped: the significant digits,
			// most significant first.
			digitsAll := call(chplan.FnReplaceAll, mr, str("."), str(""))
			return hqLet(digitsAllParam, digitsAll, func(da chplan.Expr) chplan.Expr {
				digitsLead := call(chplan.FnRegexReplaceFirst, da, str("^0+"), str(""))
				return hqLet(digitsLeadParam, digitsLead, func(dl chplan.Expr) chplan.Expr {
					digits := call(chplan.FnRegexReplaceFirst, dl, str("0+$"), str(""))
					return hqLet(digitsFinalParam, digits, func(d chplan.Expr) chplan.Expr {
						pointPos := countOf(chplan.FnStringPosition, mr, str("."))
						return hqLet(pointPosParam, pointPos, func(pp chplan.Expr) chplan.Expr {
							// Digit count left of the decimal point (the
							// whole mantissa when there is no point).
							intLen := call(chplan.FnIf, bin(chplan.OpGt, pp, i(0)),
								bin(chplan.OpSub, pp, i(1)),
								countOf(chplan.FnLength, mr))
							return hqLet(intLenParam, intLen, func(il chplan.Expr) chplan.Expr {
								// The decimal exponent: read straight off
								// CH's exponent when it used scientific
								// notation, else derived from where the
								// first significant digit sits relative to
								// the decimal point. FnToInt64OrZero, not
								// the throwing FnToInt64: ClickHouse's
								// vectorized `if` does not reliably skip
								// evaluating the untaken branch inside this
								// deep an arrayMap/hqLet nesting, so a
								// fixed-notation row (pos = 0, this branch
								// never SELECTED) can still have its
								// substring — the whole digit string, not
								// an exponent suffix — pushed through
								// toInt64 and abort the query on a string
								// like "2.565". The OrZero result is
								// discarded exactly when that happens
								// (pos > 0 is false), so the 0 fallback
								// never reaches the output.
								leadingZeros := bin(chplan.OpSub, countOf(chplan.FnLength, da), countOf(chplan.FnLength, dl))
								expVal := call(chplan.FnIf, bin(chplan.OpGt, pos, i(0)),
									call(chplan.FnToInt64OrZero, call(chplan.FnSubstring, u, bin(chplan.OpAdd, pos, i(1)))),
									bin(chplan.OpSub, bin(chplan.OpSub, il, leadingZeros), i(1)))
								return hqLet(expValParam, expVal, func(ev chplan.Expr) chplan.Expr {
									// `d` or `d.ddd` — Go's normalised
									// scientific mantissa.
									mantissa := call(chplan.FnIf, bin(chplan.OpLe, countOf(chplan.FnLength, d), i(1)),
										d,
										call(chplan.FnConcat, call(chplan.FnSubstring, d, i(1), i(1)), str("."), call(chplan.FnSubstring, d, i(2))))
									expDigits := call(chplan.FnToString, call(chplan.FnAbs, ev))
									expSuffix := call(chplan.FnConcat,
										call(chplan.FnIf, bin(chplan.OpLt, ev, i(0)), str("-"), str("+")),
										call(chplan.FnIf, bin(chplan.OpLt, call(chplan.FnAbs, ev), i(expPadBelow)),
											call(chplan.FnConcat, str("0"), expDigits),
											expDigits))
									sign := call(chplan.FnIf, bin(chplan.OpLt, v, f(0)), str("-"), str(""))
									sci := call(chplan.FnConcat, sign, mantissa, str("e"), expSuffix)
									fixed := call(chplan.FnConcat, sign, u)

									return call(chplan.FnMultiIf,
										// Zero's digit string is all zeros,
										// which the leading/trailing-zero
										// strip above reduces to "" — steer
										// it into `fixed` ("0") before the
										// digit-parsing logic ever sees it,
										// rather than special-casing an
										// empty `digits` inside `mantissa`.
										bin(chplan.OpEq, v, f(0)), str("0"),
										bin(chplan.OpOr,
											bin(chplan.OpLt, call(chplan.FnAbs, v), f(sciLowerBound)),
											bin(chplan.OpGe, call(chplan.FnAbs, v), f(sciUpperBound))), sci,
										fixed)
								})
							})
						})
					})
				})
			})
		})
	})

	return &chplan.Subscript{
		Container: call(chplan.FnArrayMap,
			&chplan.Lambda{Params: []string{valueParam, digitsParam}, Body: body},
			call(chplan.FnArray, value),
			call(chplan.FnArray, call(chplan.FnToString, call(chplan.FnAbs, value)))),
		Key: i(1),
	}
}

func histStringCall(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: args}
}

func histStringBinary(op chplan.BinaryOp, left, right chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: op, Left: left, Right: right}
}
