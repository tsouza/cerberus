package promql

import (
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// stripBucketSuffix returns a copy of matchers where any
// `__name__=<X>_bucket` matcher has the `_bucket` suffix removed.
//
// The Prometheus classic-histogram convention exposes one time series per
// bucket under the name `<metric>_bucket` with a `le=<bound>` label
// distinguishing them. OTel-CH stores the same data as one row per
// observation with parallel `BucketCounts` × `ExplicitBounds` arrays
// under the bare metric name (`<metric>`, no suffix), so a query
// like `rate(http_server_request_duration_seconds_bucket[5m])` must
// be translated to a filter against `MetricName='http_server_request_duration_seconds'`
// to find any rows.
//
// Strip is applied at every classic-histogram lowering path (bare or
// aggregated, instant or range). The exponential / native-histogram
// path uses its own metric-name routing (ExpHistogramSuffix) so it
// doesn't share this behaviour.
func stripBucketSuffix(matchers []*labels.Matcher) []*labels.Matcher {
	out := make([]*labels.Matcher, len(matchers))
	for i, m := range matchers {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual && strings.HasSuffix(m.Value, "_bucket") {
			// labels.NewMatcher recompiles a regex when applicable; for
			// MatchEqual that's cheap. Build a fresh matcher rather
			// than mutating the input — the parser may reuse the
			// matcher slice across lowering passes.
			copied, err := labels.NewMatcher(m.Type, m.Name, strings.TrimSuffix(m.Value, "_bucket"))
			if err != nil {
				// Defensive: NewMatcher only errors on regex compile;
				// MatchEqual cannot. Forward the original on the
				// near-impossible failure path so the lowering still
				// produces a valid plan.
				out[i] = m
				continue
			}
			out[i] = copied
			continue
		}
		out[i] = m
	}
	return out
}

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
// The native (exp-histogram) path requires a bare VectorSelector; the
// `rate(...)`-wrapped idiom is not modelled on the native side.
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
// `TimeUnix = now64(9)` (instant eval anchor), `Value` from the
// interpolated quantile.
func lowerHistogramQuantile(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: histogram_quantile expects 2 arguments, got %d", len(c.Args))
	}
	phi, ok := tryScalarLiteral(c.Args[0])
	if !ok {
		return nil, fmt.Errorf("promql: histogram_quantile requires a scalar-literal phi (computed phi is unsupported)")
	}

	// Recognise the canonical Prom idiom — `sum [by/without](rate(...))`
	// — and dispatch to the aggregated-input path. The walker only accepts
	// shapes whose underlying terminal is a bare VectorSelector; anything
	// else falls through to today's bare-selector path (which still emits
	// the existing error message if the shape isn't recognised).
	if shape, ok := matchHistogramAggIdiom(c.Args[1]); ok {
		if s.IsExpHistogramMetric(shape.selector.Name) {
			// Range mode (ctx.step > 0): fan the exp-histogram merge +
			// quantile interpolation across the request's step grid.
			// Modifier-bearing selectors fall back to the instant path
			// until matrix-anchor handling lands (rare in practice —
			// Grafana never threads modifiers through histogram_quantile
			// on query_range).
			if histogramRangeApplies(ctx) && !hasModifier(shape.selector) {
				return lowerHistogramQuantileNativeAggRange(shape, phi, s, ctx), nil
			}
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
		return nil, fmt.Errorf("promql: histogram_quantile second argument must be a histogram VectorSelector")
	}

	if s.IsExpHistogramMetric(vs.Name) {
		// Range mode (ctx.step > 0): build a per-step plan that fans the
		// exponential-histogram quantile interpolation across the
		// request's step grid. Each anchor independently runs the LWR
		// projection over the per-row exp-histogram fields and feeds a
		// merged distribution into HistogramQuantileNative, so the matrix
		// pivot sees one quantile row per (series, anchor) rather than
		// the single instant-mode `now64(9)` row repeated for every
		// step. Modifier-bearing selectors fall back to the instant
		// path until matrix-anchor handling lands.
		if histogramRangeApplies(ctx) && !hasModifier(vs) {
			return lowerHistogramQuantileNativeBareRange(vs, phi, s, ctx), nil
		}
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

	// Target the classic-histogram table directly. OTel-CH classic
	// histograms are one row per series with parallel BucketCounts +
	// ExplicitBounds arrays (no `le` label per row), under the bare
	// metric name. Strip the conventional `_bucket` suffix off the
	// `__name__` matcher so a Grafana query of
	// `rate(<X>_bucket[5m])` filters against `MetricName='<X>'`.
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred := buildPredicate(stripBucketSuffix(vs.LabelMatchers), s)
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
	// the rate's [range] adds the time-bound window. `_bucket` suffix
	// strip mirrors the bare-selector path — see stripBucketSuffix.
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred := buildPredicate(stripBucketSuffix(vs.LabelMatchers), s)

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
		groupBy[i] = attributeLookup(s.AttributesColumn, label)
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
// This is the instant-mode entry point — query_range traffic dispatches
// to lowerHistogramQuantileNativeBareRange (bare selector) or
// lowerHistogramQuantileNativeAggRange (aggregated idiom), which fan
// the same quantile interpolation across a StepGrid + per-anchor
// lookback window. The instant lowering keeps `TimeUnix = now64(9)`
// because instant queries have a single evaluation anchor.
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

// Aliases used by lowerHistogramQuantileNativeAgg to thread per-row
// exp-histogram fields through the Aggregate → Project stack. The
// `_hq_` prefix avoids collision with user-supplied labels (Prom's
// `__name__` is reserved; cerberus's gkey aliases use `gkey_<n>`;
// nothing else writes `_hq_*` columns).
const (
	hqAggMergedScaleAlias     = "_hq_merged_scale"
	hqAggScalesArrayAlias     = "_hq_scales"
	hqAggPosOffsetsArrayAlias = "_hq_pos_offsets"
	hqAggPosBucketsArrayAlias = "_hq_pos_buckets"
	hqAggNegOffsetsArrayAlias = "_hq_neg_offsets"
	hqAggNegBucketsArrayAlias = "_hq_neg_buckets"
)

// lowerHistogramQuantileNativeAgg builds the chplan tree for
// `histogram_quantile(phi, sum [by/without] (rate(<sel>_exp_hist[range])))`
// against the OTel-CH exponential (native) histogram table.
//
// The shape of the produced tree mirrors lowerHistogramQuantileAgg's
// classic-histogram counterpart, but the inner Project does the
// per-row exp-histogram merge (scale-fold + offset-align + zero-pad)
// before HistogramQuantileNative walks the merged distribution:
//
//	Project [Sample-row contract]
//	  HistogramQuantileNative phi=phi, groupBy=[Attributes]
//	    Project [Attributes (rebuilt from gkeys), Scale, ZeroCount, ZeroThreshold,
//	             PositiveOffset, PositiveBucketCounts,
//	             NegativeOffset, NegativeBucketCounts]
//	      Aggregate groupBy=[<user labels>] funcs=[
//	          min(Scale)                       AS _hq_merged_scale,
//	          sum(ZeroCount)                   AS ZeroCount,
//	          max(ZeroThreshold)               AS ZeroThreshold,
//	          groupArray(Scale)                AS _hq_scales,
//	          groupArray(PositiveOffset)       AS _hq_pos_offsets,
//	          groupArray(PositiveBucketCounts) AS _hq_pos_buckets,
//	          groupArray(NegativeOffset)       AS _hq_neg_offsets,
//	          groupArray(NegativeBucketCounts) AS _hq_neg_buckets,
//	      ]
//	        Filter <metric matchers> AND TimeUnix in (anchor-Range, anchor]
//	          Scan(otel_metrics_exp_histogram)
//
// The merge algorithm in the inner Project (see
// expHistogramMergeOffsetExpr + expHistogramMergeBucketsExpr) mirrors
// Prometheus's FloatHistogram.Add semantics:
//
//   - Scale folding: per-row downscale to min(Scale) via the canonical
//     "absolute bucket idx >> (origScale - targetScale)" mapping
//     (model/histogram/float_histogram.go § targetIdx). Uniform-Scale
//     groups (the common case) collapse to identity since delta = 0.
//
//   - Offset alignment: each row's downscaled bucket array contributes
//     to the merged array starting at "PositiveOffset >> delta"
//     (absolute bucket index at merged scale). The merged array spans
//     [arrayMin(downscaled_offset), arrayMax(downscaled_offset+downscaled_length))
//     across rows, zero-padding rows that don't cover the full range.
//
//   - ZeroCount sums trivially; ZeroThreshold takes the max across
//     rows (the merged zero bucket spans the largest individual zero
//     bucket).
//
// The `le` label is silently dropped from any user-supplied `by(...)`
// grouping on the native path too (cerberus's exp histograms never
// carry an `le` label — the bucket distribution lives in the
// PositiveBucketCounts array with PositiveOffset shifting the
// starting absolute index per series). Mirrors classic-agg's behaviour.
func lowerHistogramQuantileNativeAgg(shape histogramAggShape, phi float64, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector

	// Build the Scan + Filter. Same shape as the classic-agg path: the
	// metric-name + label matchers go through buildPredicate; the
	// rate's [range] adds the time-bound window.
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	if shape.windowRange > 0 {
		pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))
	}

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	// Aggregate: collect per-row exp-histogram fields into groupArrays so
	// the wrapping Project can fold them into a single merged distribution.
	// The simple aggregates (min Scale, sum ZeroCount, max ZeroThreshold)
	// land on the same aggregate so the wrapping Project can refer to
	// them by alias.
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(shape.agg, s)
	aggFuncs := []chplan.AggFunc{
		{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
		{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: s.ZeroCountColumn},
		{Name: "max", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}}, Alias: s.ZeroThresholdColumn},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
	}
	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           aggFuncs,
		DropEmptyOnNoGroup: true,
	}

	// Inner Project re-shapes the aggregate output into the exp-histogram
	// row contract HistogramQuantileNative expects: Attributes (rebuilt
	// from gkeys) + the merged Scale / ZeroCount / ZeroThreshold +
	// the folded {Positive,Negative}{Offset,BucketCounts}.
	rebuilt := &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
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
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Final Sample-row wrapping, same as the bare-selector / classic-agg paths.
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

// expHistogramMergeOffsetExpr renders the merged PositiveOffset (or
// NegativeOffset) for a group of native-histogram rows: the minimum of
// per-row downscaled-to-merged-scale offsets.
//
// Emitted CH expression:
//
//	arrayMin(arrayMap((s, off) -> bitShiftRight(off, s - <mergedScale>),
//	                   <scalesArr>, <offArr>))
//
// CH's bitShiftRight on signed Int32 performs arithmetic right shift,
// matching Prometheus's "(idx >> delta)" semantics for negative bucket
// indices (sub-1 latencies). When all rows share Scale the delta is 0
// for every row, so the shift is identity and the merged offset
// reduces to arrayMin(offArr) — identical to classic-histogram
// min-offset semantics.
func expHistogramMergeOffsetExpr(offArrAlias, scalesArrAlias, mergedScaleAlias string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "arrayMin",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{"s", "off"},
						Body: &chplan.FuncCall{
							Name: "bitShiftRight",
							Args: []chplan.Expr{
								&chplan.BareIdent{Name: "off"},
								&chplan.Binary{
									Op:    chplan.OpSub,
									Left:  &chplan.BareIdent{Name: "s"},
									Right: &chplan.ColumnRef{Name: mergedScaleAlias},
								},
							},
						},
					},
					&chplan.ColumnRef{Name: scalesArrAlias},
					&chplan.ColumnRef{Name: offArrAlias},
				},
			},
		},
	}
}

// expHistogramMergeBucketsExpr renders the merged PositiveBucketCounts
// (or NegativeBucketCounts) for a group of native-histogram rows: a
// scale-folded, offset-aligned, zero-padded, element-wise sum.
//
// Algorithm: for each target absolute bucket index `T` in
// [merged_offset, merged_offset + merged_length), the merged value is
//
//	Σ_{row i} arraySum(arrayMap(j ->
//	    if((off_i + j - 1) >> delta_i == T, arr_i[j], 0),
//	    arrayEnumerate(arr_i)))
//
// where delta_i = scales_arr[i] - merged_scale (per-row downscale
// distance), j is 1-based (array position inside row i's bucket
// array), and (off_i + j - 1) is the absolute bucket index of position
// j at row i's original scale.
//
// merged_length is computed as
//
//	max((off_i + length(arr_i) - 1) >> delta_i) -
//	    min(off_i >> delta_i) + 1
//
// across rows. Rows with empty bucket arrays contribute zero to every
// target position (no `j` in arrayEnumerate of empty array).
func expHistogramMergeBucketsExpr(offArrAlias, bucArrAlias, scalesArrAlias, mergedScaleAlias string) chplan.Expr {
	const paramT = "t"

	mergedScale := &chplan.ColumnRef{Name: mergedScaleAlias}
	scalesArr := &chplan.ColumnRef{Name: scalesArrAlias}
	offArr := &chplan.ColumnRef{Name: offArrAlias}
	bucArr := &chplan.ColumnRef{Name: bucArrAlias}

	mergedStart, mergedLength := expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale)
	rowsSum := expHistogramMergeBucketsRowsSumExpr(scalesArr, offArr, bucArr, mergedScale, mergedStart, paramT)

	// Outer: arrayMap(t -> rowsSum, range(toUInt64(mergedLength))).
	// `t` is 0-based; the inner expression reconstructs the absolute
	// target index as mergedStart + t. CH's `range(N)` produces
	// [0, N) over UInt64; toUInt64 keeps the cast explicit so the SQL
	// parses cleanly even when mergedLength is computed from signed
	// values.
	return &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramT},
				Body:   rowsSum,
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

// expHistogramMergeBucketsBoundsExpr builds (mergedStart, mergedLength)
// for the bucket-merge expression. Returned mergedStart is the
// arrayMin of per-row downscaled offsets; mergedLength is
// greatest(0, mergedEnd - mergedStart + 1), clamped so an all-empty
// group produces a zero-length output array.
func expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) (mergedStart, mergedLength chplan.Expr) {
	const (
		paramScalesInner = "sm"
		paramOffInner    = "om"
		paramArrInner    = "am"
	)

	// per-row downscaled start: arrayMap((sm, om) -> bitShiftRight(om, sm - merged_scale), scalesArr, offArr)
	downscaledStarts := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramScalesInner, paramOffInner},
				Body: &chplan.FuncCall{
					Name: "bitShiftRight",
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: paramOffInner},
						&chplan.Binary{
							Op:    chplan.OpSub,
							Left:  &chplan.BareIdent{Name: paramScalesInner},
							Right: mergedScale,
						},
					},
				},
			},
			scalesArr,
			offArr,
		},
	}

	// per-row downscaled end: arrayMap((sm, om, am) -> bitShiftRight(om + length(am) - 1, sm - merged_scale), scalesArr, offArr, bucArr).
	// Rows with empty arrays produce (om + 0 - 1) = om - 1 — slightly below their start, which is fine since they contribute nothing.
	downscaledEnds := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramScalesInner, paramOffInner, paramArrInner},
				Body: &chplan.FuncCall{
					Name: "bitShiftRight",
					Args: []chplan.Expr{
						&chplan.Binary{
							Op:   chplan.OpAdd,
							Left: &chplan.BareIdent{Name: paramOffInner},
							Right: &chplan.Binary{
								Op:    chplan.OpSub,
								Left:  &chplan.FuncCall{Name: "length", Args: []chplan.Expr{&chplan.BareIdent{Name: paramArrInner}}},
								Right: &chplan.LitInt{V: 1},
							},
						},
						&chplan.Binary{
							Op:    chplan.OpSub,
							Left:  &chplan.BareIdent{Name: paramScalesInner},
							Right: mergedScale,
						},
					},
				},
			},
			scalesArr,
			offArr,
			bucArr,
		},
	}

	mergedStart = &chplan.FuncCall{Name: "arrayMin", Args: []chplan.Expr{downscaledStarts}}
	mergedEnd := &chplan.FuncCall{Name: "arrayMax", Args: []chplan.Expr{downscaledEnds}}
	// merged_length = mergedEnd - mergedStart + 1.
	// Guard the "no rows contribute" case by clamping to 0 via greatest(0, …).
	mergedLength = &chplan.FuncCall{
		Name: "greatest",
		Args: []chplan.Expr{
			&chplan.LitInt{V: 0},
			&chplan.Binary{
				Op: chplan.OpAdd,
				Left: &chplan.Binary{
					Op:    chplan.OpSub,
					Left:  mergedEnd,
					Right: mergedStart,
				},
				Right: &chplan.LitInt{V: 1},
			},
		},
	}
	return mergedStart, mergedLength
}

// expHistogramMergeBucketsRowsSumExpr builds the per-target-bucket
// row-sum used inside the outer arrayMap. For target offset `t`
// (0-based; absolute index = mergedStart + t), it sums every row's
// contribution at that bucket: arraySum over rows of the inner
// arraySum-of-arrayMap that picks bucket[j] when (off + j - 1) >>
// (s - merged_scale) == mergedStart + t, else 0.
func expHistogramMergeBucketsRowsSumExpr(scalesArr, offArr, bucArr, mergedScale, mergedStart chplan.Expr, paramT string) chplan.Expr {
	const (
		paramScale = "s"
		paramOff   = "off"
		paramArr   = "arr"
		paramJ     = "j"
	)

	// Inner-most: for one (s, off, arr) tuple and target absolute index T,
	// arraySum(arrayMap(j -> if(bitShiftRight(off + j - 1, s - merged_scale) = T, arr[j], 0), arrayEnumerate(arr))).
	innerContrib := &chplan.FuncCall{
		Name: "arraySum",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramJ},
						Body: &chplan.FuncCall{
							Name: "if",
							Args: []chplan.Expr{
								&chplan.Binary{
									Op: chplan.OpEq,
									Left: &chplan.FuncCall{
										Name: "bitShiftRight",
										Args: []chplan.Expr{
											&chplan.Binary{
												Op:   chplan.OpAdd,
												Left: &chplan.BareIdent{Name: paramOff},
												Right: &chplan.Binary{
													Op:    chplan.OpSub,
													Left:  &chplan.BareIdent{Name: paramJ},
													Right: &chplan.LitInt{V: 1},
												},
											},
											&chplan.Binary{
												Op:    chplan.OpSub,
												Left:  &chplan.BareIdent{Name: paramScale},
												Right: mergedScale,
											},
										},
									},
									// target absolute index = mergedStart + t (t is 0-based).
									Right: &chplan.Binary{
										Op:    chplan.OpAdd,
										Left:  mergedStart,
										Right: &chplan.BareIdent{Name: paramT},
									},
								},
								&chplan.Subscript{
									Container: &chplan.BareIdent{Name: paramArr},
									Key:       &chplan.BareIdent{Name: paramJ},
								},
								&chplan.LitInt{V: 0},
							},
						},
					},
					&chplan.FuncCall{
						Name: "arrayEnumerate",
						Args: []chplan.Expr{&chplan.BareIdent{Name: paramArr}},
					},
				},
			},
		},
	}

	// Sum over rows. arraySum(arrayMap((s, off, arr) -> innerContrib, scalesArr, offArr, bucArr)).
	return &chplan.FuncCall{
		Name: "arraySum",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramScale, paramOff, paramArr},
						Body:   innerContrib,
					},
					scalesArr,
					offArr,
					bucArr,
				},
			},
		},
	}
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
