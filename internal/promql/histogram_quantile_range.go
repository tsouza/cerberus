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
//     for the aggregated path (sums element-wise across rows in the
//     rate window, same as instant mode).
//   - The group-by labels: full Attributes for bare; user-supplied
//     `by/without` clause plus the bucket layout for aggregated. The
//     bare path needs no layout key — argMax reads counts AND bounds
//     off the one newest row, so its ladder is consistent by
//     construction.
//
// Both variants surface the canonical 4-column Sample row contract to
// downstream consumers (matrix pivot in handler.go), keyed by the
// per-step anchor_ts.
//
// Modifiers split the same three ways every other range-vector lowering
// splits them, through the one [rangeGridShapeFor] decision:
//
//   - `offset <d>` is a relative shift against each step's eval time, so
//     it rides the fan-out: the per-anchor window becomes
//     `(anchor - offset - lookback, anchor - offset]` via
//     [chplan.RangeBucketFanout.Offset], and the emitted anchor timestamp
//     is unchanged.
//   - An absolute `@` pin fixes the window for the WHOLE query, so the
//     quantile is evaluated once by the instant-mode lowering (which
//     already resolves the pin through [anchorFromSelector]) and
//     [broadcastHistogramAtPin] fans that per-series value across the step
//     grid.
//
// Neither case may fall back to a bare instant lowering: that emits one
// row per series stamped at a single anchor, which the matrix pivot
// renders as a single point for the entire requested range.

const histogramAnchorCol = "anchor_ts"

// stalenessMinSamples is the sample floor for a staleness-lookback window:
// a bare selector resolves to at most one sample, so one is enough.
const stalenessMinSamples = 1

// rateMinSamples is the sample floor reference PromQL's rate / increase
// family imposes — two points are needed to span a delta, and an anchor
// whose window holds fewer emits NOTHING (not a zero). The leading steps of
// a range query are exactly where that bites: the first anchors see only
// the one scrape at the range start.
const rateMinSamples = 2

// histogramWindow is the per-anchor sample window a histogram fan-out
// reads: `(anchor - offset - lookback, anchor - offset]`, together with the
// number of samples that window must hold for the anchor to emit. lookback
// is the staleness horizon (instantLookback for the bare / value-fn paths,
// the inner range-vector function's `[range]` for the aggregated ones);
// offset is the selector's `offset` modifier, which shifts the window
// without moving the emitted anchor timestamp; minSamples is the collapsed
// function's "no sample emitted" floor. Keeping the three together means a
// lowering cannot pass one and forget the others — dropping offset silently
// reads the wrong window, and dropping minSamples silently emits at anchors
// reference PromQL leaves empty, rather than failing.
type histogramWindow struct {
	lookback   time.Duration
	offset     time.Duration
	minSamples int
}

// windowFor builds the staleness fan-out window for a bare selector.
func windowFor(vs *parser.VectorSelector, lookback time.Duration) histogramWindow {
	return histogramWindow{lookback: lookback, offset: vs.OriginalOffset, minSamples: stalenessMinSamples}
}

// aggWindowFor builds the fan-out window for the aggregated idiom
// `sum by(le)(<fn>(<bucket>[range]))`. The lookback is the matched
// `[range]`; the sample floor is the matched function's, so a `rate` window
// drops the leading anchors that hold a single scrape while a
// `sum_over_time` window keeps them.
func aggWindowFor(shape histogramAggShape) histogramWindow {
	return histogramWindow{
		lookback:   shape.windowRange,
		offset:     shape.selector.OriginalOffset,
		minSamples: shape.windowMinSamples,
	}
}

// latestSampleAgg collapses a filtered histogram scan to the newest
// sample per series. Reference PromQL resolves a bare selector to at most
// ONE sample per series, so without this collapse the quantile is
// evaluated against every stored sample and the same series is emitted
// once per row it happens to have. The range lowerings get the identical
// collapse from [chplan.RangeBucketFanout], keyed by (series, anchor)
// instead of series alone.
//
// The group key goes through [canonicalGroupKeyExpr]: this reads the raw
// table column, so it is a series-identity BINDING site, and without the
// wrap one logical series stored under two Map key orders collapses as
// two groups — each keeping only its own subset's newest sample.
func latestSampleAgg(input chplan.Node, aggs []chplan.AggFunc, s schema.Metrics) chplan.Node {
	return &chplan.Aggregate{
		Input:              input,
		GroupBy:            []chplan.Expr{histogramIdentityExpr(s)},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           aggs,
		DropEmptyOnNoGroup: true,
	}
}

// histogramIdentityExpr is the Prometheus-visible identity for histogram
// rows. Histogram lowerings group directly over their scans, bypassing the
// selector projection that ordinarily merges ResourceAttributes into labels.
func histogramIdentityExpr(s schema.Metrics) chplan.Expr {
	return selectorAttributesSource(nil, s)
}

// classicBucketLatestAggs renders the newest-sample aggregates for a
// classic histogram row. argMax(<col>, TimeUnix) picks the value at the
// row with the highest TimeUnix in the group — the series for the instant
// path, the (series, anchor) pair for the fan-out.
func classicBucketLatestAggs(s schema.Metrics) []chplan.AggFunc {
	return []chplan.AggFunc{
		latestArgMax(s.BucketCountsColumn, s),
		latestArgMax(s.ExplicitBoundsColumn, s),
	}
}

// nativeExpHistLatestAggs is classicBucketLatestAggs for an exponential
// histogram row: the per-row Scale / ZeroCount / ±Offset / ±BucketCounts
// fields the merge and interpolation kernels read.
func nativeExpHistLatestAggs(s schema.Metrics) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		latestArgMax(s.ScaleColumn, s),
		latestArgMax(s.ZeroCountColumn, s),
		latestArgMax(s.PositiveOffsetColumn, s),
		latestArgMax(s.PositiveBucketCountsColumn, s),
		latestArgMax(s.NegativeOffsetColumn, s),
		latestArgMax(s.NegativeBucketCountsColumn, s),
	}
	// ZeroThreshold only exists when the physical schema persists the
	// OTLP zero_threshold field — the upstream OTel-CH DDL doesn't, so
	// the default schema leaves the column empty and the emitter renders
	// a constant-0 zero-bucket width.
	if s.ZeroThresholdColumn != "" {
		aggs = append(aggs, latestArgMax(s.ZeroThresholdColumn, s))
	}
	return aggs
}

// latestArgMax is `argMax(<col>, TimeUnix) AS <col>`.
func latestArgMax(col string, s schema.Metrics) chplan.AggFunc {
	return chplan.AggFunc{
		Name: "argMax",
		Args: []chplan.Expr{
			&chplan.ColumnRef{Name: col},
			&chplan.ColumnRef{Name: s.TimestampColumn},
		},
		Alias: col,
	}
}

// broadcastHistogramAtPin fans a histogram lowering pinned by an absolute
// `@` across the request's step grid. inner is the INSTANT-mode tree —
// one row per series at the pinned anchor, in the canonical Sample
// contract — and reference PromQL evaluates that same pinned window at
// every step, varying only the output timestamp. A CrossJoin with a
// StepGrid over `[start, end]` supplies the timestamps and the outer
// Project restamps TimeUnix from the grid anchor, so downstream consumers
// see the identical 4-column shape the fan-out path emits.
//
// `anchor_ts` comes from the grid and `Attributes` / `Value` from inner,
// so the bare names resolve unambiguously. inner's own TimeUnix is the
// evaluation stamp every instant-mode histogram lowering emits — a plain
// `now64(9)`, never the pinned anchor — so the grid's `anchor_ts` has to
// replace it rather than merge with it, and it is simply not projected.
// Derived histogram samples drop `__name__`, so MetricName is the empty
// literal on both paths.
func broadcastHistogramAtPin(inner chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
	return &chplan.Project{
		Input: &chplan.CrossJoin{Left: grid, Right: inner},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: histogramAnchorCol}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
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
	pred := histogramQuantileMatcherPredicate(vs.LabelMatchers, s)

	groupBy := []chplan.Expr{histogramIdentityExpr(s)}
	groupByAliases := []string{s.AttributesColumn}
	attrsRebuild := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})
	return buildHistogramRangeTree(
		scan, pred, windowFor(vs, instantLookback),
		groupBy, groupByAliases, attrsRebuild,
		classicBucketShaping{aggs: classicBucketLatestAggs(s)}, phi, s, ctx,
	)
}

// lowerHistogramQuantileClassicAggRange builds the range-mode plan tree
// for `histogram_quantile(phi, sum [by/without] (rate(<bucket>[r])))`.
//
// The lookback is the rate's [range] duration; the bucket aggregation
// collects every in-window row's layout and counts per anchor and merges
// them over the union of bounds, mirroring the instant-mode classic-agg
// path — see classicBucketMergeShaping and classicBucketMergedLadderExpr.
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
	pred := histogramQuantileMatcherPredicate(vs.LabelMatchers, s)

	userGroupBy, userAliases, attrsRebuild := histogramAggGroupBy(shape.agg, s)
	return buildHistogramRangeTree(
		scan, pred, aggWindowFor(shape),
		userGroupBy, userAliases, attrsRebuild,
		classicBucketMergeShaping(shape.classicFold, s), phi, s, ctx,
	)
}

// buildHistogramRangeTree assembles the shared range-mode plan tree
// for the classic-histogram quantile rewrites. The bare-selector and
// aggregated paths pass distinct (lookback, groupBy, shaping) values;
// the resulting tree shape is otherwise identical.
//
// Plan shape (in chsql output order):
//
//	Project [MetricName='', Attributes, anchor_ts AS TimeUnix, Value]
//	  HistogramQuantile phi groupBy=[anchor_ts, Attributes]
//	    <shaping reshape: one Project for the newest-row path, two for
//	     the layout merge — see classicBucketShaping.reshape>
//	      RangeBucketFanout groupBy=[<user-labels>] funcs=<shaping.aggs>
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
	win histogramWindow,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	shaping classicBucketShaping,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, win, userGroupBy, userAliases, shaping.aggs, s, ctx)

	// Reshape the aggregate output into the histogram-row contract
	// HistogramQuantile consumes (Attributes + BucketCounts + ExplicitBounds)
	// while preserving anchor_ts as a passthrough column so the
	// downstream HistogramQuantile GroupBy can pick it up.
	rebuilt, cumulative := shaping.reshape(agg, []chplan.Projection{
		{Expr: anchorRef, Alias: histogramAnchorCol},
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
	}, s)

	// HistogramQuantile emits one row per (anchor, series). The
	// emitter's SELECT projects each GroupBy entry (anchor_ts +
	// Attributes) then the per-row quantile-interpolation expression as
	// `Value`. The outer Project re-aliases anchor_ts → TimeUnix so the
	// canonical Sample contract holds for the matrix pivot.
	hq := &chplan.HistogramQuantile{
		Input:                  rebuilt,
		Phi:                    phi.lit,
		PhiExpr:                phi.expr,
		BucketCountsColumn:     s.BucketCountsColumn,
		ExplicitBoundsColumn:   s.ExplicitBoundsColumn,
		BucketCountsCumulative: cumulative,
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

	groupBy := []chplan.Expr{histogramIdentityExpr(s)}
	groupByAliases := []string{s.AttributesColumn}
	attrsRebuild := chplan.Expr(&chplan.ColumnRef{Name: s.AttributesColumn})

	return buildHistogramNativeRangeTree(
		scan, pred, windowFor(vs, instantLookback),
		groupBy, groupByAliases, attrsRebuild,
		nativeExpHistLatestAggs(s), phi, s, ctx,
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
		scan, pred, aggWindowFor(shape),
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
	win histogramWindow,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	expHistAggs []chplan.AggFunc,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, win, userGroupBy, userAliases, expHistAggs, s, ctx)

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
	win histogramWindow,
	userGroupBy []chplan.Expr,
	userAliases []string,
	attrsRebuild chplan.Expr,
	mergeAggs []chplan.AggFunc,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: histogramAnchorCol}

	agg := buildHistogramBucketFanout(scan, pred, win, userGroupBy, userAliases, mergeAggs, s, ctx)

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
	win histogramWindow,
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
		Input:    rawSide,
		Start:    ctx.start.UTC(),
		End:      ctx.end.UTC(),
		Step:     ctx.step,
		Lookback: win.lookback,
		Offset:   win.offset,
		// The fan-out keys read the raw table column, so this is a
		// series-identity binding site — see [canonicalGroupKeyExpr].
		// Canonicalising here rather than at each caller keeps the one
		// path that reaches this node from splitting into a canonical
		// and a non-canonical variant.
		GroupBy:        canonicalGroupKeyExprs(userGroupBy, s),
		GroupByAliases: userAliases,
		AggFuncs:       aggFuncs,
		MinSamples:     win.minSamples,
		AnchorAlias:    histogramAnchorCol,
		TimestampCol:   s.TimestampColumn,
	}
}
