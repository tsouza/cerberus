package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// metricsValueAlias is the column alias the lowered Aggregate gives the
// metric output. Matches the alias used by `| count()`, `| sum(...)` &c.
// in aggregate.go so downstream code (the upcoming
// /api/metrics/query_range handler) can refer to it uniformly.
const metricsValueAlias = "Value"

// lowerMetricsPipeline lowers `RootExpr.MetricsPipeline` — i.e. the
// `rate()` / `count_over_time()` / `*_over_time(attr)` / `quantile_over_time`
// metrics aggregators — into a chplan `Aggregate(<spanset-tree>)`
// subtree.
//
// The TraceQL grammar does NOT carry the time range in the AST; the
// query range comes from the HTTP `/api/metrics/query_range` request
// (start/end/step). The handler PR wraps the returned Aggregate with a
// `chplan.RangeWindow` carrying that range; this lowering is purely the
// inner reducer + spanset selection tree.
//
// Shape:
//
//	Aggregate{ AggFuncs: [<chFunc>(<arg>) AS Value],
//	           GroupBy: <by(...) attributes>,
//	           Input: <spanset Scan/Filter tree> }
//
// `prev` is the lowered spanset prefix — typically a `Scan(otel_traces)`
// or `Filter(<predicate>, Scan)` produced by lowerPipelineElement /
// lowerSpansetFilter — supplied by Lower() so the spanset matchers from
// the query's `{ ... }` selector survive into the SQL.
func lowerMetricsPipeline(prev chplan.Node, mp traceql.FirstStageElement, s schema.Traces) (chplan.Node, error) {
	if v, ok := mp.(*traceql.MetricsAggregate); ok {
		return lowerMetricsAggregate(prev, v, s)
	}
	// Tempo parses `| avg_over_time(attr)` into an unexported
	// `*averageOverTimeAggregator` rather than a `*MetricsAggregate`
	// (see pkg/traceql/engine_metrics_average.go). The fork hasn't
	// exposed accessors on that type yet; rather than reach for
	// reflect/unsafe (forbidden post-#148), surface a clean
	// not-yet-supported error here. A follow-up fork patch can add
	// `Attribute()` / `GroupBy()` accessors and a small adapter; the
	// CH aggregate it would lower to (`avg`) is already wired in
	// mapMetricsAggregateOp below.
	return nil, fmt.Errorf("traceql: metrics pipeline element %T is not yet supported", mp)
}

// lowerMetricsAggregate maps a single `MetricsAggregate` (rate / sum /
// avg / min / max / count / quantile over_time) onto a
// chplan.MetricsAggregate.
//
// The IR carries the source TraceQL op symbolically (chplan.MetricsOp)
// rather than collapsing to a CH aggregate name at lowering time. This
// preserves the rate vs count_over_time distinction the wrapping
// chplan.RangeWindow needs for per-bucket normalisation in the
// `/api/metrics/query_range` matrix path; bare emission (no
// RangeWindow wrapper) still produces SQL byte-identical to the prior
// chplan.Aggregate shape via internal/chsql/range_window.go's
// emitMetricsAggregate.
//
// `quantile_over_time(attr, q1, q2, ...)` is supported only for a
// single quantile (the common Grafana case). Multi-quantile queries
// surface a clean "not yet supported" error so the caller can decide
// whether to split into N queries.
//
// `histogram_over_time(attr)` lowers to a dedicated
// chplan.MetricsHistogramOverTime node — the per-bucket value is a
// distribution (one row per (group-by, bucket) tuple) rather than a
// scalar, so it warrants a distinct IR shape from the scalar
// MetricsAggregate. Bucketing mirrors Tempo's bucketizeFnFor: each
// span's <attr> is rounded up to the nearest power of two
// (log2(ceil(v))); durations additionally divide by 1e9 so the bucket
// label reads in seconds.
func lowerMetricsAggregate(prev chplan.Node, agg *traceql.MetricsAggregate, s schema.Traces) (chplan.Node, error) {
	op := agg.Op()
	if op == traceql.MetricsAggregateHistogramOverTime {
		return lowerMetricsHistogramOverTime(prev, agg, s)
	}
	cop, err := mapMetricsAggregateOp(op)
	if err != nil {
		return nil, err
	}

	attr, err := metricsAggregateAttr(op, agg.Attribute(), s)
	if err != nil {
		return nil, err
	}

	groupBy, groupAliases, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	var quantiles []float64
	if op == traceql.MetricsAggregateQuantileOverTime {
		qs := agg.Quantiles()
		if len(qs) == 0 {
			return nil, fmt.Errorf("traceql: quantile_over_time requires at least one quantile")
		}
		if len(qs) > 1 {
			return nil, fmt.Errorf("traceql: multi-quantile quantile_over_time is not yet supported (got %d quantiles)", len(qs))
		}
		quantiles = []float64{qs[0]}
	}

	return &chplan.MetricsAggregate{
		Op:             cop,
		Attr:           attr,
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
		Quantiles:      quantiles,
		ValueAlias:     metricsValueAlias,
		Inner:          prev,
	}, nil
}

// mapMetricsAggregateOp turns a TraceQL MetricsAggregateOp into the
// chplan.MetricsOp enum the IR carries. The previous version returned
// a CH aggregate-function name directly — that mapping now lives in
// internal/chsql/range_window.go's metricsAggregateCH so the emitter
// can also pick the per-bucket reducer for the matrix path.
func mapMetricsAggregateOp(op traceql.MetricsAggregateOp) (chplan.MetricsOp, error) {
	switch op {
	case traceql.MetricsAggregateRate:
		return chplan.MetricsOpRate, nil
	case traceql.MetricsAggregateCountOverTime:
		return chplan.MetricsOpCountOverTime, nil
	case traceql.MetricsAggregateSumOverTime:
		return chplan.MetricsOpSumOverTime, nil
	case traceql.MetricsAggregateAvgOverTime:
		return chplan.MetricsOpAvgOverTime, nil
	case traceql.MetricsAggregateMinOverTime:
		return chplan.MetricsOpMinOverTime, nil
	case traceql.MetricsAggregateMaxOverTime:
		return chplan.MetricsOpMaxOverTime, nil
	case traceql.MetricsAggregateQuantileOverTime:
		return chplan.MetricsOpQuantileOverTime, nil
	case traceql.MetricsAggregateHistogramOverTime:
		// Handled by lowerMetricsHistogramOverTime — never falls through
		// to this switch.
		return chplan.MetricsOpHistogramOverTime, fmt.Errorf("traceql: histogram_over_time must lower via lowerMetricsHistogramOverTime")
	}
	return chplan.MetricsOpInvalid, fmt.Errorf("traceql: metrics aggregate op %s is not yet supported", op)
}

// metricsAggregateAttr picks the chplan.Expr for the metric operand.
//
//   - rate / count_over_time: nil (no operand in the TraceQL source;
//     the emitter renders `count(1)`).
//   - *_over_time(attr) and quantile_over_time(attr, q): the lowered
//     attribute reference.
//
// The "no attribute supplied" sentinel is the zero-value `Attribute{}`
// (same convention Tempo's runtime uses at
// ast_metrics.go: `a.attr != (Attribute{})`).
func metricsAggregateAttr(op traceql.MetricsAggregateOp, attr traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	switch op {
	case traceql.MetricsAggregateRate, traceql.MetricsAggregateCountOverTime:
		return nil, nil
	}
	if attr == (traceql.Attribute{}) {
		return nil, fmt.Errorf("traceql: %s requires an attribute operand", op)
	}
	return lowerAttribute(attr, s), nil
}

// histogramBucketAlias is the SELECT-list alias for the bucket column
// synthesised by histogram_over_time. Mirrors Tempo's internal label
// name `__bucket` (pkg/traceql/ast_metrics.go: `internalLabelBucket`)
// so downstream query_range wrapping code can pick the bucket out of
// the row by a stable name.
const histogramBucketAlias = "__bucket"

// lowerMetricsHistogramOverTime maps `| histogram_over_time(<attr>) [by(...)]`
// to a chplan.MetricsHistogramOverTime node.
//
// Bucketing follows Tempo's `bucketizeFnFor`: duration attrs (the
// `duration` intrinsic, or attributes typed as TypeDuration) emit
// `log2(<attr>) / 1e9` so the bucket reads in seconds; other numeric
// attrs emit the raw `log2(<attr>)`. The runtime drops spans with
// <attr> < 2 (bucketizeDuration / bucketizeAttribute return
// NewStaticNil()); the SQL emitter mirrors that with a WHERE filter on
// the operand.
func lowerMetricsHistogramOverTime(prev chplan.Node, agg *traceql.MetricsAggregate, s schema.Traces) (chplan.Node, error) {
	attr := agg.Attribute()
	if attr == (traceql.Attribute{}) {
		return nil, fmt.Errorf("traceql: histogram_over_time requires an attribute operand")
	}
	attrExpr := lowerAttribute(attr, s)

	groupBy, groupAliases, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	return &chplan.MetricsHistogramOverTime{
		Attr:           attrExpr,
		IsDuration:     attr.Intrinsic == traceql.IntrinsicDuration,
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
		BucketAlias:    histogramBucketAlias,
		ValueAlias:     metricsValueAlias,
		Inner:          prev,
	}, nil
}

// lowerMetricsGroupBy turns a TraceQL `by (<attr>, <attr>, ...)` list
// into the chplan grouping expressions + parallel alias slice.
//
// Each `by` attribute lowers via the same accessor lowerAttribute uses
// for spanset matchers, so resource.* / span.* / intrinsics all resolve
// to the right carrier (column ref for intrinsics, FieldAccess for map
// keys). The alias is the attribute's TraceQL name — that lets the
// /api/metrics/query_range handler decode the SELECT row back into a
// labelled series without re-parsing the source.
//
// Returns (nil, nil, nil) when the `by(...)` list is empty — that
// matches chplan.Aggregate's convention for "no grouping".
func lowerMetricsGroupBy(attrs []traceql.Attribute, s schema.Traces) ([]chplan.Expr, []string, error) {
	if len(attrs) == 0 {
		return nil, nil, nil
	}
	exprs := make([]chplan.Expr, 0, len(attrs))
	aliases := make([]string, 0, len(attrs))
	for _, a := range attrs {
		if a == (traceql.Attribute{}) {
			return nil, nil, fmt.Errorf("traceql: empty by(...) group key")
		}
		exprs = append(exprs, lowerAttribute(a, s))
		aliases = append(aliases, a.Name)
	}
	return exprs, aliases, nil
}
