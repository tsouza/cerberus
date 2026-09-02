package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_over_time.go lowers sum_over_time() and avg_over_time()
// over an OTel exponential (native) histogram to a histogram-valued sample.
// Both functions add the in-window distributions after reconciling them to
// the coarsest scale. avg_over_time divides the five count-bearing fields by
// the number of histogram samples; scale and bucket offsets remain positions.

const (
	sumOverTimeWindowFn = "sum_over_time"
	avgOverTimeWindowFn = "avg_over_time"

	expHistogramKahanAggName = "sumKahan"
)

// overTimeOverExpHistogram recognizes the two histogram-aware over-time
// reducers at the query root. A nested histogram-valued result still needs a
// histogram-aware parent and therefore remains on the explicit selector
// rejection path.
func overTimeOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramAggShape, bool) {
	if !expHistogramLoweringAvailable(s, ctx) {
		return histogramAggShape{}, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || (call.Func.Name != sumOverTimeWindowFn && call.Func.Name != avgOverTimeWindowFn) || len(call.Args) != 1 {
		return histogramAggShape{}, false
	}
	ms, ok := peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok || ms.Range <= 0 {
		return histogramAggShape{}, false
	}
	vs, ok := peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok || !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return histogramAggShape{}, false
	}
	return histogramAggShape{
		selector:    vs,
		windowRange: ms.Range,
		windowFn:    call.Func.Name,
	}, true
}

// lowerExpHistogramOverTime selects the same three anchor shapes as the bare,
// aggregation, and rate histogram-valued lowerings.
func lowerExpHistogramOverTime(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(shape.selector, ctx) {
	case gridFanout:
		return lowerExpHistogramOverTimeRange(shape, s, ctx), nil
	case gridBroadcast:
		windowed, err := expHistogramOverTimeWindowed(shape, s, ctx)
		if err != nil {
			return nil, err
		}
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return aggregatedHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	default:
		windowed, err := expHistogramOverTimeWindowed(shape, s, ctx)
		if err != nil {
			return nil, err
		}
		return aggregatedHistogramProjection(windowed, chplan.NowNano(), s), nil
	}
}

// expHistogramOverTimeWindowed builds one histogram row per series for an
// instant anchor. The OTel exponential-histogram table stores histogram
// samples only: float/histogram mixtures, custom-bucket histograms, and reset
// hints from Prometheus's in-memory model are not representable here.
func expHistogramOverTimeWindowed(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector
	anchor, err := selectorAnchor(vs, ctx)
	if err != nil {
		return nil, err
	}

	pred := buildPredicate(vs.LabelMatchers, s)
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))
	var input chplan.Node = &chplan.Scan{Table: s.ExpHistogramTable}
	if pred != nil {
		input = &chplan.Filter{Input: input, Predicate: pred}
	}
	windowed := expHistogramValuedWindowStage(
		input,
		shape,
		windowLeftBoundExpr(anchor, shape.windowRange),
		windowRightBoundExpr(anchor),
		s,
	)
	return windowed, nil
}

// lowerExpHistogramOverTimeRange fans each stored histogram over the request
// grid and reduces each (series, anchor) window.
func lowerExpHistogramOverTimeRange(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) chplan.Node {
	win := aggWindowFor(shape)
	anchor := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	rangeStart, rangeEnd := fanoutWindowBoundsExpr(anchor, win)
	fold, winIn := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd, s)
	aggs := expHistogramValuedWindowAggs(s, shape.windowFn)

	perSeries := expHistogramWindowReshape(
		buildHistogramBucketFanout(
			&chplan.Scan{Table: s.ExpHistogramTable},
			buildPredicate(shape.selector.LabelMatchers, s),
			nil,
			win,
			[]chplan.Expr{histogramIdentityExpr(s)},
			[]string{s.AttributesColumn},
			aggs,
			s,
			ctx,
		),
		aggs,
		[]string{stepGridAnchorColumn, s.AttributesColumn},
		nil,
		fold,
		shape.windowFn,
		winIn,
		expHistogramValuedWindowScalars(fold, s),
		s,
	)
	return aggregatedHistogramProjection(perSeries, anchor, s)
}

// expHistogramValuedOverTimeFold reproduces Prometheus's compensated
// histogram addition component by component. arrayReduce's aggregate-name
// string is data interpreted by ClickHouse (the documented FnArrayReduce
// contract), while the function shape itself stays in typed chplan IR.
// Values are ordered by their sample timestamps before summation because the
// compensation term is order-sensitive and Prometheus walks a Series in time
// order. avg_over_time divides the compensated sum by the histogram-sample
// count; every field receives the same divisor.
func expHistogramValuedOverTimeFold(fn string) histogramWindowTimeFold {
	return func(values, order chplan.Expr) chplan.Expr {
		ordered := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramWindowValues, paramWindowOrder},
				Body:   &chplan.BareIdent{Name: paramWindowOrder},
			},
			values,
			order,
		}}
		sum := chplan.Expr(&chplan.FuncCall{
			Fn: chplan.FnArrayReduce,
			Args: []chplan.Expr{
				&chplan.LitString{V: expHistogramKahanAggName},
				ordered,
			},
		})
		if fn == avgOverTimeWindowFn {
			return &chplan.Binary{
				Op:    chplan.OpDiv,
				Left:  sum,
				Right: &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{ordered}},
			}
		}
		return sum
	}
}
