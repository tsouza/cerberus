package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_last_first_over_time.go lowers last_over_time() and
// first_over_time() over an OTel exponential (native) histogram selector
// into a HISTOGRAM-valued result (cerberus issue #2480).
//
// Reference Prometheus's funcLastOverTime / funcFirstOverTime
// (tsouza/prometheus's promql/functions.go) read the window's raw H/F
// Point directly and choose between them by timestamp:
//
//	if h.H == nil || (len(el.Floats) > 0 && h.T < f.T) {
//	    return append(enh.Out, Sample{Metric: el.Metric, F: f.F}), nil
//	}
//	return append(enh.Out, Sample{Metric: el.Metric, H: h.H.Copy()}), nil
//
// Over a window holding EXCLUSIVELY histogram samples (h.H != nil, no
// Floats), both functions therefore answer the selected histogram sample
// itself — PRESERVED, not dropped, and with `Metric: el.Metric` keeping
// `__name__` — the same "preserve, don't drop" class
// [rangeFnPreservesName] already special-cases for the plain-float path
// (last_over_time/first_over_time are its only two members). This file
// gives the exp-histogram argument its own histogram-passthrough lowering:
// last_over_time collapses each series' in-window rows to the NEWEST
// sample (argMax by TimeUnix — the same "selected sample" collapse
// [lowerExpHistogramBare] already uses for a bare selector, just widened
// from the fixed staleness lookback to the matched `[range]`);
// first_over_time collapses to the OLDEST (argMin by TimeUnix — the mirror
// image, via [earliestArgMin]).
//
// The plan shape mirrors [lowerExpHistogramBare] rung for rung: same Scan,
// same matcher predicate, same per-series collapse, same
// [chplan.RangeBucketFanout] in range mode, same
// [chplan.HistogramProjection] cap — differing only in the window width
// (ms.Range instead of the fixed instantLookback) and, for first_over_time,
// the selection direction.
const (
	lastOverTimeWindowFn  = "last_over_time"
	firstOverTimeWindowFn = "first_over_time"
)

// lastFirstOverExpHistogram recognises `last_over_time(<selector>[r])` /
// `first_over_time(<selector>[r])` where the selector names an
// exp-histogram metric — the one shape this file answers. Mirrors
// [countPresentOverExpHistogram]'s own recognizer shape rung for rung,
// differing only in which two function names it admits.
func lastFirstOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return "", nil, nil, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || (call.Func.Name != lastOverTimeWindowFn && call.Func.Name != firstOverTimeWindowFn) || len(call.Args) != 1 {
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

// lowerLastFirstOverExpHistogram lowers the recognised shape across the
// three evaluation grids [rangeGridShapeFor] distinguishes — see
// [lowerExpHistogramBare] for what each means.
func lowerLastFirstOverExpHistogram(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		return lowerExpHistogramLastFirstRange(fn, ms, vs, s, ctx), nil
	case gridBroadcast:
		selected, err := expHistogramLastFirstWindowed(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast placement as [lowerExpHistogramBare]'s own
		// gridBroadcast arm: the cross join goes UNDER the histogram
		// projection, so the pinned window is resolved ONCE and every
		// step reports that one selected sample at its own timestamp.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return nativeHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: selected},
			bareExpHistogramNameExpr(s),
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	default:
		selected, err := expHistogramLastFirstWindowed(fn, ms, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		return nativeHistogramProjection(selected, bareExpHistogramNameExpr(s), chplan.NowNano(), s), nil
	}
}

// expHistogramLastFirstWindowed builds the instant-mode subtree: the
// selector's matcher-filtered scan bound to `(anchor - ms.Range, anchor]`
// — the same window [expHistogramCountPresentWindowed] applies —
// collapsed to one row per series via the direction-appropriate
// argMax/argMin aggregate set.
func expHistogramLastFirstWindowed(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
	return latestSampleAgg(input, nativeExpHistBareAggsDirectional(fn, s), s), nil
}

// lowerExpHistogramLastFirstRange is the query_range shape: one
// `[ms.Range]` window per step anchor via [buildHistogramBucketFanout] —
// reusing [windowFor] the same way [lowerExpHistogramBareRange] does for a
// bare selector, just with ms.Range in place of the fixed staleness
// lookback — capped with the histogram projection instead of a quantile
// or a folded distribution.
func lowerExpHistogramLastFirstRange(fn string, ms *parser.MatrixSelector, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) chplan.Node {
	fanout := buildHistogramBucketFanout(
		&chplan.Scan{Table: s.ExpHistogramTable},
		buildPredicate(vs.LabelMatchers, s), nil,
		windowFor(vs, ms.Range),
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		nativeExpHistBareAggsDirectional(fn, s), s, ctx,
	)
	return nativeHistogramProjection(fanout, bareExpHistogramNameExpr(s), &chplan.ColumnRef{Name: stepGridAnchorColumn}, s)
}

// nativeExpHistBareAggsDirectional is [nativeExpHistBareAggs] generalised
// by a selection direction: last_over_time wants the newest in-window
// sample (argMax by TimeUnix — the exact aggregate set [nativeExpHistBareAggs]
// already builds), first_over_time wants the oldest (argMin, via
// [earliestArgMin]). Every field is picked at the SAME selected row —
// keying every column's argMax/argMin off the identical TimeUnix order
// column, exactly as [nativeExpHistBareAggs] already does for last —
// which is what keeps the histogram's Scale / offsets / bucket arrays
// mutually consistent rather than each column independently optimised.
func nativeExpHistBareAggsDirectional(fn string, s schema.Metrics) []chplan.AggFunc {
	if fn == firstOverTimeWindowFn {
		return append(
			[]chplan.AggFunc{earliestArgMin(s.MetricNameColumn, s)},
			nativeExpHistValuedLatestAggsDirectional(firstOverTimeWindowFn, s)...,
		)
	}
	return nativeExpHistBareAggs(s)
}

// nativeExpHistValuedLatestAggsDirectional is [nativeExpHistValuedLatestAggs]
// generalised the same way [nativeExpHistBareAggsDirectional] generalises
// its own caller — see that function's doc.
func nativeExpHistValuedLatestAggsDirectional(fn string, s schema.Metrics) []chplan.AggFunc {
	if fn != firstOverTimeWindowFn {
		return nativeExpHistValuedLatestAggs(s)
	}
	return append(
		[]chplan.AggFunc{
			earliestArgMin(s.CountColumn, s),
			earliestArgMin(s.SumColumn, s),
		},
		nativeExpHistLatestAggsDirectional(fn, s)...,
	)
}

// nativeExpHistLatestAggsDirectional is [nativeExpHistLatestAggs]
// generalised the same way [nativeExpHistBareAggsDirectional] generalises
// its own caller — see that function's doc.
func nativeExpHistLatestAggsDirectional(fn string, s schema.Metrics) []chplan.AggFunc {
	if fn != firstOverTimeWindowFn {
		return nativeExpHistLatestAggs(s)
	}
	aggs := []chplan.AggFunc{
		earliestArgMin(s.ScaleColumn, s),
		earliestArgMin(s.ZeroCountColumn, s),
		earliestArgMin(s.PositiveOffsetColumn, s),
		earliestArgMin(s.PositiveBucketCountsColumn, s),
		earliestArgMin(s.NegativeOffsetColumn, s),
		earliestArgMin(s.NegativeBucketCountsColumn, s),
	}
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, earliestArgMin(s.ZeroThresholdColumn, s))
	}
	return aggs
}

// earliestArgMin is `argMin(<col>, TimeUnix) AS <col>` — the
// first_over_time counterpart of [latestArgMax]: same column, same order
// key, opposite selection direction (the OLDEST row in the group instead
// of the newest).
func earliestArgMin(col string, s schema.Metrics) chplan.AggFunc {
	return chplan.AggFunc{
		Fn: chplan.FnArgMin,
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: col},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		Alias: col,
	}
}
