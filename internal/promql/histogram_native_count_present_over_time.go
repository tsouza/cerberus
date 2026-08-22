package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_count_present_over_time.go lowers count_over_time() and
// present_over_time() over an OTel exponential (native) histogram selector
// into a FLOAT-valued result (cerberus issue #2480).
//
// Reference Prometheus's funcCountOverTime / funcPresentOverTime
// (tsouza/prometheus's promql/functions.go) both run through the shared
// aggrOverTime helper, which answers a series' one row whenever that
// series has ANY sample — float OR histogram — in the matched window:
//
//	func aggrOverTime(matrixVal Matrix, enh *EvalNodeHelper, aggrFn func(Series) float64) Vector {
//	    if len(matrixVal) == 0 {
//	        return enh.Out
//	    }
//	    el := matrixVal[0]
//	    return append(enh.Out, Sample{F: aggrFn(el)})
//	}
//
//	func funcCountOverTime(...) (Vector, annotations.Annotations) {
//	    return aggrOverTime(matrixVals, enh, func(s Series) float64 {
//	        return float64(len(s.Floats) + len(s.Histograms))
//	    }), nil
//	}
//
//	func funcPresentOverTime(...) (Vector, annotations.Annotations) {
//	    return aggrOverTime(matrixVals, enh, func(Series) float64 { return 1 }), nil
//	}
//
// Neither guards on `len(s.Floats) == 0` the way min_over_time / max_over_time
// (compareOverTime) or the seven #2477 float-only reducers do — so an
// ALL-HISTOGRAM window answers a non-empty count / presence value in
// reference, not an empty vector. The DROP treatment
// [rangeVectorFloatOnlyDropFuncs] applies to deriv/mad_over_time/
// stddev_over_time/stdvar_over_time would therefore be WRONG here. This
// file gives both functions their own histogram-aware windowed lowering
// instead: a per-(series, anchor) collapse that counts (or, for
// present_over_time, merely confirms) the in-window rows without ever
// reading a bucket/count/sum column — the same value-blind posture
// [lowerExpHistogramCountOrGroupOverPlan] already uses for the GROUP()
// aggregation operator.
//
// Both functions DROP `__name__` — neither is in [rangeFnPreservesName]'s
// set, matching reference: aggrOverTime's Sample carries no Metric labels
// of its own.
const (
	countOverTimeWindowFn   = "count_over_time"
	presentOverTimeWindowFn = "present_over_time"
)

// countPresentOverExpHistogram recognises `count_over_time(<selector>[r])`
// / `present_over_time(<selector>[r])` where the selector names an
// exp-histogram metric — the one shape this file answers. Mirrors
// [overTimeOverExpHistogram]'s own recognizer shape (sum_over_time /
// avg_over_time) rung for rung, differing only in which two function
// names it admits.
func countPresentOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return "", nil, nil, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || (call.Func.Name != countOverTimeWindowFn && call.Func.Name != presentOverTimeWindowFn) || len(call.Args) != 1 {
		return "", nil, nil, false
	}
	ms, ok = peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok || ms.Range <= 0 {
		return "", nil, nil, false
	}
	vs, ok = peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok || !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return "", nil, nil, false
	}
	return call.Func.Name, ms, vs, true
}

// lowerCountPresentOverExpHistogram lowers the recognised shape across the
// three evaluation grids [rangeGridShapeFor] distinguishes — see
// [lowerExpHistogramBare] for what each means.
func lowerCountPresentOverExpHistogram(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		return lowerExpHistogramCountPresentRange(fn, ms, vs, s, ctx), nil
	case gridBroadcast:
		windowed, err := expHistogramCountPresentWindowed(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return expHistogramCountPresentProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn}, s,
		), nil
	default:
		windowed, err := expHistogramCountPresentWindowed(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		return expHistogramCountPresentProjection(windowed, chplan.NowNano(), s), nil
	}
}

// expHistogramCountPresentValueAgg is the aggregate that collapses an
// in-window group of exp-histogram rows to count_over_time's / present_
// over_time's scalar Value: a genuine row COUNT for count_over_time
// (reference sums `len(Floats)+len(Histograms)`, which over an
// all-histogram window is exactly the in-window row count) or, for
// present_over_time, the constant 1 (reference never inspects the sample
// beyond its existence). `any(toFloat64(1))` is the same value-blind
// pattern [lowerExpHistogramCountOrGroupOverPlan] uses for the GROUP()
// aggregation operator: pick any row, ignore its payload.
func expHistogramCountPresentValueAgg(fn string, s schema.Metrics) chplan.AggFunc {
	if fn == presentOverTimeWindowFn {
		return chplan.AggFunc{
			Fn: chplan.FnAny,
			Args: []chplan.Expr{&chplan.FuncCall{
				Fn:   chplan.FnToFloat64,
				Args: []chplan.Expr{&chplan.LitInt{V: 1}},
			}},
			Alias: s.ValueColumn,
		}
	}
	return chplan.AggFunc{Fn: chplan.FnCount, Alias: s.ValueColumn}
}

// expHistogramCountPresentWindowed builds the instant-mode subtree: the
// selector's matcher-filtered scan bound to `(anchor - ms.Range, anchor]`
// — the same left-open/right-closed window [lowerMatrixSelector] applies;
// count_over_time / present_over_time read the WHOLE matched window, not
// a staleness lookback — collapsed to one row per series via the
// value-blind aggregate above. A series with zero in-window rows produces
// no group and therefore no output row, matching reference's "series
// absent from the selector's per-step Matrix" behavior.
func expHistogramCountPresentWindowed(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := selectorAnchor(vs, ctx)
	if err != nil {
		return nil, err
	}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, ms.Range))
	var input chplan.Node = &chplan.Scan{Table: s.ExpHistogramTable}
	if pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	return &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           []chplan.AggFunc{expHistogramCountPresentValueAgg(fn, s)},
		DropEmptyOnNoGroup: true,
	}, nil
}

// lowerExpHistogramCountPresentRange is the query_range shape: fan the
// stored histogram rows across the request's step grid via
// [buildHistogramBucketFanout], one `[ms.Range]` window per step anchor —
// reusing [windowFor] the same way [lowerExpHistogramOverTimeRange] does
// for sum_over_time / avg_over_time — and collapse each (series, anchor)
// group with the same value-blind aggregate the instant path uses.
func lowerExpHistogramCountPresentRange(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) chplan.Node {
	fanout := buildHistogramBucketFanout(
		&chplan.Scan{Table: s.ExpHistogramTable},
		buildPredicate(vs.LabelMatchers, s), nil,
		windowFor(vs, ms.Range),
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		[]chplan.AggFunc{expHistogramCountPresentValueAgg(fn, s)},
		s, ctx,
	)
	return expHistogramCountPresentProjection(fanout, &chplan.ColumnRef{Name: stepGridAnchorColumn}, s)
}

// expHistogramCountPresentProjection caps the counted/presence subtree
// with the canonical four-column sample quartet. `__name__` drops — see
// this file's own doc for why neither function preserves it.
func expHistogramCountPresentProjection(input chplan.Node, tsExpr chplan.Expr, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}
