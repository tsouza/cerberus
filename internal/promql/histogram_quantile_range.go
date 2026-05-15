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
// mode path: matrix-anchor handling for histogram_quantile under
// modifiers is a follow-up. In practice the modifier-bearing shapes are
// rare (Grafana never emits them on query_range), so the instant-mode
// fallback is byte-stable for the existing fixtures and the range-mode
// rewrite covers the wire-shape Grafana actually drives.

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
	pred := buildPredicate(vs.LabelMatchers, s)

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
	pred := buildPredicate(vs.LabelMatchers, s)

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
