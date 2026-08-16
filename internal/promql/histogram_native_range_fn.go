package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_range_fn.go lowers the five histogram-valued PromQL range
// functions — rate, increase, delta, irate and idelta — over an OTel
// exponential (native) histogram into a [chplan.HistogramProjection]. The
// rate/increase path originated in issue #1967; issue #2224 extended the
// same row and wire contract to the gauge-delta and instant-rate families.
//
// Reference Prometheus answers all five with a native histogram, not a
// float. rate/increase/delta use extrapolatedRate; irate/idelta use
// instantValue over the final pair. Both kernels return the whole
// distribution as the sample value rather than collapsing it to a scalar.
//
// The lowering has two stages.
//
//  1. The window reduction — per series, fold the in-window samples into
//     one distribution under the counter-reset rule, then stretch it by
//     the boundary-extrapolation factor. Cerberus already runs exactly
//     this for `histogram_quantile(phi, <agg>(rate(<sel>_exp_hist[w])))`:
//     [expHistogramWindowAggs] + [expHistogramWindowReshape] +
//     [histogramWindowFold], documented against Prometheus's own
//     histogramRate in histogram_quantile_native_window.go. This file
//     reuses that stage rather than re-deriving its arithmetic, the same
//     discipline that let `sum()` land on top of the across-series merge.
//  2. Publishing it — which the quantile path never does, because it
//     consumes the folded distribution with an interpolation kernel.
//
// The shared window stage is widened here for a histogram-VALUED answer:
//
//   - Count and Sum, the two whole-histogram scalars the quantile kernel
//     never reads (it works off the bucket ladder). Reference folds them
//     with everything else — `h.Sub(prev)` subtracts both, `h.Add(prev)`
//     adds both back at a reset, and `h.Mul(factor)` scales both — so
//     they go through the SAME fold as every bucket rather than a
//     separate rule. See [expHistogramValuedWindowScalars].
//   - `rate`'s per-second division. Reference folds it into the same
//     scalar: `factor /= ms.Range.Seconds()` before the single
//     `Mul(factor)`. The quantile path leaves it out deliberately (a
//     per-series constant cancels out of every bucket RATIO), and a
//     published histogram is precisely where it stops cancelling. See
//     [expHistogramValuedWindowFold].
//
// ONE factor for the WHOLE histogram. This is the part a plausible-but-
// wrong implementation gets wrong, so it is worth stating twice:
// reference's extrapolatedRate computes a single scalar from the
// series' own in-window sample timestamps against the requested window
// edges, and multiplies the entire result histogram by it. It does NOT
// extrapolate each bucket independently. Cerberus's fold gets this right
// structurally rather than by convention — [histogramExtrapolationFactorExpr]
// reads only `order` (the per-series timestamp list, which
// [expHistogramWindowBucketsExpr] passes unfiltered for every bucket) and
// `countValues` (the whole-histogram Count series, via
// [expHistogramWindowCountValuesExpr]), so every bucket, the zero bucket,
// Count and Sum all render the identical factor expression and scale
// alike.
//
// Plan shape (instant mode; the range shapes differ only in the anchor
// column threaded through the reduction):
//
//	HistogramProjection [MetricName='', Attributes, now64(9), Value=0, <nine histogram columns>]
//	  Project [Attributes, folded Count / Sum / Scale / ZeroCount /
//	           {Pos,Neg}{Offset,BucketCounts}]
//	    Filter uniqExact(TimeUnix) >= 2                    ← range-function floor
//	      Aggregate groupBy=[series identity] funcs=<expHistogramValuedWindowAggs>
//	        Filter <matchers> AND <window bounds>
//	          Scan(otel_metrics_exponential_histogram)
//
// Unlike `sum()`, there is no second grouping stage: `rate()` reduces
// each series along TIME and emits one row per series, so the across-
// series merge never enters. `sum(rate(<sel>_exp_hist[w]))` — which
// stacks both — is a further shape and stays on
// [expHistogramSelectorRouting]'s explicit rejection.
//
// Counter functions follow reference's whole-histogram reset verdict rather
// than letting each component decide independently. Gauge functions bypass
// that mask: delta/idelta preserve a negative difference, while irate retains
// the current whole histogram when the final pair is a reset.

// hqWindowSumArrayAlias holds the group's groupArray of each row's Sum —
// the total observed VALUE, positionally parallel to the counts / scales
// / offsets / buckets / timestamp lists [expHistogramWindowAggs] already
// collects. It is the one field a histogram-VALUED window answer needs
// that no quantile ever reads, which is why it is appended by
// [expHistogramValuedWindowAggs] rather than added to the shared list:
// the quantile paths would carry it as a dead column in every emitted
// SELECT.
const hqWindowSumArrayAlias = "_hq_sum_list"

// hqWindowAllTsListAlias preserves the full window's timestamps while the
// delta path narrows every histogram field to the first and last samples.
// Prometheus subtracts only those endpoint histograms, but its extrapolation
// factor still divides the endpoint span by the number of gaps across every
// in-window sample.
const hqWindowAllTsListAlias = "_hq_all_ts_list"

const hqWindowAllArrayAliasPrefix = "_hq_all_"

// lastTwoSamplesSliceOffset is ClickHouse arraySlice's negative offset for
// retaining exactly the final pair consumed by irate and idelta.
const lastTwoSamplesSliceOffset int64 = -2

type histogramWindowSampleSelection uint8

const (
	histogramWindowAllSamples histogramWindowSampleSelection = iota
	histogramWindowEndpoints
	histogramWindowLastTwo
)

const (
	paramWindowSelectedValue = "wsv"
	paramWindowSelectedTime  = "wst"
)

// Histogram-valued range functions are spelled once so the matcher and the
// fold dispatch cannot disagree about which functions publish a native
// histogram rather than an ordinary float sample.
const (
	rateWindowFn     = "rate"
	increaseWindowFn = "increase"
	deltaWindowFn    = "delta"
	irateWindowFn    = "irate"
	ideltaWindowFn   = "idelta"
)

// rangeFnOverExpHistogram reports whether expr is one of Prometheus's five
// histogram-valued range functions over a bare exponential-histogram selector,
// returning the matched [histogramAggShape].
//
// Like [bareExpHistogramSelector] and [sumOrAvgOverExpHistogram] it is asked
// only of the ROOT of a query (see [lowerRoot]), and for the same reason:
// the answer is a histogram row, which only the wire can consume.
// `rate(m_exp_hist[5m]) * 2` or `label_replace(rate(m_exp_hist[5m]), ...)`
// reaches lowerVectorSelector through the ordinary descent and is still
// rejected there.
//
// An aggregation wrapper never reaches the direct call assertion above:
// `sum(rate(...))` stacks the window reduction and the across-series merge,
// and admitting it here would answer it with the window reduction alone —
// silently dropping the sum.
//
// A non-positive `[range]` is refused rather than answered: `rate`
// divides by it (see [expHistogramValuedWindowFold]), and falling through
// leaves the shape on the explicit rejection path instead of emitting a
// division by zero.
func rangeFnOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramAggShape, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return histogramAggShape{}, false
	}
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || len(call.Args) != 1 {
		return histogramAggShape{}, false
	}
	switch call.Func.Name {
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn:
	default:
		return histogramAggShape{}, false
	}
	ms, ok := peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok || ms.Range <= 0 {
		return histogramAggShape{}, false
	}
	vs, ok := peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	shape := histogramAggShape{selector: vs, windowRange: ms.Range, windowFn: call.Func.Name}
	if !s.IsExpHistogramMetric(metricNameFromMatchers(shape.selector.LabelMatchers)) {
		return histogramAggShape{}, false
	}
	return shape, true
}

// lowerExpHistogramRangeFn lowers a histogram-valued range function across
// the three evaluation shapes [rangeGridShapeFor] distinguishes, the same
// three its two siblings handle — see [lowerExpHistogramBare] for what each
// one means.
func lowerExpHistogramRangeFn(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if grid := rangeGridShapeFor(shape.selector, ctx); grid == gridFanout {
		return lowerExpHistogramRangeFnRange(shape, s, ctx), nil
	} else if grid == gridBroadcast {
		windowed, err := expHistogramRangeFnWindowed(shape, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast placement as the bare and `sum()` paths': the
		// cross join goes UNDER the histogram projection so the plan root
		// stays a HistogramProjection and keeps publishing the
		// thirteen-column contract. The pinned window is reduced ONCE and
		// every step reports that one rate at its own timestamp.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return aggregatedHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	windowed, err := expHistogramRangeFnWindowed(shape, s, ctx)
	if err != nil {
		return nil, err
	}
	return aggregatedHistogramProjection(windowed, chplan.NowNano(), s), nil
}

// expHistogramRangeFnWindowed builds the instant-mode subtree beneath the
// histogram projection: the filtered scan reduced, per series, to that
// series' window rate. Its prologue is lowerHistogramQuantileNativeAgg's
// rung for rung — same anchor resolution, same `(anchor - range, anchor]`
// predicate, and the same two bound expressions handed to the fold — for
// the reason [fanoutWindowBoundsExpr]'s doc gives about its range-mode
// twin: the extrapolation correction must read the SAME window edges the
// Filter selects rows against, or the two drift apart.
func expHistogramRangeFnWindowed(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	rangeEnd := windowRightBoundExpr(anchor)
	rangeStart := windowLeftBoundExpr(anchor, shape.windowRange)
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return expHistogramValuedWindowStage(input, shape, rangeStart, rangeEnd, s), nil
}

// lowerExpHistogramRangeFnRange is the query_range shape: the per-anchor
// [chplan.RangeBucketFanout] the sibling range paths build — one
// `[range]` window per step anchor, keyed on SERIES identity — with the
// window reduction on top and the histogram projection as the cap.
//
// The fan-out owns the range function's two-sample floor natively through
// [chplan.RangeBucketFanout.MinSamples] (emitted as a HAVING), which is
// why this path does not repeat the instant stage's minSamplesFilter.
func lowerExpHistogramRangeFnRange(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(shape.selector.LabelMatchers, s)
	win := aggWindowFor(shape)

	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	rangeStart, rangeEnd := fanoutWindowBoundsExpr(anchorRef, win)
	fold := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd, s)

	aggs := expHistogramValuedWindowAggs(s, shape.windowFn)
	grouped := buildHistogramBucketFanout(
		scan, pred, nil, win,
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		aggs, s, ctx,
	)
	selected := selectExpHistogramWindowSamples(
		grouped, aggs, []string{stepGridAnchorColumn, s.AttributesColumn},
		histogramWindowSelectionFor(shape.windowFn),
	)
	perSeries := expHistogramWindowReshape(
		selected,
		aggs,
		[]string{stepGridAnchorColumn, s.AttributesColumn},
		expHistogramResetMaskFor(shape.windowFn),
		fold,
		expHistogramValuedWindowScalars(fold, s),
		s,
	)
	return aggregatedHistogramProjection(perSeries, anchorRef, s)
}

// expHistogramValuedWindowStage is [expHistogramWindowStage] widened for
// a histogram-VALUED answer: the same per-series grouping, the same
// two-sample floor, the same reshape — with Sum collected alongside the
// fields the quantile path already collects, and Count / Sum folded into
// the reshaped row.
func expHistogramValuedWindowStage(input chplan.Node, shape histogramAggShape, rangeStart, rangeEnd chplan.Expr, s schema.Metrics) chplan.Node {
	fold := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd, s)
	aggs := expHistogramValuedWindowAggs(s, shape.windowFn)
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(aggs, windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	selected := selectExpHistogramWindowSamples(
		minSamplesFilter(group, shape.minSamples()), aggs, []string{s.AttributesColumn},
		histogramWindowSelectionFor(shape.windowFn),
	)
	return expHistogramWindowReshape(
		selected,
		aggs,
		[]string{s.AttributesColumn},
		expHistogramResetMaskFor(shape.windowFn),
		fold,
		expHistogramValuedWindowScalars(fold, s),
		s,
	)
}

func histogramWindowSelectionFor(windowFn string) histogramWindowSampleSelection {
	switch windowFn {
	case deltaWindowFn:
		return histogramWindowEndpoints
	case irateWindowFn, ideltaWindowFn:
		return histogramWindowLastTwo
	default:
		return histogramWindowAllSamples
	}
}

// selectExpHistogramWindowSamples narrows the grouped row arrays to the
// histogram samples the reference kernel actually subtracts. delta uses the
// first and last histograms; irate/idelta use the last two. rate/increase keep
// the whole window because reset detection visits every consecutive pair.
func selectExpHistogramWindowSamples(
	input chplan.Node,
	aggs []chplan.AggFunc,
	keyAliases []string,
	selection histogramWindowSampleSelection,
) chplan.Node {
	if selection == histogramWindowAllSamples {
		return input
	}

	input = aliasExpHistogramWindowArrays(input, aggs, keyAliases)
	times := &chplan.ColumnRef{Name: hqWindowAllTsListAlias}
	projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+1)
	for _, alias := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: alias}, Alias: alias})
	}
	projs = append(projs, chplan.Projection{Expr: times, Alias: hqWindowAllTsListAlias})

	for _, agg := range aggs {
		expr := chplan.Expr(&chplan.ColumnRef{Name: agg.Alias})
		switch {
		case agg.Alias == hqAggMergedScaleAlias:
			expr = &chplan.FuncCall{Fn: chplan.FnArrayMin, Args: []chplan.Expr{
				selectExpHistogramValues(&chplan.ColumnRef{Name: fullExpHistogramWindowArrayAlias(hqAggScalesArrayAlias)}, times, selection),
			}}
		case agg.Fn == chplan.FnGroupArray:
			if agg.Alias == hqWindowTsListAlias {
				expr = selectExpHistogramTimes(times, selection)
			} else {
				expr = selectExpHistogramValues(
					&chplan.ColumnRef{Name: fullExpHistogramWindowArrayAlias(agg.Alias)},
					times,
					selection,
				)
			}
		}
		projs = append(projs, chplan.Projection{Expr: expr, Alias: agg.Alias})
	}
	return &chplan.Project{Input: input, Projections: projs}
}

// aliasExpHistogramWindowArrays puts every full groupArray under a name the
// selection Project never overwrites. ClickHouse substitutes SELECT aliases
// across sibling expressions, including forward references; selecting a
// shortened array beside expressions that read its original name would apply
// the selection twice and make arraySort see unequal lengths.
func aliasExpHistogramWindowArrays(input chplan.Node, aggs []chplan.AggFunc, keyAliases []string) chplan.Node {
	projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs))
	for _, alias := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: alias}, Alias: alias})
	}
	for _, agg := range aggs {
		alias := agg.Alias
		if agg.Fn == chplan.FnGroupArray {
			alias = fullExpHistogramWindowArrayAlias(alias)
		}
		projs = append(projs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: agg.Alias},
			Alias: alias,
		})
	}
	return &chplan.Project{Input: input, Projections: projs}
}

func fullExpHistogramWindowArrayAlias(alias string) string {
	if alias == hqWindowTsListAlias {
		return hqWindowAllTsListAlias
	}
	return hqWindowAllArrayAliasPrefix + alias
}

func selectExpHistogramValues(values, times chplan.Expr, selection histogramWindowSampleSelection) chplan.Expr {
	sorted := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramWindowSelectedValue, paramWindowSelectedTime},
			Body:   &chplan.BareIdent{Name: paramWindowSelectedTime},
		},
		values,
		times,
	}}
	return selectExpHistogramSorted(sorted, selection)
}

func selectExpHistogramTimes(times chplan.Expr, selection histogramWindowSampleSelection) chplan.Expr {
	return selectExpHistogramSorted(
		&chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{times}},
		selection,
	)
}

func selectExpHistogramSorted(sorted chplan.Expr, selection histogramWindowSampleSelection) chplan.Expr {
	if selection == histogramWindowLastTwo {
		return &chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
			sorted, &chplan.LitInt{V: lastTwoSamplesSliceOffset},
		}}
	}
	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
			sorted, &chplan.LitInt{V: 1}, &chplan.LitInt{V: 1},
		}},
		&chplan.FuncCall{Fn: chplan.FnArraySlice, Args: []chplan.Expr{
			sorted, &chplan.LitInt{V: -1}, &chplan.LitInt{V: 1},
		}},
	}}
}

// expHistogramValuedWindowFold is [histogramWindowFold] over the
// exponential-histogram window — the counter-increase numerator times
// Prometheus's boundary-extrapolation factor — carrying `rate`'s
// per-second division.
//
// Reference applies that division to the SAME scalar the extrapolation
// produces — `factor /= ms.Range.Seconds()`, then ONE `Mul(factor)` over
// the whole histogram — so the divisor is handed to the fold and lands on
// the factor, not on the fold's product. Every bucket, the zero bucket,
// Count and Sum then pass through one multiplication and are scaled
// alike. `increase` passes no divisor at all — it is the same reduction
// WITHOUT the per-second division, which is the entire difference between
// the two functions in reference as well.
//
// Dividing the product instead — which this path did until the
// histogram-valued compat cases caught it — is the same value in exact
// arithmetic and a different one in float64, because (a*b)/c and a*(b/c)
// disagree by an ulp on most inputs. Nothing before those cases could
// see it: a scalar answer is compared against reference under an epsilon,
// a quantile reads ratios this divisor cancels out of, and the txtar
// goldens compare cerberus against its own arithmetic. A histogram-VALUED
// answer is compared bucket by bucket with no epsilon at all, because the
// comparer's approximate-equality option binds float64 while histogram
// counts decode as model.FloatString.
//
// Putting the divisor on the factor also leaves the durationToZero clamp
// reading exactly the quantities reference reads: that clamp weighs the
// first in-window Count against the window's own total increase, both
// pre-factor and pre-division (see [histogramExtrapolationFactorExpr]).
//
// countValues is the whole-histogram Count series rather than any one bucket's
// counts. Its per-series temporality selects the DELTA numerator when needed.
func expHistogramValuedWindowFold(shape histogramAggShape, rangeStart, rangeEnd chplan.Expr, s schema.Metrics) histogramWindowTimeFold {
	if shape.windowFn == sumOverTimeWindowFn || shape.windowFn == avgOverTimeWindowFn {
		return expHistogramValuedOverTimeFold(shape.windowFn)
	}
	var perSecond chplan.Expr
	if shape.windowFn == rateWindowFn {
		perSecond = &chplan.LitFloat{V: shape.windowRange.Seconds()}
	}
	var factorOrder chplan.Expr
	if shape.windowFn == deltaWindowFn {
		factorOrder = &chplan.ColumnRef{Name: hqWindowAllTsListAlias}
	}
	return histogramWindowFold(shape.windowFn, histogramWindowInputs{
		rangeStart:  rangeStart,
		rangeEnd:    rangeEnd,
		factorOrder: factorOrder,
		countValues: expHistogramWindowCountValuesExpr(),
		temporality: expHistogramWindowTemporalityExpr(s, shape.windowFn),
		resets:      expHistogramResetMaskFor(shape.windowFn),
		perSecond:   perSecond,
	})
}

// expHistogramValuedWindowAggs is [expHistogramWindowAggs] widened by the
// one field a histogram-VALUED window answer needs and a quantile does
// not: the group's Sum readings, collected in the same row order as every
// other groupArray so the fold can put them in time order. Count is
// already there — the quantile path collects it for the durationToZero
// clamp (see [hqWindowCountArrayAlias]).
func expHistogramValuedWindowAggs(s schema.Metrics, windowFn string) []chplan.AggFunc {
	return append(expHistogramWindowAggs(s, windowFn), chplan.AggFunc{
		Fn:    chplan.FnGroupArray,
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}},
		Alias: hqWindowSumArrayAlias,
	})
}

// expHistogramValuedWindowScalars is the pair of projections a
// histogram-VALUED window adds to the reshape and a quantile does not:
// the window-reduced Count and Sum.
//
// Both scalars go through the caller's own `fold` — the same one
// [expHistogramWindowReshape] applies to the zero bucket and to both
// signed bucket ladders — because reference treats them exactly like a
// bucket: `h.Sub(prev)` subtracts Count and Sum, the reset iteration's
// `h.Add(prev)` adds them back, and `h.Mul(factor)` scales them. Folding
// them any other way (a plain `last - first`, say, or a sum across the
// window) would leave a published histogram whose Count disagreed with
// the bucket counts it publishes alongside.
//
// Both are cast into the float domain first, for the reason
// [expHistogramWindowFloatsExpr] documents: the fold differences
// consecutive readings and a stored UInt64 Count would underflow a
// legitimate negative difference into a colossal positive one.
func expHistogramValuedWindowScalars(fold histogramWindowTimeFold, s schema.Metrics) []chplan.Projection {
	tsList := &chplan.ColumnRef{Name: hqWindowTsListAlias}
	return []chplan.Projection{
		{
			Expr:  fold(expHistogramWindowCountValuesExpr(), tsList),
			Alias: s.CountColumn,
		},
		{
			Expr:  fold(expHistogramWindowFloatsExpr(&chplan.ColumnRef{Name: hqWindowSumArrayAlias}), tsList),
			Alias: s.SumColumn,
		},
	}
}
