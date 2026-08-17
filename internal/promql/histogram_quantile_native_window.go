package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The exponential-histogram half of the two-stage reduction
// histogram_quantile_window.go builds for classic buckets.
//
// `histogram_quantile(phi, <agg> by(le) (<fn>(<sel>_exp_hist[range])))`
// is the same two reductions in the same fixed order reference
// Prometheus applies — per SERIES the range-vector function `<fn>`
// reduces that series' in-window samples to one distribution, then
// ACROSS SERIES `<agg>` merges those distributions — and the native path
// used to collapse both into one grouping, exactly as the classic path
// did before its per-series stage landed (#1629, the native halves of
// #1535 and #1584):
//
//   - For a cumulative counter, folding every in-window ROW of every
//     series at once contributes each series' running total once per
//     scrape instead of its window increase once. It hides while the
//     bucket distribution holds its shape — `histogram_quantile` reads
//     only the RATIOS between buckets, and a constant-shape window
//     scales every bucket alike — and shows the moment the shape moves.
//   - `rate` / `increase` emit nothing for a series with fewer than two
//     samples in the window. That floor is per SERIES, and while series
//     are folded together there is nowhere to apply it: under
//     `sum by(le)`, which drops `le` and leaves histogramAggGroupBy with
//     no grouping key at all, `uniqExact(TimeUnix) >= 2` asked whether
//     the whole anchor held two scrapes, so two samples from two
//     different series satisfied a floor meant to require two samples
//     from one.
//
// What makes the exponential case its own file rather than a parameter
// on the classic one is the domain the time reduction happens in. A
// classic row carries its own `ExplicitBounds`, so rows are reconciled
// over a union of bounds in the CUMULATIVE domain. An exponential row
// carries a `Scale` and a bucket-index `Offset`, so rows are reconciled
// by downscaling every row's indices to the group's coarsest scale —
// merging is only ever lossless downward — and there the per-bucket
// counts are directly comparable across time without a cumulative round
// trip: bucket k at a fixed scale means the same interval at every
// timestamp, and for a cumulative histogram each bucket's count is
// itself a counter.
//
// That downscaling is the same rescale the across-series merge already
// performs, so the two stages share it (expHistogramRowContribsExpr /
// expHistogramBucketRowContribExpr) and differ only in how they reduce
// the per-row contributions: the merge sums them, this stage folds them
// in timestamp order under the counter-reset rule.

// hqWindowZeroCountsArrayAlias holds the group's groupArray of each
// row's ZeroCount, positionally parallel to the scales / offsets /
// buckets / timestamp lists. The zero bucket is a counter like every
// other bucket, so it reduces across the window with the SAME fold —
// summing it while the rest of the distribution is differenced would
// leave the zero band inflated by the number of scrapes, which is
// precisely the defect this stage exists to remove.
const hqWindowZeroCountsArrayAlias = "_hq_zero_counts"

// hqWindowCountArrayAlias holds the group's groupArray of each row's
// total-observation Count — the same field reference Prometheus's native-
// histogram `extrapolatedRate` reads for the durationToZero
// zero-crossing clamp (`samples.Histograms[0].H.Count` /
// `resultHistogram.Count`). It is the exponential-histogram counterpart
// of a classic rung's own cumulative values, which durationToZero reads
// directly for that path instead — see histogramWindowFold's doc.
const hqWindowCountArrayAlias = "_hq_count_list"

// hqMergeZeroCountsArrayAlias holds the across-series group's zero-bucket
// counts. Histogram SUM/AVG folds it with Prometheus's compensated histogram
// addition rather than ClickHouse's plain sum aggregate.
const hqMergeZeroCountsArrayAlias = "_hq_merge_zero_counts"

// paramExpRowCount binds one row's already-rescaled count while it is
// cast into the float domain the window fold needs.
const paramExpRowCount = "n"

// expHistogramWindowAggs are the per-series stage's aggregates: every
// in-window row's scale, offset, bucket array, zero count and
// timestamp, plus the group's coarsest scale to downscale them onto. A
// rate/increase window also needs its one AggregationTemporality reading.
//
// min(Scale) is the merged scale for the same reason it is on the
// across-series stage: an exponential histogram can be folded to a
// COARSER scale exactly (adjacent bucket pairs merge) but never to a
// finer one, so the coarsest scale present is the only one every row
// can reach.
func expHistogramWindowAggs(s schema.Metrics, windowFn string) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		{Fn: chplan.FnMin, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
	}
	// max(ZeroThreshold) only when the physical schema persists the OTLP
	// zero_threshold field — the upstream OTel-CH DDL doesn't, and the
	// emitter then renders a constant-0 zero-bucket width.
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, chplan.AggFunc{
			Fn:    chplan.FnMax,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}},
			Alias: s.ZeroThresholdColumn,
		})
	}
	aggs = append(aggs, []chplan.AggFunc{
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: hqWindowZeroCountsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.CountColumn}}, Alias: hqWindowCountArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: hqWindowTsListAlias},
	}...)
	if needsTemporalityAgg(windowFn) && s.AggregationTemporalityColumn != "" {
		aggs = append(aggs, chplan.AggFunc{
			Fn:    chplan.FnAny,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.AggregationTemporalityColumn}},
			Alias: hqWindowTemporalityAlias,
		})
	}
	return aggs
}

// expHistogramWindowTemporalityExpr returns the per-series temporality reading
// projected by expHistogramWindowAggs for a rate/increase window.
func expHistogramWindowTemporalityExpr(s schema.Metrics, windowFn string) chplan.Expr {
	if !needsTemporalityAgg(windowFn) || s.AggregationTemporalityColumn == "" {
		return nil
	}
	return &chplan.ColumnRef{Name: hqWindowTemporalityAlias}
}

// expHistogramWindowFloatsExpr casts a per-row contribution array into
// the float domain before the fold reduces it.
//
// Stored bucket counts are unsigned. counterIncreaseFold differences
// consecutive readings, and a legitimate negative difference — a
// counter that restarted mid-window, or a bucket the distribution moved
// out of — would underflow an unsigned subtraction into a colossal
// positive count. It would also make ClickHouse infer a
// Variant(Int64, UInt64) that no array aggregate accepts. This is the
// same float domain classicBucketRowCumulativeExpr moves the classic
// ladder into, for the same two reasons; Prometheus's native-histogram
// buckets are float counts anyway.
func expHistogramWindowFloatsExpr(contribs chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramExpRowCount},
				Body:   toFloat64Expr(&chplan.BareIdent{Name: paramExpRowCount}),
			},
			contribs,
		},
	}
}

// expHistogramWindowBucketsExpr renders ONE series' window-reduced
// bucket array, at the group's merged scale: for each target bucket
// index, that bucket's readings across the series' in-window rows,
// reduced by `fold` in timestamp order.
//
// It is expHistogramMergeBucketsExpr with the outer reduction swapped.
// The merge sums each target bucket's per-row contributions because its
// rows are different SERIES observed at one instant, and distributions
// add. Here the rows are one series observed at different TIMES, and a
// counter's readings do not add — they telescope — so the same
// contributions go through the range-vector function's own fold
// instead.
func expHistogramWindowBucketsExpr(
	offArrAlias, bucArrAlias, scalesArrAlias, mergedScaleAlias string,
	fold histogramWindowTimeFold,
) chplan.Expr {
	// Deliberately not "t": `fold` binds paramRowTime ("t") inside its own
	// arraySort comparator, and that comparator sits INSIDE this lambda's
	// body. Naming both "t" would shadow this binding at exactly the point
	// where the emitted SQL still reads as if it referred to the target
	// bucket. The references below are resolved before the shadow opens, so
	// "t" would be correct today and silently wrong after any refactor that
	// moved one inside the other.
	const paramExpTargetBucket = "tb"

	mergedScale := chplan.Expr(&chplan.ColumnRef{Name: mergedScaleAlias})
	scalesArr := chplan.Expr(&chplan.ColumnRef{Name: scalesArrAlias})
	offArr := chplan.Expr(&chplan.ColumnRef{Name: offArrAlias})
	bucArr := chplan.Expr(&chplan.ColumnRef{Name: bucArrAlias})
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})

	mergedStart, mergedLength := expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale)
	contribs := expHistogramRowContribsExpr(
		scalesArr, offArr, bucArr,
		expHistogramBucketRowContribExpr(mergedScale, mergedStart, paramExpTargetBucket),
	)

	// Every row contributes to every target index — a row whose stored
	// buckets miss the target contributes a 0 rather than being filtered
	// out — so the contributions array stays positionally aligned with
	// the timestamp list, which is what lets the fold order it. That is
	// the one structural difference from the classic ladder, whose
	// arrayFilter drops rows that never reported a bound: a bucket an
	// exponential row does not store is a bucket it observed zero of, not
	// a series it never reported.
	return &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramExpTargetBucket},
				Body:   fold(expHistogramWindowFloatsExpr(contribs), tsList),
			},
			&chplan.FuncCall{
				Fn: chplan.FnRange,
				Args: []chplan.Expr{
					&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{mergedLength}},
				},
			},
		},
	}
}

// expHistogramWindowReshape wraps the per-series grouping in the Project
// that turns it back into the exponential-histogram row contract
// (Attributes + Scale + ZeroCount + {Positive,Negative}{Offset,
// BucketCounts}), so the across-series stage above consumes it exactly
// as it consumes raw table rows.
//
// A COUNTER window gets a second layer beneath it, which divides the
// work the way the classic reshape's two layers do — a quantity every
// projection above reads is computed once in a layer of its own rather
// than re-rendered per reader. Here that quantity is the counter-reset
// mask: [expHistogramResetMaskStage] projects it beside the grouping's
// own columns, and every fold in the layer above reads it as a column.
// See histogram_native_reset.go.
//
// `resets` is that mask's column reference — [expHistogramResetMaskFor]'s
// answer for the same windowFn the caller built `fold` with — and a nil
// one leaves the extra layer out entirely, which is what keeps a
// `sum_over_time` or bare-selector window emitting exactly the SQL it
// emitted before the mask existed. Passing the same value here and into
// the fold is what makes the reader and the projector agree.
//
// keyAliases names the grouping's key columns (Attributes, preceded by
// the step anchor in range mode); they are forwarded through both layers
// unchanged. aggs is the aggregate list the grouping beneath was built
// with, which is what the mask layer forwards by name. scalars are the
// window-reduced whole-histogram scalars a histogram-VALUED answer
// publishes and a quantile does not — see
// [expHistogramValuedWindowScalars].
func expHistogramWindowReshape(
	group chplan.Node,
	aggs []chplan.AggFunc,
	keyAliases []string,
	resets chplan.Expr,
	fold histogramWindowTimeFold,
	scalars []chplan.Projection,
	s schema.Metrics,
) chplan.Node {
	projs := make([]chplan.Projection, 0, len(keyAliases)+len(scalars)+7)
	for _, name := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}
	projs = append(projs, scalars...)
	projs = append(
		projs,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		chplan.Projection{
			Expr: fold(
				expHistogramWindowFloatsExpr(&chplan.ColumnRef{Name: hqWindowZeroCountsArrayAlias}),
				&chplan.ColumnRef{Name: hqWindowTsListAlias},
			),
			Alias: s.ZeroCountColumn,
		},
	)
	if s.ZeroThresholdColumn != "" {
		projs = append(projs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ZeroThresholdColumn},
			Alias: s.ZeroThresholdColumn,
		})
	}
	input := group
	if resets != nil {
		input = expHistogramResetMaskStage(group, aggs, keyAliases)
	}
	return &chplan.Project{
		Input: input,
		Projections: append(
			projs,
			chplan.Projection{
				Expr:  expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
				Alias: s.PositiveOffsetColumn,
			},
			chplan.Projection{
				Expr:  expHistogramWindowBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias, fold),
				Alias: s.PositiveBucketCountsColumn,
			},
			chplan.Projection{
				Expr:  expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias),
				Alias: s.NegativeOffsetColumn,
			},
			chplan.Projection{
				Expr:  expHistogramWindowBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias, fold),
				Alias: s.NegativeBucketCountsColumn,
			},
		),
	}
}

// expHistogramWindowStage builds the instant-mode per-series stage: one
// row per series carrying that series' window-reduced distribution,
// with series holding too few samples dropped.
//
// The grouping key is the full series identity, which is also a
// series-identity BINDING site — it reads the raw table columns, so it
// goes through the same canonicalisation every other histogram grouping
// uses. The Attributes column it aliases out is already canonical,
// which is why the across-series stage above binds its keys from that
// column rather than re-deriving them from the table.
//
// rangeStart / rangeEnd are the window's own edges — see
// classicBucketWindowStage's twin doc.
func expHistogramWindowStage(input chplan.Node, shape histogramAggShape, rangeStart, rangeEnd chplan.Expr, s schema.Metrics) chplan.Node {
	// Widened by expHistogramValuedWindowAggs / expHistogramValuedWindowScalars
	// — the same Sum-collecting widening rate()/increase() apply for their
	// histogram-VALUED output — so the quantile kernel's rankBase /
	// sumIsNaN (histogram_quantile_native.go, cerberus issue #2072) see
	// this series' own window-folded Count and Sum rather than a
	// bucket-derived total.
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(expHistogramValuedWindowAggs(s, shape.windowFn), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	resets := expHistogramResetMaskFor(shape.windowFn)
	fold := histogramWindowFold(shape.windowFn, histogramWindowInputs{
		rangeStart:  rangeStart,
		rangeEnd:    rangeEnd,
		countValues: expHistogramWindowCountValuesExpr(),
		temporality: expHistogramWindowTemporalityExpr(s, shape.windowFn),
		resets:      resets,
	})
	return expHistogramWindowReshape(
		minSamplesFilter(group, shape.minSamples()),
		expHistogramValuedWindowAggs(s, shape.windowFn),
		[]string{s.AttributesColumn},
		resets,
		fold,
		expHistogramValuedWindowScalars(fold, s),
		s,
	)
}

// expHistogramWindowCountValuesExpr casts the per-series stage's
// groupArray(Count) into the float domain histogramWindowFold's
// durationToZero clamp needs — the same cast expHistogramWindowFloatsExpr
// applies to every bucket contribution array, applied here to the
// whole-histogram Count series instead of any one bucket's counts. Both
// the instant (expHistogramWindowStage) and range-mode
// (buildHistogramNativeRangeTreeMerge) callers share this: both read
// hqWindowCountArrayAlias off expHistogramWindowAggs's output, one via a
// chplan.Aggregate, the other via the RangeBucketFanout underneath it.
func expHistogramWindowCountValuesExpr() chplan.Expr {
	return expHistogramWindowFloatsExpr(&chplan.ColumnRef{Name: hqWindowCountArrayAlias})
}

// expHistogramMergeAggs are the ACROSS-SERIES stage's aggregates: the
// group's coarsest scale plus every row's distribution, collected for
// expHistogramMergeOffsetExpr / expHistogramMergeBucketsExpr to fold
// into one merged distribution.
//
// Shared by the instant and range aggregated paths, which differ only
// in whether the grouping is a chplan.Aggregate or the per-anchor
// RangeBucketFanout underneath it.
func expHistogramMergeAggs(s schema.Metrics) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		{Fn: chplan.FnMin, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: hqMergeZeroCountsArrayAlias},
	}
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, chplan.AggFunc{
			Fn:    chplan.FnMax,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}},
			Alias: s.ZeroThresholdColumn,
		})
	}
	return append(aggs, []chplan.AggFunc{
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
		// Positionally aligned with the five groupArrays above — see
		// expHistogramMergeSeriesOrderKeyExpr and
		// expHistogramSortRowsByKeyExpr (cerberus issue #2254).
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{expHistogramMergeSeriesOrderKeyExpr(s)}, Alias: hqAggSeriesOrderKeyAlias},
	}...)
}

// expHistogramMergeSortStage wraps agg — the across-series merge
// [chplan.Aggregate] built from [expHistogramMergeAggs] — in a
// passthrough Project that resorts three of its groupArray columns into
// the deterministic per-series order expHistogramMergeSeriesOrderKeyExpr
// pins (cerberus issue #2254), computed exactly ONCE per group here.
//
// expHistogramMergeBucketsExpr used to call expHistogramSortRowsByKeyExpr
// directly on the raw groupArray columns from inside its own
// per-target-bucket arrayMap(t -> ...): ClickHouse has no reason to hoist
// a t-INDEPENDENT subexpression out of a lambda body, so the three
// arraySort calls re-ran once per target bucket instead of once per
// output row — an O(mergedLength) blowup that regressed the -floor
// compat lane's already-thin timeout margin (cerberus issue #2267,
// caused by #2258's otherwise-correct fix for #2254). `* REPLACE (...)`
// rewrites the three columns IN PLACE under their existing aliases, so
// every downstream reader — expHistogramMergeBucketsExpr included —
// keeps reading hqAggScalesArrayAlias / hqAggPos*Alias / hqAggNeg*Alias
// exactly as before and needs no signature change to know the sort now
// happens here instead of inline.
//
// Sorting hqAggScalesArrayAlias here also benefits
// expHistogramMergeOffsetExpr's arrayMin call, which is order-invariant
// either way — sorting it once is strictly cheaper than the alternative
// of carrying two differently-ordered copies of the same column.
func expHistogramMergeSortStage(agg chplan.Node) chplan.Node {
	orderKey := &chplan.ColumnRef{Name: hqAggSeriesOrderKeyAlias}
	sorted := func(arrAlias string) chplan.Projection {
		return chplan.Projection{
			Expr:  expHistogramSortRowsByKeyExpr(&chplan.ColumnRef{Name: arrAlias}, orderKey),
			Alias: arrAlias,
		}
	}
	return &chplan.Project{
		Input: agg,
		Replacements: []chplan.Projection{
			sorted(hqAggScalesArrayAlias),
			sorted(hqAggPosOffsetsArrayAlias),
			sorted(hqAggPosBucketsArrayAlias),
			sorted(hqAggNegOffsetsArrayAlias),
			sorted(hqAggNegBucketsArrayAlias),
		},
	}
}

// expHistogramMergeProjections renders the across-series stage's
// reshape back into the exponential-histogram row contract: the merged
// scale, zero count and both signed bucket ladders, folded from the
// groupArrays expHistogramMergeAggs collected.
//
// Shared by the instant and range aggregated paths for the same reason
// expHistogramMergeAggs is — the two differ only in the anchor column
// the caller prepends. Callers MUST route the Aggregate through
// [expHistogramMergeSortStage] before it reaches this Project — see that
// function's doc for why.
func expHistogramMergeProjections(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeZeroCountsArrayAlias}), Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		projs = append(projs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ZeroThresholdColumn},
			Alias: s.ZeroThresholdColumn,
		})
	}
	return append(projs, []chplan.Projection{
		{Expr: expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveBucketCountsColumn},
		{Expr: expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeBucketCountsColumn},
	}...)
}

// hqQuantileRankScalarMergeAggs is [expHistogramMergeAggs] widened by the
// group's Count and Sum arrays, which the projection folds with Prometheus's
// compensated histogram addition before the quantile kernel ranks against
// the histogram's own stored scalars
// (histogram_quantile_native.go's rankBase / sumIsNaN, cerberus issue
// #2072). Count and Sum add plainly across a merge, the same additivity
// [expHistogramGroupMergeAggs] relies on for a histogram-VALUED sum() /
// avg() answer; unlike that sibling this one never widens further for
// avg's per-series-count collection, since histogram_quantile has no
// avg-shaped caller.
func hqQuantileRankScalarMergeAggs(s schema.Metrics) []chplan.AggFunc {
	return append([]chplan.AggFunc{
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.CountColumn}}, Alias: hqMergeCountsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}}, Alias: hqMergeSumsArrayAlias},
	}, expHistogramMergeAggs(s)...)
}

// hqQuantileRankScalarMergeProjections is [expHistogramMergeProjections]
// plus the Count / Sum pass-through [hqQuantileRankScalarMergeAggs]
// collected, so the quantile node's Input row carries the two scalars
// alongside the merged bucket ladders.
func hqQuantileRankScalarMergeProjections(s schema.Metrics) []chplan.Projection {
	return append([]chplan.Projection{
		{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeCountsArrayAlias}), Alias: s.CountColumn},
		{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeSumsArrayAlias}), Alias: s.SumColumn},
	}, expHistogramMergeProjections(s)...)
}
