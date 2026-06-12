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
// in range mode the rewrite fans each histogram sample over only the
// anchors whose per-anchor lookback window covers it (the single-pass
// `chplan.RangeBucketFanout` introduced for histograms by the
// RangeLWR-style rework, #804), aggregates BucketCounts /
// ExplicitBounds per (series, anchor), and surfaces `anchor_ts` as the
// per-row TimeUnix.
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
// under modifiers falls back to instant mode. In practice the modifier-bearing
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
	phi phiArg,
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
	phi phiArg,
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
//	      RangeBucketFanout groupBy=[<user-labels>] funcs=<bucketAggs>
//	        Filter(Scan, <matchers>)
//
// The RangeBucketFanout node replaces the O(rows × N) StepGrid CROSS
// JOIN + per-anchor lookback Filter + per-(series, anchor) Aggregate
// that earlier revisions emitted with the single-pass bounded
// sample-side fan-out RangeLWR (#804) introduced — see
// chplan.RangeBucketFanout for the semantics.
func buildHistogramRangeTree(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	bucketAggs []chplan.AggFunc,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, lookback, userGroupBy, userAliases, bucketAggs, s, ctx)

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
		Phi:                  phi.lit,
		PhiExpr:              phi.expr,
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
//	      RangeBucketFanout groupBy=[Attributes] funcs=[
//	          argMax(Scale, TimeUnix), argMax(ZeroCount, TimeUnix),
//	          argMax(ZeroThreshold, TimeUnix),
//	          argMax(PositiveOffset, TimeUnix),
//	          argMax(PositiveBucketCounts, TimeUnix),
//	          argMax(NegativeOffset, TimeUnix),
//	          argMax(NegativeBucketCounts, TimeUnix)]
//	        Filter(Scan, <matchers>)
func lowerHistogramQuantileNativeBareRange(
	vs *parser.VectorSelector,
	phi phiArg,
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
	// argMax(ZeroThreshold, TimeUnix) only exists when the physical
	// schema persists the OTLP zero_threshold field — the upstream
	// OTel-CH DDL doesn't, so the default schema leaves the column
	// empty and the emitter renders a constant-0 zero-bucket width.
	if s.ZeroThresholdColumn != "" {
		expHistAggs = append(expHistAggs, chplan.AggFunc{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ZeroThresholdColumn},
				&chplan.ColumnRef{Name: s.TimestampColumn},
			},
			Alias: s.ZeroThresholdColumn,
		})
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
//	      RangeBucketFanout groupBy=[<user-labels>] funcs=<merge aggs>
//	        Filter(Scan, <matchers>)
func lowerHistogramQuantileNativeAggRange(
	shape histogramAggShape,
	phi phiArg,
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
	}
	// max(ZeroThreshold) only when the physical schema persists the
	// OTLP zero_threshold field — the upstream OTel-CH DDL doesn't.
	if s.ZeroThresholdColumn != "" {
		expHistMergeAggs = append(expHistMergeAggs, chplan.AggFunc{Name: "max", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}}, Alias: s.ZeroThresholdColumn})
	}
	expHistMergeAggs = append(expHistMergeAggs, []chplan.AggFunc{
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
	}...)
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
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, lookback, userGroupBy, userAliases, expHistAggs, s, ctx)

	// Pass-through reshape: anchor_ts + attrs + per-row exp-histogram
	// fields (already aliased to their schema-canonical names by the
	// fanout).
	rebuiltProjs := []chplan.Projection{
		{Expr: anchorRef, Alias: histogramAnchorCol},
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: s.ScaleColumn}, Alias: s.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		rebuiltProjs = append(rebuiltProjs, chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ZeroThresholdColumn}, Alias: s.ZeroThresholdColumn})
	}
	rebuiltProjs = append(rebuiltProjs, []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.PositiveOffsetColumn}, Alias: s.PositiveOffsetColumn},
		{Expr: &chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}, Alias: s.PositiveBucketCountsColumn},
		{Expr: &chplan.ColumnRef{Name: s.NegativeOffsetColumn}, Alias: s.NegativeOffsetColumn},
		{Expr: &chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}, Alias: s.NegativeBucketCountsColumn},
	}...)
	rebuilt := &chplan.Project{
		Input:       agg,
		Projections: rebuiltProjs,
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      rebuilt,
		Phi:                        phi.lit,
		PhiExpr:                    phi.expr,
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
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, lookback, userGroupBy, userAliases, mergeAggs, s, ctx)

	// Reshape: fold per-row arrays into a single merged distribution.
	// Mirrors the inner Project in lowerHistogramQuantileNativeAgg.
	mergeProjs := []chplan.Projection{
		{Expr: anchorRef, Alias: histogramAnchorCol},
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		mergeProjs = append(mergeProjs, chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ZeroThresholdColumn}, Alias: s.ZeroThresholdColumn})
	}
	mergeProjs = append(mergeProjs, []chplan.Projection{
		{Expr: expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveBucketCountsColumn},
		{Expr: expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeBucketCountsColumn},
	}...)
	rebuilt := &chplan.Project{
		Input:       agg,
		Projections: mergeProjs,
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      rebuilt,
		Phi:                        phi.lit,
		PhiExpr:                    phi.expr,
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

// buildHistogramBucketFanout renders the single-pass bounded
// sample-side fan-out shared by every histogram-range lowering (classic
// + native, bare + agg, plus the native value-function path): a
// scan-side metric-bounded Filter feeding a chplan.RangeBucketFanout
// that fans each sample over only the ≤ lookback/step + 1 anchors whose
// half-open staleness window covers it, then collapses each (series,
// anchor) bucket with the variant-specific aggregate funcs.
//
// This supersedes the StepGrid CROSS JOIN + per-anchor lookback Filter +
// per-(series, anchor) Aggregate that earlier revisions emitted — the
// same O(rows × N) compute fan-out RangeLWR (#804) killed for bare
// selectors. The output schema is byte-identical to the Aggregate node
// it replaces: `(anchor_ts, <userAliases...>, <aggFuncs[i].Alias...>)`,
// so the wrapping reshape Project + HistogramQuantile{,Native} consume
// it unchanged.
//
// The anchor key is implicit — it is prepended by the fanout node under
// histogramAnchorCol — so callers pass ONLY the user group keys in
// userGroupBy / userAliases (the full Attributes column for the bare
// paths, the `by/without` projection for the aggregated paths).
func buildHistogramBucketFanout(
	scan *chplan.Scan,
	pred chplan.Expr,
	lookback time.Duration,
	userGroupBy []chplan.Expr,
	userAliases []string,
	aggFuncs []chplan.AggFunc,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	// Scan-side metric-bounded Filter: apply label matchers so the
	// fan-out reads a metric-bounded row set; this is the
	// PREWHERE-eligible shape the optimizer keeps fast.
	var rawSide chplan.Node = scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: scan, Predicate: pred}
	}

	return &chplan.RangeBucketFanout{
		Input:          rawSide,
		Start:          ctx.start.UTC(),
		End:            ctx.end.UTC(),
		Step:           ctx.step,
		Lookback:       lookback,
		GroupBy:        userGroupBy,
		GroupByAliases: userAliases,
		AggFuncs:       aggFuncs,
		AnchorAlias:    histogramAnchorCol,
		TimestampCol:   s.TimestampColumn,
	}
}
