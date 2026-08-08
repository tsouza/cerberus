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

// paramExpRowCount binds one row's already-rescaled count while it is
// cast into the float domain the window fold needs.
const paramExpRowCount = "n"

// expHistogramWindowAggs are the per-series stage's aggregates: every
// in-window row's scale, offset, bucket array, zero count and
// timestamp, plus the group's coarsest scale to downscale them onto.
//
// min(Scale) is the merged scale for the same reason it is on the
// across-series stage: an exponential histogram can be folded to a
// COARSER scale exactly (adjacent bucket pairs merge) but never to a
// finer one, so the coarsest scale present is the only one every row
// can reach.
func expHistogramWindowAggs(s schema.Metrics) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
	}
	// max(ZeroThreshold) only when the physical schema persists the OTLP
	// zero_threshold field — the upstream OTel-CH DDL doesn't, and the
	// emitter then renders a constant-0 zero-bucket width.
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, chplan.AggFunc{
			Name:  "max",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}},
			Alias: s.ZeroThresholdColumn,
		})
	}
	return append(aggs, []chplan.AggFunc{
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: hqWindowZeroCountsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: hqWindowTsListAlias},
	}...)
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
		Name: "arrayMap",
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
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramExpTargetBucket},
				Body:   fold(expHistogramWindowFloatsExpr(contribs), tsList),
			},
			&chplan.FuncCall{
				Name: "range",
				Args: []chplan.Expr{
					&chplan.FuncCall{Name: "toUInt64", Args: []chplan.Expr{mergedLength}},
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
// One Project layer suffices, unlike the classic reshape's two: the
// classic path differences a cumulative ladder and so has to read the
// ladder twice, whereas exponential buckets are already the per-bucket
// quantity the contract wants.
func expHistogramWindowReshape(
	group chplan.Node,
	fold histogramWindowTimeFold,
	passthrough []chplan.Projection,
	s schema.Metrics,
) chplan.Node {
	projs := make([]chplan.Projection, 0, len(passthrough)+7)
	projs = append(projs, passthrough...)
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
	return &chplan.Project{
		Input: group,
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
	group := &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(expHistogramWindowAggs(s), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	return expHistogramWindowReshape(
		minSamplesFilter(group, shape.minSamples()),
		histogramWindowFold(shape.windowFn, rangeStart, rangeEnd),
		[]chplan.Projection{{
			Expr:  &chplan.ColumnRef{Name: s.AttributesColumn},
			Alias: s.AttributesColumn,
		}},
		s,
	)
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
		{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
		{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, chplan.AggFunc{
			Name:  "max",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}},
			Alias: s.ZeroThresholdColumn,
		})
	}
	return append(aggs, []chplan.AggFunc{
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
	}...)
}

// expHistogramMergeProjections renders the across-series stage's
// reshape back into the exponential-histogram row contract: the merged
// scale, zero count and both signed bucket ladders, folded from the
// groupArrays expHistogramMergeAggs collected.
//
// Shared by the instant and range aggregated paths for the same reason
// expHistogramMergeAggs is — the two differ only in the anchor column
// the caller prepends.
func expHistogramMergeProjections(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
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
