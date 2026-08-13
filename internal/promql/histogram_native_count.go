package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_count.go lowers `count [by/without] (<selector>)` over
// an OTel exponential (native) histogram — `<base>_exp_hist` — into a
// FLOAT-valued result.
//
// `count()` is the one aggregation that reduces native-histogram samples
// without merging them, and that is why it needs a file rather than a
// place on [expHistogramAggOpIsMergeable]'s list. That predicate answers
// "does this operator have a merged-DISTRIBUTION lowering", and admitting
// `count` to it would be a category error twice over: it would route the
// shape into histogram_native_sum.go's bucket-ladder merge, which would
// publish a distribution where reference publishes a scalar, and it would
// also flip [histogramAggShapeLowerable] for every classic-histogram path
// that consults the same predicate.
//
// Reference's aggregation switch is explicit that `count` never looks at
// the sample's value:
//
//	case parser.COUNT:
//	    group.groupCount++
//
// There is no `if h != nil` guard around it and no annotation raised —
// unlike `min` / `max` / `stddev` / `stdvar` / `quantile`, which DROP a
// histogram sample and warn (HistogramIgnoredInAggregation), and which is
// exactly why those five stay rejected while this one does not. A
// histogram sample is a sample; `count()` counts it.
//
// # What is being counted
//
// The instant vector, not the stored rows. A series with forty samples in
// the staleness lookback contributes ONE to the count, because reference
// evaluates the selector to at most one sample per series first. That
// makes the collapse to the newest sample per series load-bearing rather
// than an optimisation — counting the filtered scan directly would report
// the scrape count, which is a different number that happens to agree
// only when every series was scraped exactly once in the window.
//
// The two stages are therefore the same two `sum()` uses, keyed the same
// way and for the reason [lowerExpHistogramSumOrAvgRange]'s doc gives:
// stage 1 groups on SERIES identity so each series collapses on its own,
// and stage 2 groups on the user's `by/without` clause. `count by(service)`
// over three pods is 3, not 1 — which is what falls out of collapsing per
// pod and then counting per service, and what would NOT fall out of
// collapsing per service.
//
// Plan shape (instant mode):
//
//	Project [MetricName='', <regrouped Attributes>, now64(9), Value]
//	  Aggregate groupBy=[<by/without key>] funcs=[count() AS Value]
//	    Aggregate groupBy=[series identity] funcs=[argMax(TimeUnix, TimeUnix)]
//	      Filter <matchers> AND <staleness window>
//	        Scan(otel_metrics_exponential_histogram)

// countOverExpHistogram reports whether expr is `count [by/without]
// (<bare exp-histogram selector>)` — the shape this file answers —
// returning the aggregation and the selector under it.
//
// It mirrors [sumOrAvgOverExpHistogram] rung for rung, differing only in
// which operator it admits. `count_values` carries a Param and is a
// distinct ItemType, so the nil-Param check states the assumption rather
// than relying on the parser never producing one.
func countOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.VectorSelector, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false
	}
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || agg.Op != parser.COUNT || agg.Param != nil {
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

// lowerExpHistogramCount lowers the `count()` shape across the three
// evaluation shapes [rangeGridShapeFor] distinguishes, the same three
// its siblings handle — see [lowerExpHistogramBare] for what each means.
func lowerExpHistogramCount(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if shape := rangeGridShapeFor(vs, ctx); shape == gridFanout {
		return lowerExpHistogramCountRange(agg, vs, s, ctx), nil
	} else if shape == gridBroadcast {
		counted, err := expHistogramCountInstant(agg, vs, s, ctx)
		if err != nil {
			return nil, err
		}
		// Same broadcast placement as every sibling path's: the cross join
		// goes UNDER the sample projection, so the pinned window is counted
		// ONCE and every step reports that one count at its own timestamp.
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return expHistogramCountProjection(
			&chplan.CrossJoin{Left: grid, Right: counted},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	counted, err := expHistogramCountInstant(agg, vs, s, ctx)
	if err != nil {
		return nil, err
	}
	return expHistogramCountProjection(counted, chplan.NowNano(), s), nil
}

// expHistogramCountInstant builds the instant-mode subtree beneath the
// sample projection: the filtered scan collapsed to the newest in-window
// sample per series, then counted across series by the user's grouping.
func expHistogramCountInstant(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
	perSeries := latestSampleAgg(input, expHistogramCountLatestAggs(s), s)
	return expHistogramGroupCount(perSeries, nil, agg, s), nil
}

// lowerExpHistogramCountRange is the query_range shape: stage 1 is the
// per-anchor [chplan.RangeBucketFanout] the sibling range paths build —
// one staleness window per step anchor, keyed on SERIES identity — and
// stage 2 is the across-series count within each anchor.
func lowerExpHistogramCountRange(agg *parser.AggregateExpr, vs *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) chplan.Node {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	perSeries := buildHistogramBucketFanout(
		scan, pred, nil, windowFor(vs, instantLookback),
		[]chplan.Expr{histogramIdentityExpr(s)}, []string{s.AttributesColumn},
		expHistogramCountLatestAggs(s), s, ctx,
	)
	anchor := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	return expHistogramCountProjection(expHistogramGroupCount(perSeries, anchor, agg, s), anchor, s)
}

// expHistogramCountLatestAggs is the per-series collapse's aggregate set,
// which for `count()` is as small as a valid GROUP BY allows: the newest
// sample's timestamp and nothing else.
//
// Every other exp-histogram path collects the distribution columns here
// because something above it reads them. `count()` reads none of them —
// it needs the collapse to exist, not to carry anything — so collecting
// the bucket ladders would put eight arrays into the emitted SELECT for a
// query whose answer is one integer per group.
func expHistogramCountLatestAggs(s schema.Metrics) []chplan.AggFunc {
	return []chplan.AggFunc{latestArgMax(s.TimestampColumn, s)}
}

// expHistogramGroupCount is the across-SERIES stage: it counts the rows
// perSeries hands up, grouped by the user's `by/without` clause, and
// rebuilds the output Attributes from that grouping.
//
// anchor is nil in instant mode and the fan-out's anchor column in range
// mode, where it prepends both the grouping key (each step anchor counts
// independently) and the projection above it.
//
// The grouping keys bind from the per-series stage's already-canonical
// Attributes column rather than from the raw table, which is no longer in
// scope above stage 1 — the same binding [histogramAggGroupBy]'s doc
// describes, and the reason they must not be re-wrapped in
// [canonicalGroupKeyExpr].
func expHistogramGroupCount(perSeries chplan.Node, anchor *chplan.ColumnRef, agg *parser.AggregateExpr, s schema.Metrics) chplan.Node {
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s,
	)
	var projs []chplan.Projection
	if anchor != nil {
		groupBy = append([]chplan.Expr{anchor}, groupBy...)
		groupByAliases = append([]string{stepGridAnchorColumn}, groupByAliases...)
		projs = append(projs, chplan.Projection{Expr: anchor, Alias: stepGridAnchorColumn})
	}
	counted := &chplan.Aggregate{
		Input:          perSeries,
		GroupBy:        groupBy,
		GroupByAliases: groupByAliases,
		AggFuncs: []chplan.AggFunc{{
			Name:  "count",
			Alias: s.ValueColumn,
		}},
		DropEmptyOnNoGroup: true,
	}
	projs = append(
		projs,
		chplan.Projection{Expr: attrsRebuild, Alias: s.AttributesColumn},
		// No cast here: the emitter renders a `count` aggregate as
		// `toFloat64(count(...))` precisely so the published column is a
		// Float64 rather than the UInt64 ClickHouse would otherwise infer
		// — prod CH is strict about that landing in a Float64 scan
		// destination where chDB would coerce it. Casting again would only
		// wrap a second toFloat64 around an already-float column.
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
	)
	return &chplan.Project{Input: counted, Projections: projs}
}

// expHistogramCountProjection caps the counted subtree with the ordinary
// four-column sample quartet, reporting an EMPTY `__name__` — PromQL's
// rule for every aggregation result, the same one
// [aggregatedHistogramProjection] applies on the histogram-valued side.
func expHistogramCountProjection(input chplan.Node, tsExpr chplan.Expr, s schema.Metrics) chplan.Node {
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: tsExpr, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}
