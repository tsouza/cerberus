package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	// hqMergeCountsArrayAlias and hqMergeSumsArrayAlias name the groupArray
	// columns that collect the Count/Sum scalar fields across matched rows,
	// shared by every merge in this package. Most callers (this file,
	// histogram_quantile_native_window.go) fold that array through the same
	// compensated across-series fold as every bucket — [promHistogramKahanSum]
	// — but histogram_native_binop.go's two-operand merge deliberately folds
	// it with the PLAIN [plainArraySum] instead, matching reference
	// Prometheus's uncompensated FloatHistogram.Add/Sub (see that file's
	// header doc for why). The aliases only name the collected array; they
	// carry no compensation guarantee of their own.
	hqMergeCountsArrayAlias = "_hq_merge_counts"
	hqMergeSumsArrayAlias   = "_hq_merge_sums"
)

// histogram_native_sum.go lowers `sum [by/without] (<selector>)` over an
// OTel exponential (native) histogram — `<base>_exp_hist` — into a
// histogram-VALUED result, the second lowering (after the bare selector
// in histogram_native_bare.go) to cap a plan with a
// [chplan.HistogramProjection].
//
// It also hosts the MERGE that `avg()` reuses: histogram_native_avg.go
// adds one division on top of the row this file builds, and shares this
// file's recognizer. Everything below about the merge applies to both.
//
// Reference Prometheus answers `sum()` over native-histogram samples
// with a native histogram, not a float: promql's aggregation walks the
// group calling `histogram.FloatHistogram.Add` on each member, so the
// result is the element-wise ADDITION of the members' distributions.
// That is a genuinely different operation from the float path's
// `sum(Value)` — the operands are bucket LADDERS whose absolute bucket
// indices only line up after their scales and offsets are reconciled.
//
// Reusing the reconciliation rather than re-deriving it is the whole
// point of this file's shape. The across-series merge cerberus already
// runs for `histogram_quantile(phi, sum(rate(...)))` —
// [expHistogramMergeAggs] + [expHistogramMergeProjections], documented
// against Prometheus's FloatHistogram.Add in
// lowerHistogramQuantileNativeAgg — IS histogram addition; the quantile
// path just happens to consume its output with an interpolation kernel
// instead of publishing it. `sum()` publishes it, so the two paths share
// one merge and cannot drift apart in their arithmetic.
//
// What this file adds on top of that shared merge is the pair of
// whole-histogram scalars the quantile kernel never reads — Count and
// Sum — which add plainly across the group, and the
// [chplan.HistogramProjection] cap.
//
// Plan shape (instant mode; the range shapes differ only in the anchor
// column threaded through both reductions):
//
//	HistogramProjection [MetricName='', Attributes, now64(9), Value=0, <nine histogram columns>]
//	  Project [Attributes (rebuilt from gkeys), merged Scale / ZeroCount /
//	           ZeroThreshold / {Pos,Neg}{Offset,BucketCounts}, Count, Sum]
//	    Aggregate groupBy=[<user by/without labels>] funcs=<expHistogramGroupMergeAggs>
//	      Aggregate groupBy=[series identity] funcs=<nativeExpHistValuedLatestAggs>  ← per series
//	        Filter <matchers> AND <staleness window>
//	          Scan(otel_metrics_exponential_histogram)
//
// The two stages are the same split every native-histogram aggregation
// in this package uses, and for the same reason: the inner one resolves
// each SERIES to a single sample (a selector under an aggregation is
// still subject to staleness lookback — one sample per series, the
// newest in the window), and only then does the outer one add
// distributions ACROSS series. Collapsing them into one grouping would
// add a series' own consecutive scrapes to each other.

// sumOrAvgOverExpHistogram reports whether expr is `sum [by/without]
// (<bare exp-histogram selector>)` or its `avg` twin — the two shapes
// this file answers.
//
// The two share one recognizer because they share one merge: `avg()` is
// this file's merge divided by the group's series count, and
// histogram_native_avg.go adds only that division. Recognising them
// apart would let the shapes they accept drift.
//
// Like [bareExpHistogramSelector] it is asked only of the ROOT of a
// query (see [lowerRoot]), and for the same reason: the answer is a
// histogram row, which only the wire can consume. `sum(m_exp_hist) + 1`
// or `topk(3, sum(m_exp_hist))` reaches lowerVectorSelector through the
// ordinary descent and is still rejected there.
//
// The inner expression must unwrap to a bare selector. `sum(rate(
// m_exp_hist[5m]))` does not: its aggregand is a range-vector function, and
// admitting it here would silently drop the rate. That composed shape is
// answered by a different recognizer, not this one — mergeableExpHistogramAggregate
// (also in this file) matches a `sum`/`avg` wrapping ANY already
// histogram-valued shape, [lowerExpHistogramValuedShape] recurses into the
// aggregand to lower the `rate()` first, and
// [lowerExpHistogramSumOrAvgOverPlan] then reuses this function's merge
// machinery over that already-histogram-valued result (issue #2296, closed
// by #2245). Keeping this recognizer narrow to the bare-selector fast path
// keeps its byte-stable direct lowering unchanged by that composition.
func sumOrAvgOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.VectorSelector, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false
	}
	agg, ok := unwrapAggregateExpr(expr)
	// Param is nil for every SUM and AVG the parser produces (only the
	// topk / bottomk / quantile / count_values family carries one); the
	// check states the assumption rather than relying on it.
	if !ok || !expHistogramAggOpIsMergeable(agg.Op) || agg.Param != nil {
		return nil, nil, false
	}
	vs, ok := unwrapVectorSelector(agg.Expr)
	if !ok {
		return nil, nil, false
	}
	if !s.IsExpHistogramMetric(metricNameFromMatchers(vs.LabelMatchers)) {
		return nil, nil, false
	}
	return agg, vs, true
}

// mergeableExpHistogramAggregate recognizes the SUM/AVG consumer itself,
// independently of the expression that produces its input samples. The
// direct-selector fast path above keeps its byte-stable lowering; this
// consumer shape is what lets the same histogram merge stack over a window
// result, another aggregation, or a label-only wrapper.
func mergeableExpHistogramAggregate(expr parser.Expr) (*parser.AggregateExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || !expHistogramAggOpIsMergeable(agg.Op) || agg.Param != nil {
		return nil, false
	}
	return agg, true
}

// unwrapAggregateExpr peels the transparent wrappers the parser can put
// between the root and an aggregation — `(sum(m))` and the
// step-invariance marker — and reports the aggregation underneath.
// Mirrors [unwrapVectorSelector], which does the same for a selector.
func unwrapAggregateExpr(e parser.Expr) (*parser.AggregateExpr, bool) {
	for {
		switch v := e.(type) {
		case *parser.AggregateExpr:
			return v, true
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return nil, false
		}
	}
}

// lowerExpHistogramSumOrAvg lowers the `sum()` / `avg()` shape across the three
// evaluation shapes [rangeGridShapeFor] distinguishes, the same three
// the bare sibling handles — see [lowerExpHistogramBare] for what each
// one means.
func lowerExpHistogramSumOrAvg(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if shape := rangeGridShapeFor(vs, ctx); shape == gridFanout {
		return lowerExpHistogramSumOrAvgRange(agg, vs, s, ctx), nil
	} else if shape == gridBroadcast {
		merged, err := expHistogramGroupMergedInstant(agg, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast placement as the bare path's: the cross join goes
		// UNDER the histogram projection so the plan root stays a
		// HistogramProjection and keeps publishing the thirteen-column
		// contract. The pinned window is merged ONCE and every step
		// reports that one distribution at its own timestamp.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return aggregatedHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: merged},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	merged, err := expHistogramGroupMergedInstant(agg, vs, s, ctx)
	if err != nil {
		return nil, err
	}
	return aggregatedHistogramProjection(merged, chplan.NowNano(), s), nil
}

// expHistogramGroupMergedInstant builds the instant-mode subtree beneath
// the histogram projection: the filtered scan collapsed to the newest
// in-window sample per series, then merged across series by the user's
// grouping. Its output is one row per output series carrying the merged
// distribution under the schema's own column names.
func expHistogramGroupMergedInstant(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred, err := andInstantWindow(pred, vs, s.TimestampColumn, ctx)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	perSeries := latestSampleAgg(input, nativeExpHistValuedLatestAggs(s), s)
	return expHistogramGroupMerge(perSeries, nil, agg, s), nil
}

// lowerExpHistogramSumOrAvgRange is the query_range shape. Stage 1 is the
// per-anchor [chplan.RangeBucketFanout] the bare range path builds — one
// staleness window per step anchor, keyed on SERIES identity — and stage
// 2 is the across-series merge within each anchor, exactly as
// buildHistogramNativeRangeTreeMerge stacks the two for the quantile.
//
// Keying stage 1 on series identity rather than on the user's
// `by/without` labels is what makes the per-anchor collapse mean what
// reference PromQL means: `sum by(service)` over three pods must take
// each pod's own newest sample and add the three, not take one newest
// sample per service.
func lowerExpHistogramSumOrAvgRange(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	perSeries := buildHistogramBucketFanout(
		scan, pred, nil, windowFor(vs, instantLookback),
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		nativeExpHistValuedLatestAggs(s), s, ctx,
	)
	anchor := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	return aggregatedHistogramProjection(expHistogramGroupMerge(perSeries, anchor, agg, s), anchor, s)
}

// expHistogramGroupMerge is the across-SERIES stage: it adds the per-series
// distributions perSeries hands up, grouped by the user's `by/without`
// clause, and reshapes the result back into the exp-histogram row
// contract under the schema's own column names.
//
// anchor is nil in instant mode and the fan-out's anchor column in range
// mode, where it prepends both the grouping key (each step anchor merges
// independently) and the reshape projection (the projection above reads
// it back for the output timestamp).
//
// The grouping keys bind from the per-series stage's already-canonical
// Attributes column rather than from the raw table, which is no longer
// in scope above stage 1 — the same binding [histogramAggGroupBy]'s doc
// describes for the quantile paths, and the reason they must not be
// re-wrapped in [canonicalGroupKeyExpr].
func expHistogramGroupMerge(perSeries chplan.Node, anchor *chplan.ColumnRef, agg *parser.AggregateExpr, s schema.Metrics) chplan.Node {
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s,
	)
	var projs []chplan.Projection
	if anchor != nil {
		groupBy = append([]chplan.Expr{anchor}, groupBy...)
		groupByAliases = append([]string{stepGridAnchorColumn}, groupByAliases...)
		projs = append(projs, chplan.Projection{Expr: anchor, Alias: stepGridAnchorColumn})
	}
	merged := &chplan.Aggregate{
		Input:              perSeries,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           expHistogramGroupMergeAggs(agg, s),
		DropEmptyOnNoGroup: true,
	}
	projs = append(projs, chplan.Projection{Expr: attrsRebuild, Alias: s.AttributesColumn})
	fields := expHistogramGroupMergeProjections(s)
	if expHistogramGroupIsAvg(agg) {
		// The ONE place the two aggregations differ. Everything below this
		// line — the scale fold, the offset alignment, the zero padding —
		// is identical for `sum` and `avg`, which is why the division
		// wraps the merged row here rather than reaching into the shared
		// merge. See histogram_native_avg.go for which fields it touches
		// and why the other four must not be touched.
		fields = expHistogramAvgScaleProjections(fields, s)
	}
	projs = append(projs, fields...)
	return &chplan.Project{Input: merged, Projections: projs}
}

// lowerExpHistogramSumOrAvgOverPlan applies a cross-series SUM/AVG to an
// already histogram-valued result. The input's HistogramProjection is a
// stable thirteen-column boundary, so this path deliberately consumes those
// public aliases instead of reaching through to a producer-specific subtree.
// Grouping by the published timestamp keeps every query_range anchor
// independent while remaining a single key for an instant evaluation.
func lowerExpHistogramSumOrAvgOverPlan(agg *parser.AggregateExpr, input chplan.Node, s schema.Metrics) (chplan.Node, error) {
	if chplan.RowShapeOf(input) != chplan.HistogramRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: nested native-histogram aggregation input is %T with %s row shape", input, chplan.RowShapeOf(input))
	}

	histSchema := histogramProjectionSchema(s)
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s,
	)
	groupBy = append([]chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, groupBy...)
	groupByAliases = append([]string{s.TimestampColumn}, groupByAliases...)
	merged := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           expHistogramGroupMergeAggs(agg, histSchema),
		DropEmptyOnNoGroup: true,
	}

	fields := expHistogramGroupMergeProjections(histSchema)
	if expHistogramGroupIsAvg(agg) {
		fields = expHistogramAvgScaleProjections(fields, histSchema)
	}
	reshaped := &chplan.Project{
		Input: merged,
		Projections: append([]chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
		}, fields...),
	}
	return nativeHistogramProjection(
		reshaped,
		&chplan.LitString{V: ""},
		&chplan.ColumnRef{Name: s.TimestampColumn},
		histSchema,
	), nil
}

// histogramProjectionSchema names the physical histogram fields exposed by a
// HistogramProjection. It leaves the canonical sample quartet unchanged and
// redirects only the nine payload columns to their fixed output aliases.
func histogramProjectionSchema(s schema.Metrics) schema.Metrics {
	s.CountColumn = chplan.HistogramCountColumn
	s.SumColumn = chplan.HistogramSumColumn
	s.ScaleColumn = chplan.HistogramScaleColumn
	s.ZeroThresholdColumn = chplan.HistogramZeroThresholdColumn
	s.ZeroCountColumn = chplan.HistogramZeroCountColumn
	s.PositiveOffsetColumn = chplan.HistogramPositiveOffsetColumn
	s.PositiveBucketCountsColumn = chplan.HistogramPositiveBucketCountsColumn
	s.NegativeOffsetColumn = chplan.HistogramNegativeOffsetColumn
	s.NegativeBucketCountsColumn = chplan.HistogramNegativeBucketCountsColumn
	return s
}

// expHistogramGroupMergeAggs is [expHistogramMergeAggs] — the shared
// scale-fold + offset-align + zero-pad collection, the arithmetic of
// FloatHistogram.Add — widened by the two whole-histogram scalars a
// histogram-VALUED answer must also publish.
//
// Count and Sum are collected into arrays and folded with the same
// compensated recurrence as the buckets: they are the group's total
// observation count and total observed value, and neither depends on bucket
// alignment, but Prometheus's FloatHistogram.KahanAdd makes their rounding
// behavior part of parity.
// `avg()` additionally collects the group's member count, the divisor it
// scales the merged distribution by (see
// [expHistogramGroupSeriesCountAgg]). `sum()` does NOT collect it: it
// has no use for the column, and adding one to a shipped lowering's
// emitted SQL would cost a counter per group and churn its goldens for
// nothing.
func expHistogramGroupMergeAggs(agg *parser.AggregateExpr, s schema.Metrics) []chplan.AggFunc {
	aggs := []chplan.AggFunc{
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.CountColumn}}, Alias: hqMergeCountsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}}, Alias: hqMergeSumsArrayAlias},
	}
	if expHistogramGroupIsAvg(agg) {
		aggs = append(aggs, expHistogramGroupSeriesCountAgg())
	}
	return append(aggs, expHistogramMergeAggs(s)...)
}

// expHistogramGroupMergeProjections is [expHistogramMergeProjections] plus
// the Count / Sum pass-through, so the reshaped row carries all nine
// fields [chplan.HistogramProjection] reads by name rather than the
// seven the quantile kernel needs.
func expHistogramGroupMergeProjections(s schema.Metrics) []chplan.Projection {
	return append(
		[]chplan.Projection{
			{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeCountsArrayAlias}), Alias: s.CountColumn},
			{Expr: promHistogramKahanSum(&chplan.ColumnRef{Name: hqMergeSumsArrayAlias}), Alias: s.SumColumn},
		},
		expHistogramMergeProjections(s)...,
	)
}

// aggregatedHistogramProjection caps a merged subtree with the
// thirteen-column [chplan.HistogramProjection], reporting an EMPTY
// `__name__`.
//
// The empty name is PromQL's rule, not a shortcut: an aggregation
// produces a derived sample, and reference Prometheus drops `__name__`
// from every aggregation result. It is also why the merge stage never
// collects MetricName — there is no per-group answer to collect, since
// one group can span several stored spellings of the metric name.
func aggregatedHistogramProjection(input chplan.Node, tsExpr chplan.Expr, s schema.Metrics) *chplan.HistogramProjection {
	return nativeHistogramProjection(input, &chplan.LitString{V: ""}, tsExpr, s)
}
