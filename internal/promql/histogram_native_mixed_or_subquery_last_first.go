package promql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_last_first.go composes
// `last_over_time`/`first_over_time` over a sum/avg-wrapped mixed
// float/histogram `or` subquery — `<fn>((sum by (series) ((a) or
// (b)))[range:step])` — cerberus issue #2714, the last of the three gaps
// #2615 named as deliberately unattempted by this package's siblings
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go's own doc,
// "Scope" section).
//
// # Why this needs its own machinery rather than either existing composer
//
// Reference Prometheus's funcLastOverTime/funcFirstOverTime
// (histogram_native_last_first_over_time.go's own doc has the citation)
// read the window's raw samples directly and pick ONE — the newest for
// last_over_time, the oldest for first_over_time — by timestamp, returning
// it VERBATIM, whichever type it happens to be. Unlike the seven FOLD-family
// names (histogram_native_mixed_or_subquery_aggregate_range_fn.go), there is
// no window-purity drop test at all: a window holding both a histogram and
// a float sample answers with whichever one wins the timestamp comparison,
// not an empty result. And unlike resets/changes
// (histogram_native_mixed_or_subquery_resets_changes.go), there is no
// per-pair sequential verdict either — this is a single per-group
// selection, the same shape [nativeExpHistBareAggsDirectional] already
// answers for a PURE histogram-native window
// (histogram_native_last_first_over_time.go) and
// [selectFnOverSubqueryWindowed] already reuses for a pure
// histogram-native SUBQUERY inner (histogram_native_subquery_select.go).
//
// The one genuine gap: [nativeExpHistBareAggsDirectional]'s own argMax/argMin
// set only carries the nine Histogram*Column fields (plus MetricName) — it
// has no slot for the real Value column a float row publishes, nor for
// [chplan.MixedDiscriminatorColumn] itself, both of which
// [lowerSumOrAvgOverMixedExpHistogramSetOp]'s own Mixed-shaped subquery
// inner ALREADY carries on every row. [mixedLastFirstAggs] below widens
// that same argMax/argMin set by exactly those two columns: every field is
// still picked at the SAME selected row (every AggFunc orders by the
// identical TimeUnix column [nativeExpHistBareAggsDirectional] already
// uses), so the result is coherent whichever type wins — a float row's
// nine placeholder histogram columns and a histogram row's placeholder
// Value both survive the selection unchanged, exactly as
// [chplan.VectorSetOp.Mixed]'s own doc describes for every OTHER Mixed-row
// consumer in this tree.
//
// [mixedLastFirstProjection] then caps the aggregate with an explicit
// [chplan.Project] naming [chplan.MixedDiscriminatorColumn] as one of its
// outputs — [chplan.RowShapeOf]'s own *Project case recognises exactly that
// as still Mixed-shaped, the same convention
// histogram_shape_guard.go / label_fns.go's Mixed-preserving Projects
// already rely on — so the final answer stays decodable by
// internal/chclient's shapeSampleMixed exactly like every other Mixed
// result this tree produces.
//
// All three grid modes (instant, `@`-pinned broadcast, true query_range
// fan-out) compose here, precisely because there is no window-purity test
// to rescope per anchor — the same reason resets/changes reaches all three
// where the FOLD family needed its own dedicated fan-out lowering
// ([lowerSumOrAvgMixedOrSubqueryFoldFnRange]).
//
// No new chplan or chsql surface: every node this file builds is a
// [chplan.Aggregate], a [chplan.RangeBucketFanout], a [chplan.CrossJoin], or
// a [chplan.Project], composed the same way this package's other mixed-or
// composers already do.

// lowerMixedOrSubqueryLastFirst lowers the [sumOrAvgMixedOrSubqueryShape]
// shape for windowFn last_over_time/first_over_time across all three grid
// modes. mixedRel, the subquery's own inner aggregation lowered ONCE across
// the whole grid via the exact existing root-only composer
// ([lowerSumOrAvgOverMixedExpHistogramSetOp]), is the same correct
// per-(group, subquery anchor) Mixed relation
// [lowerSumOrAvgMixedOrSubquerySelectFn] folds over for its own four
// type-blind names.
func lowerMixedOrSubqueryLastFirst(shape sumOrAvgMixedOrSubqueryShape, gridCtx lowerCtx, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	mixedRel, err := lowerSumOrAvgOverMixedExpHistogramSetOp(shape.agg, shape.b, s, gridCtx)
	if err != nil {
		return nil, err
	}
	if chplan.RowShapeOf(mixedRel) != chplan.MixedRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: sum/avg-mixed-or subquery input is %T with %s row shape", mixedRel, chplan.RowShapeOf(mixedRel))
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""

	if ctx.rangeMode() && !subqueryPinned(sub) {
		return lowerMixedOrSubqueryLastFirstRange(shape, mixedRel, anchor, histSchema, s, ctx), nil
	}

	windowed := mixedLastFirstWindowed(shape.windowFn, mixedRel, histSchema, s)
	if ctx.rangeMode() {
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return mixedLastFirstProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			histSchema, s,
		), nil
	}
	tsExpr := chplan.NowNano()
	if !anchor.End.IsZero() {
		tsExpr = windowRightBoundExpr(evalAnchor{End: anchor.End})
	}
	return mixedLastFirstProjection(windowed, tsExpr, histSchema, s), nil
}

// lowerMixedOrSubqueryLastFirstRange is the true query_range fan-out mode:
// one `[sub.Range]` window per output step anchor via
// [chplan.RangeBucketFanout] over mixedRel directly — mirrors
// [lowerMixedOrSubqueryResetsRange]'s identical shape (same MinSamples
// floor, same AnchorAlias / TimestampCol wiring), fed [mixedLastFirstAggs]
// instead of [mixedPairCountAggs] so each bucket collapses to the single
// argMax/argMin-selected row instead of a groupArray pair-verdict.
func lowerMixedOrSubqueryLastFirstRange(shape sumOrAvgMixedOrSubqueryShape, mixedRel chplan.Node, anchor evalAnchor, histSchema, s schema.Metrics, ctx lowerCtx) chplan.Node {
	fanout := &chplan.RangeBucketFanout{
		Input:          mixedRel,
		Start:          ctx.start.UTC(),
		End:            ctx.end.UTC(),
		Step:           ctx.step,
		Lookback:       shape.sub.Range,
		Offset:         anchor.Offset,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases: []string{s.AttributesColumn},
		AggFuncs:       mixedLastFirstAggs(shape.windowFn, histSchema),
		MinSamples:     stalenessMinSamples,
		AnchorAlias:    stepGridAnchorColumn,
		TimestampCol:   s.TimestampColumn,
	}
	return mixedLastFirstProjection(fanout, &chplan.ColumnRef{Name: stepGridAnchorColumn}, histSchema, s)
}

// mixedLastFirstWindowed builds the instant-mode (and pinned-broadcast,
// before the CrossJoin) per-series reduction: mixedRel grouped by its own
// published Attributes column, collapsed by [mixedLastFirstAggs] — mirrors
// [selectFnOverSubqueryWindowed]'s last_over_time/first_over_time case, one
// level up in Mixed-row width.
func mixedLastFirstWindowed(windowFn string, mixedRel chplan.Node, histSchema, s schema.Metrics) chplan.Node {
	return &chplan.Aggregate{
		Input:              mixedRel,
		GroupBy:            []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           mixedLastFirstAggs(windowFn, histSchema),
		DropEmptyOnNoGroup: true,
	}
}

// mixedLastFirstAggs is [nativeExpHistBareAggsDirectional] widened by the
// two columns this file's own top-level doc names as the gap it closes: the
// real Value column a float row publishes, and [chplan.MixedDiscriminatorColumn]
// itself — both picked by the SAME argMax/argMin selection (keyed by the
// identical TimeUnix order column every other field already shares), so the
// selected row's own discriminator and value/histogram payload stay
// mutually consistent.
func mixedLastFirstAggs(windowFn string, histSchema schema.Metrics) []chplan.AggFunc {
	pick := latestArgMax
	if windowFn == firstOverTimeWindowFn {
		pick = earliestArgMin
	}
	return append(
		[]chplan.AggFunc{
			pick(histSchema.MetricNameColumn, histSchema),
			pick(histSchema.ValueColumn, histSchema),
			pick(chplan.MixedDiscriminatorColumn, histSchema),
		},
		nativeExpHistValuedLatestAggsDirectional(windowFn, histSchema)...,
	)
}

// mixedLastFirstProjection caps input — the fourteen-column argMax/argMin
// aggregate [mixedLastFirstWindowed] / [lowerMixedOrSubqueryLastFirstRange]
// build — with an explicit [chplan.Project] publishing the Mixed contract:
// the canonical quartet, the nine Histogram*Column fields, and
// [chplan.MixedDiscriminatorColumn] — the Mixed-row equivalent of
// [nativeHistogramProjection], which has no slot for either the real Value
// column or the discriminator.
func mixedLastFirstProjection(input chplan.Node, tsExpr chplan.Expr, histSchema, s schema.Metrics) chplan.Node {
	projs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		{Expr: tsExpr, Alias: s.TimestampColumn},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.CountColumn}, Alias: histSchema.CountColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.SumColumn}, Alias: histSchema.SumColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.ScaleColumn}, Alias: histSchema.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.ZeroCountColumn}, Alias: histSchema.ZeroCountColumn},
	}
	if histSchema.ZeroThresholdColumn != "" {
		projs = append(projs, chplan.Projection{
			Expr: &chplan.ColumnRef{Name: histSchema.ZeroThresholdColumn}, Alias: histSchema.ZeroThresholdColumn,
		})
	}
	projs = append(
		projs,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.PositiveOffsetColumn}, Alias: histSchema.PositiveOffsetColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.PositiveBucketCountsColumn}, Alias: histSchema.PositiveBucketCountsColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.NegativeOffsetColumn}, Alias: histSchema.NegativeOffsetColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.NegativeBucketCountsColumn}, Alias: histSchema.NegativeBucketCountsColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn}, Alias: chplan.MixedDiscriminatorColumn},
	)
	return &chplan.Project{Input: input, Projections: projs}
}
