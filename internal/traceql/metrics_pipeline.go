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
// avg / min / max / count / quantile over_time) onto a chplan.Aggregate.
//
// Per fork-tempo-plan.md § 2c, `rate()` and `count_over_time()` both
// reduce to a row-count at this layer (CH `count(1)`); the
// per-evaluation-step rate normalisation lives on the wrapping
// `chplan.RangeWindow` the handler attaches, not here.
//
// `quantile_over_time(attr, q1, q2, ...)` is supported only for a
// single quantile (the common Grafana case). Multi-quantile queries
// surface a clean "not yet supported" error so the caller can decide
// whether to split into N queries.
//
// `histogram_over_time(attr)` is deferred — the lowering would need a
// chplan node distinct from a scalar Aggregate, and the
// `/api/metrics/query_range` handler shape for histogram series has not
// landed yet.
func lowerMetricsAggregate(prev chplan.Node, agg *traceql.MetricsAggregate, s schema.Traces) (chplan.Node, error) {
	op := agg.Op()
	chFunc, params, err := mapMetricsAggregateOp(op, agg.Quantiles())
	if err != nil {
		return nil, err
	}

	args, err := metricsAggregateArgs(op, agg.Attribute(), s)
	if err != nil {
		return nil, err
	}

	groupBy, groupAliases, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        groupBy,
		GroupByAliases: groupAliases,
		AggFuncs: []chplan.AggFunc{{
			Name:   chFunc,
			Params: params,
			Args:   args,
			Alias:  metricsValueAlias,
		}},
	}, nil
}

// mapMetricsAggregateOp turns a TraceQL MetricsAggregateOp into the CH
// aggregate-function name + parameter list (for parameterised aggregates
// like `quantile(0.95)(value)`).
//
// rate / count_over_time both lower to `count` — the handler-side
// RangeWindow.Func discriminates them when it picks the per-step
// normalisation (rate divides by the window's seconds; count_over_time
// does not).
func mapMetricsAggregateOp(op traceql.MetricsAggregateOp, qs []float64) (name string, params []chplan.Expr, err error) {
	switch op {
	case traceql.MetricsAggregateRate, traceql.MetricsAggregateCountOverTime:
		return "count", nil, nil
	case traceql.MetricsAggregateSumOverTime:
		return "sum", nil, nil
	case traceql.MetricsAggregateAvgOverTime:
		return "avg", nil, nil
	case traceql.MetricsAggregateMinOverTime:
		return "min", nil, nil
	case traceql.MetricsAggregateMaxOverTime:
		return "max", nil, nil
	case traceql.MetricsAggregateQuantileOverTime:
		if len(qs) == 0 {
			return "", nil, fmt.Errorf("traceql: quantile_over_time requires at least one quantile")
		}
		if len(qs) > 1 {
			return "", nil, fmt.Errorf("traceql: multi-quantile quantile_over_time is not yet supported (got %d quantiles)", len(qs))
		}
		return "quantile", []chplan.Expr{&chplan.LitFloat{V: qs[0]}}, nil
	case traceql.MetricsAggregateHistogramOverTime:
		return "", nil, fmt.Errorf("traceql: histogram_over_time is not yet supported")
	}
	return "", nil, fmt.Errorf("traceql: metrics aggregate op %s is not yet supported", op)
}

// metricsAggregateArgs picks the inner expression list for the CH
// aggregate.
//
//   - rate / count_over_time: `count(1)` — there is no operand in the
//     TraceQL source; we count rows.
//   - *_over_time(attr) and quantile_over_time(attr, q): the lowered
//     attribute reference.
//
// The "no attribute supplied" sentinel is the zero-value `Attribute{}`
// (same convention Tempo's runtime uses at
// ast_metrics.go: `a.attr != (Attribute{})`).
func metricsAggregateArgs(op traceql.MetricsAggregateOp, attr traceql.Attribute, s schema.Traces) ([]chplan.Expr, error) {
	switch op {
	case traceql.MetricsAggregateRate, traceql.MetricsAggregateCountOverTime:
		// `count(1)` — keeps the parameterised form symmetrical with the
		// `count()` aggregate produced by lowerAggregate in aggregate.go.
		return []chplan.Expr{&chplan.LitInt{V: 1}}, nil
	}
	if attr == (traceql.Attribute{}) {
		return nil, fmt.Errorf("traceql: %s requires an attribute operand", op)
	}
	return []chplan.Expr{lowerAttribute(attr, s)}, nil
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
