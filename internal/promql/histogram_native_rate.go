package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_rate.go lowers `rate(<selector>[range])` and
// `increase(<selector>[range])` over an OTel exponential (native)
// histogram — `<base>_exp_hist` — into a histogram-VALUED result, the
// third and last lowering of issue #1967 to cap a plan with a
// [chplan.HistogramProjection] (after the bare selector in
// histogram_native_bare.go and `sum()` in histogram_native_sum.go).
//
// Reference Prometheus answers both with a native histogram, not a
// float: promql's extrapolatedRate hands a range of native-histogram
// samples to histogramRate, which returns a *histogram.FloatHistogram,
// and the caller scales THAT by the boundary-extrapolation factor
// (`resultHistogram.Mul(factor)`). The whole distribution is the answer;
// there is no scalar anywhere in it.
//
// Two reductions, and only one of them is new here.
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
// What this file adds on top of the shared window stage is therefore
// small and entirely about being a histogram-VALUED answer:
//
//   - Count and Sum, the two whole-histogram scalars the quantile kernel
//     never reads (it works off the bucket ladder). Reference folds them
//     with everything else — `h.Sub(prev)` subtracts both, `h.Add(prev)`
//     adds both back at a reset, and `h.Mul(factor)` scales both — so
//     they go through the SAME fold as every bucket rather than a
//     separate rule. See [expHistogramValuedWindowPassthrough].
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
//	    Filter uniqExact(TimeUnix) >= 2                    ← rate/increase floor
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
// Counter-reset detection is the one place this path is known to differ
// from reference, and it differs there for the shared window fold's
// existing reason rather than anything introduced here: reference
// decides a reset for the WHOLE histogram (`FloatHistogram.DetectReset`
// — any bucket, the zero bucket, or Count regressing condemns the whole
// sample) and then adds the entire previous distribution, whereas
// [counterIncreaseFold] decides per component and adds the previous
// reading only of the components that themselves regressed. The two
// agree exactly on a genuine restart, where every component drops
// together, and drift only where a component recovers past its
// pre-reset reading within one scrape. Issue #2017 carries the
// whole-histogram detection for the shared fold, which the quantile path
// reads through the same code.

// hqWindowSumArrayAlias holds the group's groupArray of each row's Sum —
// the total observed VALUE, positionally parallel to the counts / scales
// / offsets / buckets / timestamp lists [expHistogramWindowAggs] already
// collects. It is the one field a histogram-VALUED window answer needs
// that no quantile ever reads, which is why it is appended by
// [expHistogramValuedWindowAggs] rather than added to the shared list:
// the quantile paths would carry it as a dead column in every emitted
// SELECT.
const hqWindowSumArrayAlias = "_hq_sum_list"

// rateWindowFn / increaseWindowFn are the two range-vector functions this
// file answers, spelled once so the matcher and the per-second division
// cannot disagree about which is which.
const (
	rateWindowFn     = "rate"
	increaseWindowFn = "increase"
)

// rateOverExpHistogram reports whether expr is `rate(<bare exp-histogram
// selector>[range])` or the `increase` twin — the two shapes this file
// answers — returning the matched [histogramAggShape].
//
// Like [bareExpHistogramSelector] and [sumOverExpHistogram] it is asked
// only of the ROOT of a query (see [lowerRoot]), and for the same reason:
// the answer is a histogram row, which only the wire can consume.
// `rate(m_exp_hist[5m]) * 2` or `label_replace(rate(m_exp_hist[5m]), ...)`
// reaches lowerVectorSelector through the ordinary descent and is still
// rejected there.
//
// An aggregation WRAPPER disqualifies the match: `sum(rate(...))` stacks
// the window reduction and the across-series merge, and admitting it here
// would answer it with the window reduction alone — silently dropping the
// sum. [matchHistogramAggIdiom] captures any such wrapper into
// shape.agg, so testing that field for nil is the whole of the check.
//
// A non-positive `[range]` is refused rather than answered: `rate`
// divides by it (see [expHistogramValuedWindowFold]), and falling through
// leaves the shape on the explicit rejection path instead of emitting a
// division by zero.
func rateOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramAggShape, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return histogramAggShape{}, false
	}
	shape, ok := matchHistogramAggIdiom(expr, s)
	if !ok || shape.agg != nil || shape.windowRange <= 0 {
		return histogramAggShape{}, false
	}
	if shape.windowFn != rateWindowFn && shape.windowFn != increaseWindowFn {
		return histogramAggShape{}, false
	}
	if !s.IsExpHistogramMetric(metricNameFromMatchers(shape.selector.LabelMatchers)) {
		return histogramAggShape{}, false
	}
	return shape, true
}

// lowerExpHistogramRate lowers the `rate` / `increase` shape across the
// three evaluation shapes [rangeGridShapeFor] distinguishes, the same
// three its two siblings handle — see [lowerExpHistogramBare] for what
// each one means.
func lowerExpHistogramRate(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if grid := rangeGridShapeFor(shape.selector, ctx); grid == gridFanout {
		return lowerExpHistogramRateRange(shape, s, ctx), nil
	} else if grid == gridBroadcast {
		windowed, err := expHistogramRateWindowed(shape, s, ctx)
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
	windowed, err := expHistogramRateWindowed(shape, s, ctx)
	if err != nil {
		return nil, err
	}
	return aggregatedHistogramProjection(windowed, chplan.NowNano(), s), nil
}

// expHistogramRateWindowed builds the instant-mode subtree beneath the
// histogram projection: the filtered scan reduced, per series, to that
// series' window rate. Its prologue is lowerHistogramQuantileNativeAgg's
// rung for rung — same anchor resolution, same `(anchor - range, anchor]`
// predicate, and the same two bound expressions handed to the fold — for
// the reason [fanoutWindowBoundsExpr]'s doc gives about its range-mode
// twin: the extrapolation correction must read the SAME window edges the
// Filter selects rows against, or the two drift apart.
func expHistogramRateWindowed(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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

// lowerExpHistogramRateRange is the query_range shape: the per-anchor
// [chplan.RangeBucketFanout] the sibling range paths build — one
// `[range]` window per step anchor, keyed on SERIES identity — with the
// window reduction on top and the histogram projection as the cap.
//
// The fan-out owns the rate/increase two-sample floor natively through
// [chplan.RangeBucketFanout.MinSamples] (emitted as a HAVING), which is
// why this path does not repeat the instant stage's minSamplesFilter.
func lowerExpHistogramRateRange(shape histogramAggShape, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(shape.selector.LabelMatchers, s)
	win := aggWindowFor(shape)

	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	rangeStart, rangeEnd := fanoutWindowBoundsExpr(anchorRef, win)
	fold := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd)

	perSeries := expHistogramWindowReshape(
		buildHistogramBucketFanout(
			scan, pred, nil, win,
			[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
			expHistogramValuedWindowAggs(s), s, ctx,
		),
		fold,
		expHistogramValuedWindowPassthrough([]chplan.Projection{
			{Expr: anchorRef, Alias: stepGridAnchorColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		}, fold, s),
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
	fold := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd)
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(expHistogramValuedWindowAggs(s), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	return expHistogramWindowReshape(
		minSamplesFilter(group, shape.minSamples()),
		fold,
		expHistogramValuedWindowPassthrough([]chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		}, fold, s),
		s,
	)
}

// expHistogramValuedWindowFold is [histogramWindowFold] over the
// exponential-histogram window — the counter-increase numerator times
// Prometheus's boundary-extrapolation factor — carrying `rate`'s
// per-second division.
//
// Reference applies that division to the SAME scalar the extrapolation
// produces (`factor /= ms.Range.Seconds()`, then one `Mul(factor)` over
// the whole histogram), so dividing the fold's product is the same
// arithmetic: every bucket, the zero bucket, Count and Sum all pass
// through here and are scaled alike. `increase` returns the fold
// unchanged — it is the same reduction WITHOUT the per-second division,
// which is the entire difference between the two functions in reference
// as well.
//
// The division wraps the fold's OUTPUT rather than reaching inside it,
// which is what keeps the durationToZero clamp reading the quantities
// reference reads: that clamp weighs the first in-window Count against
// the window's own total increase, both pre-factor and pre-division (see
// [histogramExtrapolationFactorExpr]).
//
// countValues is the whole-histogram Count series rather than any one
// bucket's counts, and temporality is nil — the two arguments the shared
// fold takes that this path does not vary. Both match the exponential-
// histogram window's other call sites exactly: the Count series is what
// reference's native-histogram durationToZero reads, and a nil
// temporality applies the CUMULATIVE counter-reset reading, which is the
// reading every OTel exponential-histogram path in this package uses —
// the branch issue #1963 covers for this table.
func expHistogramValuedWindowFold(shape histogramAggShape, rangeStart, rangeEnd chplan.Expr) histogramWindowTimeFold {
	fold := histogramWindowFold(shape.windowFn, rangeStart, rangeEnd, expHistogramWindowCountValuesExpr(), nil)
	if shape.windowFn != rateWindowFn {
		return fold
	}
	rangeSeconds := &chplan.LitFloat{V: shape.windowRange.Seconds()}
	return func(values, order chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpDiv, Left: fold(values, order), Right: rangeSeconds}
	}
}

// expHistogramValuedWindowAggs is [expHistogramWindowAggs] widened by the
// one field a histogram-VALUED window answer needs and a quantile does
// not: the group's Sum readings, collected in the same row order as every
// other groupArray so the fold can put them in time order. Count is
// already there — the quantile path collects it for the durationToZero
// clamp (see [hqWindowCountArrayAlias]).
func expHistogramValuedWindowAggs(s schema.Metrics) []chplan.AggFunc {
	return append(expHistogramWindowAggs(s), chplan.AggFunc{
		Name:  "groupArray",
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}},
		Alias: hqWindowSumArrayAlias,
	})
}

// expHistogramValuedWindowPassthrough is the reshape passthrough for a
// histogram-VALUED window: the caller's own key projections (Attributes,
// preceded by the step anchor in range mode) followed by the window-
// reduced Count and Sum.
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
func expHistogramValuedWindowPassthrough(keys []chplan.Projection, fold histogramWindowTimeFold, s schema.Metrics) []chplan.Projection {
	tsList := &chplan.ColumnRef{Name: hqWindowTsListAlias}
	projs := make([]chplan.Projection, 0, len(keys)+2)
	projs = append(projs, keys...)
	return append(
		projs,
		chplan.Projection{
			Expr:  fold(expHistogramWindowCountValuesExpr(), tsList),
			Alias: s.CountColumn,
		},
		chplan.Projection{
			Expr:  fold(expHistogramWindowFloatsExpr(&chplan.ColumnRef{Name: hqWindowSumArrayAlias}), tsList),
			Alias: s.SumColumn,
		},
	)
}
