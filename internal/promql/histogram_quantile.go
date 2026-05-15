package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerHistogramQuantile handles `histogram_quantile(phi, X)`. X is
// either:
//
//   - A bare `*parser.VectorSelector` naming a histogram metric —
//     classic (target table `otel_metrics_histogram`) or exponential /
//     native (target table `otel_metrics_exp_histogram`); OR
//
//   - A composition of `sum [by/without]` aggregations and range-vector
//     functions (`rate`, `increase`) wrapping a bare VectorSelector —
//     i.e. the canonical Prom idiom
//     `histogram_quantile(phi, sum by(le)(rate(metric_bucket[5m])))`.
//     The OTel-CH classic-histogram representation is one row per series
//     with parallel `BucketCounts` × `ExplicitBounds` arrays (no `le`
//     label per row), so the lowering rewrites the inner chain to:
//
//   - Filter the histogram-table Scan to the rate's time window.
//
//   - `sumForEach(BucketCounts)` element-wise across rows in the user's
//     by/without group (the `le` label is implicit in the array
//     position and is dropped from the by-clause silently).
//
//   - `any(ExplicitBounds)` — picking one representative bounds array
//     (every row of the same metric in OTel-CH carries the same bounds).
//
// The native (exp-histogram) path still requires a bare VectorSelector
// for now — the same idiom over native histograms lands in a later
// milestone.
//
// Routing decision: the metric-name suffix configured on
// schema.Metrics.ExpHistogramSuffix (default `"_exp_hist"`) selects
// the native path; everything else falls through to the classic
// path. PromQL itself has no naming convention for exp histograms;
// this is a cerberus-side heuristic, configurable per deployment.
//
// Lowering produces either a chplan.HistogramQuantile (classic) or
// chplan.HistogramQuantileNative (exp). The chsql emitter renders the
// quantile arithmetic in two flavours: linear interpolation across
// ExplicitBounds × BucketCounts for the classic case, log-scale
// midpoint estimation across PositiveBucketCounts for the native case.
//
// The result is wrapped in a Project to match the Sample contract
// downstream — `MetricName=”` (Prom quantile drops __name__),
// `Attributes` reconstructed from the per-row Attributes column,
// `TimeUnix = now64(9)` (instant eval anchor; M2.1 plumbs real eval
// time), `Value` from the interpolated quantile.
func lowerHistogramQuantile(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: histogram_quantile expects 2 arguments, got %d", len(c.Args))
	}
	phi, ok := tryScalarLiteral(c.Args[0])
	if !ok {
		return nil, fmt.Errorf("promql: histogram_quantile requires a scalar-literal phi (computed phi defers to RC3)")
	}

	// Recognise the canonical Prom idiom — `sum [by/without](rate(...))`
	// — and dispatch to the aggregated-input path. The walker only accepts
	// shapes whose underlying terminal is a bare VectorSelector; anything
	// else falls through to today's bare-selector path (which still emits
	// the existing error message if the shape isn't recognised).
	if shape, ok := matchHistogramAggIdiom(c.Args[1]); ok {
		if s.IsExpHistogramMetric(shape.selector.Name) {
			return lowerHistogramQuantileNativeAgg(shape, phi, s, ctx)
		}
		// Range mode (ctx.step > 0): build a per-step plan that fans the
		// bucket aggregation + quantile interpolation across the request's
		// step grid. Modifier-bearing inner selectors fall back to the
		// instant path until matrix-anchor handling for `@`/offset is
		// wired (rare in practice — Grafana never threads modifiers
		// through histogram_quantile on query_range).
		if histogramRangeApplies(ctx) && !hasModifier(shape.selector) {
			return lowerHistogramQuantileClassicAggRange(shape, phi, s, ctx), nil
		}
		return lowerHistogramQuantileAgg(shape, phi, s, ctx)
	}

	vs, ok := unwrapVectorSelector(c.Args[1])
	if !ok {
		return nil, fmt.Errorf("promql: histogram_quantile second argument must be a histogram VectorSelector (aggregated forms land in RC3)")
	}

	if s.IsExpHistogramMetric(vs.Name) {
		return lowerHistogramQuantileNative(vs, phi, s, ctx)
	}

	// Range mode (ctx.step > 0): build a per-step plan that fans the
	// classic-histogram bucket array forward through a StepGrid + LWR
	// window so each step in `[start, end]` emits its own quantile row.
	// Pool-AK flagged the now64(9) hardcode in this lowering as the
	// `histogram_quantile classic-bucket still hardcodes now64(9) in
	// range mode` bug surfaced when finishing the per-step LWR rework
	// (PR #347). Modifier-bearing selectors fall back to the instant
	// path until matrix-anchor handling lands.
	if histogramRangeApplies(ctx) && !hasModifier(vs) {
		return lowerHistogramQuantileClassicBareRange(vs, phi, s, ctx), nil
	}

	// Target the classic-histogram table directly — the metric name is
	// not required to carry a `_bucket` suffix (OTel-CH classic
	// histograms are one row per series with parallel BucketCounts +
	// ExplicitBounds arrays; there is no `le` label).
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	if hasModifier(vs) {
		anchor, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		if pred == nil {
			pred = timeBound
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
		}
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	hq := &chplan.HistogramQuantile{
		Input:                input,
		Phi:                  phi,
		BucketCountsColumn:   s.BucketCountsColumn,
		ExplicitBoundsColumn: s.ExplicitBoundsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Wrap in a Project to match the Sample-row contract downstream
	// (MetricName='', Attributes=<gkey>, TimeUnix=now64(9), Value=value).
	// Mirrors wrapAggregateForSample in lower.go.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// histogramAggShape collects the bits we need to build the
// aggregated-input plan for `histogram_quantile(phi, <agg>(rate(<sel>[range])))`.
//
// `selector` is the underlying bare VectorSelector (carrying the metric
// name + label matchers). `windowRange` is the `[range]` duration from
// the wrapping `rate`/`increase` (zero if there's no range-vector
// function — currently always set when this struct is built, but
// kept explicit for clarity). `agg` carries the AggregateExpr metadata
// (Op, Grouping, Without) when a wrapping aggregation is present; nil
// means "no aggregation wrap, just rate(...)".
type histogramAggShape struct {
	selector    *parser.VectorSelector
	windowRange time.Duration
	agg         *parser.AggregateExpr
}

// matchHistogramAggIdiom walks the expression tree looking for the
// shape `[sum by/without (...) ((paren))*] rate|increase((paren)*
// <VectorSelector>[range])`. Returns the captured shape on a match.
//
// Accepted shapes (after peeling ParenExpr / StepInvariantExpr at each
// level):
//   - rate(metric_bucket[5m])
//   - increase(metric_bucket[5m])
//   - sum by(le)(rate(metric_bucket[5m]))
//   - sum without(...) (rate(metric_bucket[5m]))
//
// Anything else returns ok=false and the caller falls through to the
// bare-selector path. Specifically rejected: non-sum aggregations
// (avg / max / quantile / …), range-vector functions other than rate
// / increase, deeper nestings (e.g. `sum(sum(...))`).
func matchHistogramAggIdiom(e parser.Expr) (histogramAggShape, bool) {
	e = peelWrappers(e)

	// Try an outer aggregation wrapper.
	var agg *parser.AggregateExpr
	if a, ok := e.(*parser.AggregateExpr); ok {
		if a.Op != parser.SUM {
			return histogramAggShape{}, false
		}
		if a.Param != nil {
			// `sum` with a parameter (the parser allows topk/bottomk
			// to surface as AggregateExpr with Param set; defensive).
			return histogramAggShape{}, false
		}
		agg = a
		e = peelWrappers(a.Expr)
	}

	// Inner must be a rate/increase call over a MatrixSelector.
	call, ok := e.(*parser.Call)
	if !ok {
		return histogramAggShape{}, false
	}
	switch call.Func.Name {
	case "rate", "increase":
		// supported range-vector functions on the histogram-array path
	default:
		return histogramAggShape{}, false
	}
	if len(call.Args) != 1 {
		return histogramAggShape{}, false
	}
	ms, ok := peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	vs, ok := peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	return histogramAggShape{
		selector:    vs,
		windowRange: ms.Range,
		agg:         agg,
	}, true
}

// peelWrappers strips ParenExpr / StepInvariantExpr wrappers — the
// parser inserts them for shapes that are otherwise inert.
func peelWrappers(e parser.Expr) parser.Expr {
	for {
		switch v := e.(type) {
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return e
		}
	}
}

// lowerHistogramQuantileAgg builds the chplan tree for
// `histogram_quantile(phi, sum [by/without] (rate(<sel>[range])))`
// against the OTel-CH classic-histogram table.
//
// The shape of the produced tree:
//
//	Project [Sample-row contract]
//	  HistogramQuantile phi=phi, groupBy=[Attributes]
//	    Project [Attributes (rebuilt from gkeys), BucketCounts, ExplicitBounds]
//	      Aggregate groupBy=[<user labels>] funcs=[sumForEach(BucketCounts), any(ExplicitBounds)]
//	        Filter <metric matchers> AND TimeUnix in (anchor-Range, anchor]
//	          Scan(otel_metrics_histogram)
//
// When `agg` is nil (bare `rate(...)` with no surrounding `sum`),
// the Aggregate groups by the full Attributes map (preserving per-series
// identity), and the inner Project re-surfaces Attributes as-is.
//
// The `le` label is silently dropped from any user-supplied `by(...)`
// grouping because OTel-CH classic histograms never carry an `le`
// label — the bucket distribution lives in the parallel arrays. So
// `sum by(le)(...)` collapses to a single group, while
// `sum by(le, job)(...)` groups by `job` alone.
//
// For `sum without (k1, k2, ...)`, the standard MapWithoutKeys group
// expression is used (le is not special; if the user lists it, the
// downstream `mapFilter` simply removes a non-existent key).
func lowerHistogramQuantileAgg(shape histogramAggShape, phi float64, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector

	// Build the Scan + Filter. The metric-name matcher and any
	// user-supplied label matchers go straight through buildPredicate;
	// the rate's [range] adds the time-bound window.
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	// Anchor backfill mirrors selectorAnchor: keep the eval anchor sticky
	// to the surrounding query's end timestamp so the time window stays
	// deterministic across calls (matches what timeBoundExpr does for the
	// bare-selector path under hasModifier).
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	// Upper bound: TimeUnix <= anchor (Prom's right-closed window).
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	// Lower bound: TimeUnix > anchor - Range (Prom's left-open window).
	if shape.windowRange > 0 {
		pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))
	}

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	// Build the Aggregate. GroupBy comes from the surrounding `sum`
	// clause (dropping `le` from by-lists since OTel-CH classic
	// histograms have no per-bucket rows); aggregations are
	// sumForEach(BucketCounts) + any(ExplicitBounds).
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(shape.agg, s)
	aggFuncs := []chplan.AggFunc{
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
	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           aggFuncs,
		DropEmptyOnNoGroup: true,
	}

	// Inner Project re-shapes the aggregate output back into the
	// histogram-row contract HistogramQuantile expects: an Attributes
	// column (rebuilt from the gkey aliases) plus BucketCounts +
	// ExplicitBounds aliased through unchanged.
	rebuilt := &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: attrsRebuild, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
			{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
		},
	}

	hq := &chplan.HistogramQuantile{
		Input:                rebuilt,
		Phi:                  phi,
		BucketCountsColumn:   s.BucketCountsColumn,
		ExplicitBoundsColumn: s.ExplicitBoundsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Final Sample-row wrapping, same as the bare-selector path.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// histogramAggGroupBy translates the user's `sum [by/without]` clause
// into the chplan.Aggregate.GroupBy + GroupByAliases + the
// Attributes-rebuild expression for the wrapping Project.
//
// agg == nil collapses to a single-group aggregation (the user only
// wrote `rate(...)` with no `sum` wrapper) — still useful because the
// rate's time window still applies.
func histogramAggGroupBy(agg *parser.AggregateExpr, s schema.Metrics) ([]chplan.Expr, []string, chplan.Expr) {
	if agg == nil {
		// `histogram_quantile(phi, rate(metric[5m]))` — group by series
		// identity so each series gets its own bucket-rate vector.
		return []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
			[]string{"gkey_0"},
			&chplan.ColumnRef{Name: "gkey_0"}
	}
	if agg.Without {
		// `sum without (...)` — single group key derived from
		// mapFilter on Attributes. `le` doesn't exist in OTel-CH but
		// listing it is harmless (no-op key removal). Empty `without ()`
		// is the degenerate "remove nothing" shape: group by the full
		// Attributes map directly (CH rejects mapFilter with an empty
		// IN list as a syntax error).
		if len(agg.Grouping) == 0 {
			return []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
				[]string{"gkey_0"},
				&chplan.ColumnRef{Name: "gkey_0"}
		}
		return []chplan.Expr{
				&chplan.MapWithoutKeys{
					Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
					Keys: append([]string(nil), agg.Grouping...),
				},
			},
			[]string{"gkey_0"},
			&chplan.ColumnRef{Name: "gkey_0"}
	}
	// `sum by (...)` — drop `le` from the user's list (the bucket
	// distribution lives in the array, not in an Attributes key).
	labels := dropLabel(agg.Grouping, "le")
	if len(labels) == 0 {
		// Either `sum by()` or `sum by(le)` — collapse to a single
		// group and project an empty Attributes map. This is the same
		// path emptyAttrsMap takes in wrapAggregateForSample.
		return nil, nil, emptyAttrsMap()
	}
	groupBy := make([]chplan.Expr, len(labels))
	aliases := make([]string, len(labels))
	mapArgs := make([]chplan.Expr, 0, len(labels)*2)
	for i, label := range labels {
		alias := fmt.Sprintf("gkey_%d", i)
		groupBy[i] = &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: label},
		}
		aliases[i] = alias
		mapArgs = append(
			mapArgs,
			&chplan.LitString{V: label},
			&chplan.ColumnRef{Name: alias},
		)
	}
	attrs := &chplan.FuncCall{Name: "map", Args: mapArgs}
	return groupBy, aliases, attrs
}

// dropLabel returns a copy of labels with every occurrence of `name`
// removed. Used to strip `le` from PromQL `by(...)` lists on the
// histogram-aggregation path (cerberus's classic histograms have no
// per-bucket rows, so `le` cannot be a real grouping key).
func dropLabel(labels []string, name string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == name {
			continue
		}
		out = append(out, l)
	}
	return out
}

// andExpr returns `a AND b`. Either operand may be nil, in which case
// the other is returned unchanged. nil + nil → nil.
func andExpr(a, b chplan.Expr) chplan.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: b}
}

// lowerHistogramQuantileNative builds the chplan.HistogramQuantileNative
// IR for the exp-histogram path. Mirrors the classic-path scaffold:
// Scan or Filter against the exp-histogram table, then wrap in a
// Project to satisfy the Sample-row contract downstream.
//
// Instant-mode only for now — range-mode (per-step anchor grid) is
// the Phase 3 follow-up. See docs/native-histogram-plan.md § Phase 3.
// query_range over a native-histogram metric currently collapses to
// instant-mode behaviour with TimeUnix = now64(9) for every step
// (matches pre-#353 classic-path behaviour); fixing this requires the
// StepGrid + per-anchor lookback rewrite mirroring
// lowerHistogramQuantileClassicBareRange.
func lowerHistogramQuantileNative(vs *parser.VectorSelector, phi float64, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	if hasModifier(vs) {
		anchor, err := anchorFromSelector(vs, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		if pred == nil {
			pred = timeBound
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
		}
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      input,
		Phi:                        phi,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// lowerHistogramQuantileNativeAgg is the Phase 2 placeholder for the
// aggregated-input native (exp) histogram path:
// `histogram_quantile(phi, sum [by/without] (rate(<sel>_exp_hist[r])))`.
//
// The classic-histogram sibling (lowerHistogramQuantileAgg) rewrites
// the inner chain to sumForEach(BucketCounts) + any(ExplicitBounds)
// over a time-bounded Filter. The native equivalent needs to align
// per-series PositiveOffset values before element-wise summing
// PositiveBucketCounts, downscale series whose Scale differs from the
// group minimum, sum ZeroCount, and max-merge ZeroThreshold. None of
// those operations have a single CH primitive, so the lowering needs
// careful design — see docs/native-histogram-plan.md § Phase 2.
//
// Pinned by TestLower_HistogramQuantile_OverAggregation_NativeRejected
// (internal/promql/histogram_quantile_test.go); Phase 2 deletes the
// rejection test and adds the positive-shape mirror tests alongside
// the classic-agg lowering tests.
func lowerHistogramQuantileNativeAgg(_ histogramAggShape, _ float64, _ schema.Metrics, _ lowerCtx) (chplan.Node, error) {
	return nil, fmt.Errorf("promql: histogram_quantile over aggregated native (exp) histograms is not yet supported (see docs/native-histogram-plan.md § Phase 2)")
}

// unwrapVectorSelector peels ParenExpr / StepInvariantExpr wrappers off
// the argument and returns the bare VectorSelector if any. Mirrors what
// tryScalarLiteral does for NumberLiteral, but for the vector arg.
func unwrapVectorSelector(e parser.Expr) (*parser.VectorSelector, bool) {
	for {
		switch v := e.(type) {
		case *parser.VectorSelector:
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
