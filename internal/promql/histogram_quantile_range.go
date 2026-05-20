package promql

import (
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Range-mode rewrites for `histogram_quantile(phi, X)` against the
// OTel-CH classic-histogram table.
//
// The instant-mode lowerings (`lowerHistogramQuantile` /
// `lowerHistogramQuantileAgg`) emit a single quantile row per series and
// surface `TimeUnix = now64(9)` in the wrapping Project. That is correct
// for `/api/v1/query` (instant eval = "now") but wrong for
// `/api/v1/query_range` — every step in `[start, end]` carries the same
// `now64(9)` value, so the matrix pivot collapses N anchors onto one
// "now" point.
//
// The fix mirrors the structural template established by Pool-AK's
// per-step LWR rework (PR #347) and the matrix fan-out for
// quantile_over_time / predict_linear / holt_winters (PRs #348 / #349):
// in range mode the rewrite cross-joins the histogram-table scan with a
// `chplan.StepGrid`, filters each (sample, anchor) pair to the per-anchor
// lookback window, aggregates BucketCounts / ExplicitBounds per (series,
// anchor), and surfaces `anchor_ts` as the per-row TimeUnix.
//
// The bare-selector and aggregated (`sum by(le)(rate(<bucket>[r]))`)
// shapes share the rewrite scaffold; they differ only in:
//
//   - The per-anchor lookback duration: `instantLookback` (5m) for the
//     bare-selector path, `shape.windowRange` for the aggregated path.
//   - The bucket aggregation function: `argMax(BucketCounts, TimeUnix)`
//     + `argMax(ExplicitBounds, TimeUnix)` for the bare path (LWR-like
//     "latest histogram sample per (series, anchor)"); `sumForEach`
//     + `any` for the aggregated path (sums element-wise across rows
//     in the rate window, same as instant mode).
//   - The group-by labels: full Attributes for bare; user-supplied
//     `by/without` clause for aggregated.
//
// Both variants surface the canonical 4-column Sample row contract to
// downstream consumers (matrix pivot in handler.go), keyed by the
// per-step anchor_ts.
//
// Selectors carrying `@`/offset modifiers fall through to the instant-
// mode path; range-mode matrix-anchor handling for histogram_quantile
// under modifiers is not implemented. In practice the modifier-bearing
// shapes are rare (Grafana never emits them on query_range), so the
// instant-mode fallback is byte-stable for the existing fixtures and
// the range-mode rewrite covers the wire-shape Grafana actually drives.

const histogramAnchorCol = "anchor_ts"

// histogramRangeApplies reports whether the lowering context has a
// non-zero step and a populated start/end so the range-mode rewrite can
// fan an anchor grid across the request window. Mirrors the gate
// lowerRangeVectorCall uses for the matrix RangeWindow fan-out.
func histogramRangeApplies(ctx lowerCtx) bool {
	return ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero()
}

// lowerHistogramQuantileClassicBareRange builds the range-mode plan tree
// for `histogram_quantile(phi, <bare-VectorSelector>)`.
//
// The tree mirrors lowerHistogramQuantileClassicAggRange — the lookback
// derives from instantLookback (PromQL's 5-minute staleness default),
// the bucket aggregation is `argMax` (LWR-canonical "newest histogram
// sample in window"), and there is no user `by/without` clause so the
// GroupBy is the full Attributes column.
func lowerHistogramQuantileClassicBareRange(
	vs *parser.VectorSelector,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	scan := &chplan.Scan{Table: s.HistogramTable}
	// `_bucket` suffix strip — see stripBucketSuffix in
	// histogram_quantile.go. Grafana classic-histogram dashboards
	// fire `rate(<X>_bucket[r])`; the OTel-CH histogram row carries
	// the bare `<X>` MetricName, so the strip is what makes the
	// filter find rows.
	pred := buildPredicate(stripBucketSuffix(vs.LabelMatchers), s)

	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	groupByAliases := []string{s.AttributesColumn}
	attrsRebuild := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})
	bucketAggs := []chplan.AggFunc{
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.BucketCountsColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.BucketCountsColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ExplicitBoundsColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.ExplicitBoundsColumn,
		},
	}
	return buildHistogramRangeTree(
		scan, pred, instantLookback,
		groupBy, groupByAliases, attrsRebuild,
		bucketAggs, phi, s, ctx,
	)
}

// lowerHistogramQuantileClassicAggRange builds the range-mode plan tree
// for `histogram_quantile(phi, sum [by/without] (rate(<bucket>[r])))`.
//
// The lookback is the rate's [range] duration; the bucket aggregation is
// sumForEach (mirrors the instant-mode classic-agg path's element-wise
// sum across all rows in the rate window).
func lowerHistogramQuantileClassicAggRange(
	shape histogramAggShape,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	vs := shape.selector
	scan := &chplan.Scan{Table: s.HistogramTable}
	// `_bucket` suffix strip — see stripBucketSuffix in
	// histogram_quantile.go.
	pred := buildPredicate(stripBucketSuffix(vs.LabelMatchers), s)

	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(shape.agg, s)
	bucketAggs := []chplan.AggFunc{
		{
			Name:  "sumForEach",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.BucketCountsColumn}},
			Alias: s.BucketCountsColumn,
		},
		{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}},
			Alias: s.ExplicitBoundsColumn,
		},
	}
	return buildHistogramRangeTree(
		scan, pred, shape.windowRange,
		groupBy, groupByAliases, attrsRebuild,
		bucketAggs, phi, s, ctx,
	)
}

// buildHistogramRangeTree assembles the shared range-mode plan tree
// for the classic-histogram quantile rewrites. The bare-selector and
// aggregated paths pass distinct (lookback, groupBy, bucketAggs)
// values; the resulting tree shape is otherwise identical.
//
// Plan shape (in chsql output order):
//
//	Project [MetricName='', Attributes, anchor_ts AS TimeUnix, Value]
//	  HistogramQuantile phi groupBy=[anchor_ts, Attributes]
//	    Project [anchor_ts, <attrs-rebuilt>, BucketCounts, ExplicitBounds]
//	      Aggregate groupBy=[anchor_ts, <user-labels>] funcs=<bucketAggs>
//	        Filter (TimeUnix > anchor_ts - <lookback> AND TimeUnix <= anchor_ts)
//	          CrossJoin(StepGrid(start, end, step), Filter(Scan, <matchers>))
func buildHistogramRangeTree(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	bucketAggs []chplan.AggFunc,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	// Pre-StepGrid scan-side filter: apply label matchers so the
	// CrossJoin's right side is already metric-bounded; this is the
	// PREWHERE-eligible shape the optimizer keeps fast.
	var rawSide chplan.Node = scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: scan, Predicate: pred}
	}

	stepGrid := &chplan.StepGrid{
		Start: ctx.start.UTC(),
		End:   ctx.end.UTC(),
		Step:  ctx.step,
	}
	joined := &chplan.CrossJoin{
		Left:  stepGrid,
		Right: rawSide,
	}

	// Per-anchor lookback window:
	//   TimeUnix > anchor_ts - <lookback>  AND  TimeUnix <= anchor_ts
	//
	// Left-open / right-closed mirrors the Prom staleness convention
	// applied by stalenessLowerBoundExpr / timeBoundExpr for the
	// non-histogram LWR path.
	lwrUpper := &chplan.Binary{
		Op:    chplan.OpLe,
		Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: anchorRef,
	}
	lwrLower := &chplan.Binary{
		Op:   chplan.OpGt,
		Left: &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: &chplan.Binary{
			Op:   chplan.OpSub,
			Left: anchorRef,
			Right: &chplan.FuncCall{
				Name: "toIntervalNanosecond",
				Args: []chplan.Expr{&chplan.LitInt{V: lookback.Nanoseconds()}},
			},
		},
	}
	filtered := &chplan.Filter{
		Input: joined,
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd, Left: lwrUpper, Right: lwrLower,
		},
	}

	// Aggregate per (anchor_ts, <user-group-keys>). For the bare path
	// `userGroupBy` is `[Attributes]`; for the agg path it's the
	// user-supplied `by/without` projection (already prepared by
	// histogramAggGroupBy).
	aggGroupBy := append([]chplan.Expr{anchorRef}, userGroupBy...)
	aggAliases := append([]string{histogramAnchorCol}, userAliases...)
	agg := &chplan.Aggregate{
		Input:              filtered,
		GroupBy:            aggGroupBy,
		GroupByAliases:     aggAliases,
		AggFuncs:           bucketAggs,
		DropEmptyOnNoGroup: true,
	}

	// Reshape the aggregate output into the histogram-row contract
	// HistogramQuantile consumes (Attributes + BucketCounts + ExplicitBounds)
	// while preserving anchor_ts as a passthrough column so the
	// downstream HistogramQuantile GroupBy can pick it up.
	rebuilt := &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: anchorRef, Alias: histogramAnchorCol},
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
			{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
		},
	}

	// HistogramQuantile emits one row per (anchor, series). The
	// emitter's SELECT projects each GroupBy entry (anchor_ts +
	// Attributes) then the per-row quantile-interpolation expression as
	// `Value`. The outer Project re-aliases anchor_ts → TimeUnix so the
	// canonical Sample contract holds for the matrix pivot.
	hq := &chplan.HistogramQuantile{
		Input:                rebuilt,
		Phi:                  phi,
		BucketCountsColumn:   s.BucketCountsColumn,
		ExplicitBoundsColumn: s.ExplicitBoundsColumn,
		GroupBy: []chplan.Expr{
			anchorRef,
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{histogramAnchorCol, s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchorRef, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// lowerHistogramQuantileNativeBareRange builds the range-mode plan tree
// for `histogram_quantile(phi, <bare-exp-hist-VectorSelector>)`.
//
// The shape mirrors lowerHistogramQuantileClassicBareRange exactly,
// substituting the per-row exp-histogram fields (Scale / ZeroCount /
// ZeroThreshold / PositiveOffset / PositiveBucketCounts /
// NegativeOffset / NegativeBucketCounts) for the classic-side
// BucketCounts + ExplicitBounds pair. The per-anchor LWR projects the
// newest exp-histogram row per (series, anchor) via argMax(<col>, TimeUnix)
// before HistogramQuantileNative walks the merged distribution.
//
// Plan shape (in chsql output order):
//
//	Project [MetricName='', Attributes, anchor_ts AS TimeUnix, Value]
//	  HistogramQuantileNative phi groupBy=[anchor_ts, Attributes]
//	    Project [anchor_ts, Attributes, Scale, ZeroCount, ZeroThreshold,
//	             PositiveOffset, PositiveBucketCounts,
//	             NegativeOffset, NegativeBucketCounts]
//	      Aggregate groupBy=[anchor_ts, Attributes] funcs=[
//	          argMax(Scale, TimeUnix), argMax(ZeroCount, TimeUnix),
//	          argMax(ZeroThreshold, TimeUnix),
//	          argMax(PositiveOffset, TimeUnix),
//	          argMax(PositiveBucketCounts, TimeUnix),
//	          argMax(NegativeOffset, TimeUnix),
//	          argMax(NegativeBucketCounts, TimeUnix)]
//	        Filter (TimeUnix > anchor_ts - 5m AND TimeUnix <= anchor_ts)
//	          CrossJoin(StepGrid(start, end, step), Filter(Scan, <matchers>))
func lowerHistogramQuantileNativeBareRange(
	vs *parser.VectorSelector,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	groupByAliases := []string{s.AttributesColumn}
	attrsRebuild := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})

	// LWR aggregation: latest exp-histogram fields per (series, anchor).
	// argMax(<col>, TimeUnix) picks the value at the row with the highest
	// TimeUnix in the (series, anchor) group, matching the Phase-1 instant-
	// mode "newest sample" semantic with anchor swapped in for `now64(9)`.
	expHistAggs := []chplan.AggFunc{
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ScaleColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.ScaleColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ZeroCountColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.ZeroCountColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ZeroThresholdColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.ZeroThresholdColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.PositiveOffsetColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.PositiveOffsetColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.PositiveBucketCountsColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.NegativeOffsetColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.NegativeOffsetColumn,
		},
		{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.NegativeBucketCountsColumn,
		},
	}
	return buildHistogramNativeRangeTree(
		scan, pred, instantLookback,
		groupBy, groupByAliases, attrsRebuild,
		expHistAggs, phi, s, ctx,
	)
}

// lowerHistogramQuantileNativeAggRange builds the range-mode plan tree
// for `histogram_quantile(phi, sum [by/without] (rate(<sel>_exp_hist[r])))`.
//
// The lookback is the rate's [range] duration; the per-anchor aggregation
// collects per-row exp-histogram fields into groupArrays so the wrapping
// reshape Project can fold them into a single merged distribution per
// (anchor, series) via the same expHistogramMergeOffsetExpr /
// expHistogramMergeBucketsExpr helpers used by the instant path.
//
// Plan shape (in chsql output order):
//
//	Project [MetricName='', Attributes, anchor_ts AS TimeUnix, Value]
//	  HistogramQuantileNative phi groupBy=[anchor_ts, Attributes]
//	    Project [anchor_ts, <attrs-rebuilt>, merged Scale / ZeroCount /
//	             ZeroThreshold / {Pos,Neg}{Offset,BucketCounts}]
//	      Aggregate groupBy=[anchor_ts, <user-labels>] funcs=<merge aggs>
//	        Filter (TimeUnix > anchor_ts - <windowRange> AND TimeUnix <= anchor_ts)
//	          CrossJoin(StepGrid, Filter(Scan, <matchers>))
func lowerHistogramQuantileNativeAggRange(
	shape histogramAggShape,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	vs := shape.selector
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(shape.agg, s)

	// Per-anchor merge aggregates: collect rows into groupArrays + simple
	// reducers, matching the instant-mode aggregated path. The wrapping
	// reshape Project folds them into a single merged distribution.
	expHistMergeAggs := []chplan.AggFunc{
		{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
		{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: s.ZeroCountColumn},
		{Name: "max", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}}, Alias: s.ZeroThresholdColumn},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
	}
	return buildHistogramNativeRangeTreeMerge(
		scan, pred, shape.windowRange,
		groupBy, groupByAliases, attrsRebuild,
		expHistMergeAggs, phi, s, ctx,
	)
}

// buildHistogramNativeRangeTree assembles the bare-selector range-mode
// plan tree for the native-histogram quantile rewrite. The Aggregate
// surfaces the per-row exp-histogram fields directly under their
// schema-canonical names so the wrapping reshape Project can pass them
// through unchanged into HistogramQuantileNative.
func buildHistogramNativeRangeTree(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	expHistAggs []chplan.AggFunc,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	filtered, _ := buildHistogramRangeAnchorJoin(scan, pred, lookback, anchorRef, s, ctx)

	aggGroupBy := append([]chplan.Expr{anchorRef}, userGroupBy...)
	aggAliases := append([]string{histogramAnchorCol}, userAliases...)
	agg := &chplan.Aggregate{
		Input:              filtered,
		GroupBy:            aggGroupBy,
		GroupByAliases:     aggAliases,
		AggFuncs:           expHistAggs,
		DropEmptyOnNoGroup: true,
	}

	// Pass-through reshape: anchor_ts + attrs + per-row exp-histogram
	// fields (already aliased to their schema-canonical names by the
	// Aggregate).
	rebuilt := &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: anchorRef, Alias: histogramAnchorCol},
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.ScaleColumn}, Alias: s.ScaleColumn},
			{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
			{Expr: &chplan.ColumnRef{Name: s.ZeroThresholdColumn}, Alias: s.ZeroThresholdColumn},
			{Expr: &chplan.ColumnRef{Name: s.PositiveOffsetColumn}, Alias: s.PositiveOffsetColumn},
			{Expr: &chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}, Alias: s.PositiveBucketCountsColumn},
			{Expr: &chplan.ColumnRef{Name: s.NegativeOffsetColumn}, Alias: s.NegativeOffsetColumn},
			{Expr: &chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}, Alias: s.NegativeBucketCountsColumn},
		},
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      rebuilt,
		Phi:                        phi,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			anchorRef,
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{histogramAnchorCol, s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchorRef, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// buildHistogramNativeRangeTreeMerge assembles the aggregated-idiom
// range-mode plan tree for the native-histogram quantile rewrite. The
// Aggregate surfaces groupArrays + simple reducers; the wrapping
// reshape Project then folds them into a single merged distribution
// per (anchor, series) using the same expHistogramMerge* helpers as
// the instant-mode aggregated path.
func buildHistogramNativeRangeTreeMerge(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	mergeAggs []chplan.AggFunc,
	phi float64,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	filtered, _ := buildHistogramRangeAnchorJoin(scan, pred, lookback, anchorRef, s, ctx)

	aggGroupBy := append([]chplan.Expr{anchorRef}, userGroupBy...)
	aggAliases := append([]string{histogramAnchorCol}, userAliases...)
	agg := &chplan.Aggregate{
		Input:              filtered,
		GroupBy:            aggGroupBy,
		GroupByAliases:     aggAliases,
		AggFuncs:           mergeAggs,
		DropEmptyOnNoGroup: true,
	}

	// Reshape: fold per-row arrays into a single merged distribution.
	// Mirrors the inner Project in lowerHistogramQuantileNativeAgg.
	rebuilt := &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: anchorRef, Alias: histogramAnchorCol},
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
			{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
			{Expr: &chplan.ColumnRef{Name: s.ZeroThresholdColumn}, Alias: s.ZeroThresholdColumn},
			{Expr: expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveOffsetColumn},
			{Expr: expHistogramMergeBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveBucketCountsColumn},
			{Expr: expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeOffsetColumn},
			{Expr: expHistogramMergeBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeBucketCountsColumn},
		},
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      rebuilt,
		Phi:                        phi,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			anchorRef,
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{histogramAnchorCol, s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: anchorRef, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// buildHistogramRangeAnchorJoin renders the pre-Aggregate shape shared
// by every histogram-range lowering (classic + native, bare + agg):
// scan-side metric-bounded Filter → CrossJoin against a StepGrid →
// per-anchor lookback Filter. Returns the filtered subtree plus the
// joined CrossJoin (the latter exposed so callers that want to wrap
// the join itself stay flexible). The classic path duplicates this
// shape inline; the duplication is scoped to one file so unrelated
// callers stay untouched.
func buildHistogramRangeAnchorJoin(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	anchorRef *chplan.ColumnRef,
	s schema.Metrics,
	ctx lowerCtx,
) (filtered, joined chplan.Node) {
	var rawSide chplan.Node = scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: scan, Predicate: pred}
	}

	stepGrid := &chplan.StepGrid{
		Start: ctx.start.UTC(),
		End:   ctx.end.UTC(),
		Step:  ctx.step,
	}
	cj := &chplan.CrossJoin{
		Left:  stepGrid,
		Right: rawSide,
	}

	lwrUpper := &chplan.Binary{
		Op:    chplan.OpLe,
		Left:  &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: anchorRef,
	}
	lwrLower := &chplan.Binary{
		Op:   chplan.OpGt,
		Left: &chplan.ColumnRef{Name: s.TimestampColumn},
		Right: &chplan.Binary{
			Op:   chplan.OpSub,
			Left: anchorRef,
			Right: &chplan.FuncCall{
				Name: "toIntervalNanosecond",
				Args: []chplan.Expr{&chplan.LitInt{V: lookback.Nanoseconds()}},
			},
		},
	}
	return &chplan.Filter{
		Input: cj,
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd, Left: lwrUpper, Right: lwrLower,
		},
	}, cj
}
