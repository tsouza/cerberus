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
	if v, ok := mp.(*traceql.AverageOverTimeAggregator); ok {
		return lowerAverageOverTime(prev, v, s)
	}
	return nil, fmt.Errorf("traceql: metrics pipeline element %T is unsupported", mp)
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
// `quantile_over_time(attr, q1, q2, ...)` is supported for any number
// of phi values; the IR carries the full slice on
// chplan.MetricsAggregate.Quantiles and the chsql emit path (see
// internal/chsql/range_window.go) fans out one output series per phi
// tagged with a synthetic `__phi__` label.
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

	groupBy, groupAliases, groupDisplay, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	var quantiles []float64
	if op == traceql.MetricsAggregateQuantileOverTime {
		qs := agg.Quantiles()
		if len(qs) == 0 {
			return nil, fmt.Errorf("traceql: quantile_over_time requires at least one quantile")
		}
		// Multi-quantile is accepted at the lowering layer (v0.0.3
		// fork accessors expose the full slice via Quantiles()); the
		// emitted plan carries all phi values on
		// chplan.MetricsAggregate.Quantiles. The chsql emitter
		// (internal/chsql/range_window.go::metricsAggregateCH plus
		// the bare/matrix emit fanout) renders one output series per
		// phi tagged with the synthetic `__phi__` label.
		quantiles = make([]float64, len(qs))
		copy(quantiles, qs)
	}

	// IsDuration drives the bucketise divisor in the chsql matrix-path
	// quantile emitter (and the post-processor in
	// internal/api/tempo/metrics_query_range.go) so the bucket edges fed
	// into Tempo's Log2QuantileWithBucket match what
	// pkg/traceql.bucketizeDuration produces upstream — Log2Bucketize(d)
	// in nanos, divided by 1e9 so the bucket reads in seconds. Only
	// quantile_over_time consults IsDuration today; the rest of the
	// matrix path emits the same SQL whether or not the operand was
	// originally a duration intrinsic.
	isDuration := op == traceql.MetricsAggregateQuantileOverTime && agg.Attribute().Intrinsic == traceql.IntrinsicDuration

	return &chplan.MetricsAggregate{
		Op:                  cop,
		Attr:                attr,
		GroupBy:             groupBy,
		GroupByAliases:      groupAliases,
		GroupByDisplayNames: groupDisplay,
		Quantiles:           quantiles,
		IsDuration:          isDuration,
		ValueAlias:          metricsValueAlias,
		Inner:               prev,
	}, nil
}

// lowerAverageOverTime lowers Tempo's dedicated
// *traceql.AverageOverTimeAggregator (the unexported
// averageOverTimeAggregator surfaced via the exported type alias in the
// cerberus-accessors fork) into a chplan.MetricsAggregate with
// Op=MetricsOpAvgOverTime.
//
// Tempo parses `| avg_over_time(attr)` into
// *averageOverTimeAggregator rather than *MetricsAggregate because
// avg_over_time uses a Kahan-summation weighted-average algorithm that
// doesn't fit MetricsAggregate's generic init() switch. For cerberus's
// purposes the lowering is identical — we emit `avg(<attr>)` in CH SQL
// — so we just unwrap the accessor fields and produce the same
// chplan.MetricsAggregate node.
func lowerAverageOverTime(prev chplan.Node, agg *traceql.AverageOverTimeAggregator, s schema.Traces) (chplan.Node, error) {
	attr, err := metricsAggregateAttr(traceql.MetricsAggregateAvgOverTime, agg.Attribute(), s)
	if err != nil {
		return nil, err
	}

	groupBy, groupAliases, groupDisplay, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	return &chplan.MetricsAggregate{
		Op:                  chplan.MetricsOpAvgOverTime,
		Attr:                attr,
		GroupBy:             groupBy,
		GroupByAliases:      groupAliases,
		GroupByDisplayNames: groupDisplay,
		ValueAlias:          metricsValueAlias,
		Inner:               prev,
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
	return chplan.MetricsOpInvalid, fmt.Errorf("traceql: metrics aggregate op %s is unsupported", op)
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
//
// Unit conversion for `duration`: the OTel-CH `Duration` column is
// Int64 nanoseconds. Tempo's reference engine emits metric values in
// fractional seconds — its sumOverTimeAggregator / averageOverTimeAggregator
// / quantileOverTimeAggregator all read `Span.DurationNanos()` and
// divide by 1e9 before producing the per-bucket sample. To match Tempo's
// wire shape (so the differ's `metrics_avg_over_time_instant` and
// kin compare against a Tempo-side value in seconds, not raw ns), we
// wrap the lowered duration expression in `<expr> / 1e9` at lowering
// time when the operand is the `duration` intrinsic. The wrap applies
// to sum / avg / min / max / quantile aggregations — all the
// duration-aware *_over_time forms; rate / count_over_time fall out of
// the switch above before this code runs.
func metricsAggregateAttr(op traceql.MetricsAggregateOp, attr traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	switch op {
	case traceql.MetricsAggregateRate, traceql.MetricsAggregateCountOverTime:
		return nil, nil
	}
	if attr == (traceql.Attribute{}) {
		return nil, fmt.Errorf("traceql: %s requires an attribute operand", op)
	}
	// Map(String, String) coercion: see coerceMapNumericAggInput in
	// aggregate.go. `*_over_time(span.foo)` resolves to a FieldAccess
	// against SpanAttributes (typed String); wrap so the downstream CH
	// aggregate (`max`/`min`/`sum`/`avg`/`quantiles`) sees a Float64.
	attrExpr, err := lowerAttribute(attr, s)
	if err != nil {
		return nil, err
	}
	expr := coerceMapNumericAggInput(attrExpr)
	if attr.Intrinsic == traceql.IntrinsicDuration {
		expr = durationNsToSeconds(expr)
	}
	return expr, nil
}

// nanosecondsPerSecond is the divisor used to rebase Int64-ns duration
// values into fractional seconds for duration-based metrics
// aggregations. Mirrors Tempo's `nanosToSec` constant
// (pkg/traceql/engine_metrics.go: 1e9 in the
// average/sum/quantileOverTime aggregators) so cerberus emits values in
// the same unit Tempo's wire shape uses.
const nanosecondsPerSecond = 1e9

// durationNsToSeconds wraps the lowered duration expression in
// `<expr> / 1e9`. Kept as a tiny helper so the unit-conversion site is
// easy to spot — the only callers are metricsAggregateAttr (the
// MetricsAggregate operand) and any future second-stage shapers that
// need the same rebase.
//
// Why a typed chplan.Binary rather than a CH-side `toFloat64(...) /
// 1e9` FuncCall: the chsql emitter renders Binary{OpDiv} as the bare
// SQL `/` operator and ClickHouse already promotes the
// `Int64 / Float64` arithmetic to Float64 — no explicit cast required.
// The result feeds straight into `sum` / `avg` / `min` / `max` /
// `quantile`, which all accept Float64.
func durationNsToSeconds(e chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  e,
		Right: &chplan.LitFloat{V: nanosecondsPerSecond},
	}
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
	attrExpr, err := lowerAttribute(attr, s)
	if err != nil {
		return nil, err
	}

	groupBy, groupAliases, groupDisplay, err := lowerMetricsGroupBy(agg.GroupBy(), s)
	if err != nil {
		return nil, err
	}

	return &chplan.MetricsHistogramOverTime{
		Attr:                attrExpr,
		IsDuration:          attr.Intrinsic == traceql.IntrinsicDuration,
		GroupBy:             groupBy,
		GroupByAliases:      groupAliases,
		GroupByDisplayNames: groupDisplay,
		BucketAlias:         histogramBucketAlias,
		ValueAlias:          metricsValueAlias,
		Inner:               prev,
	}, nil
}

// lowerMetricsGroupBy turns a TraceQL `by (<attr>, <attr>, ...)` list
// into the chplan grouping expressions + parallel alias slice + parallel
// display-name slice.
//
// Each `by` attribute lowers via the same accessor lowerAttribute uses
// for spanset matchers, so resource.* / span.* / intrinsics all resolve
// to the right carrier (column ref for intrinsics, FieldAccess for map
// keys).
//
// Two parallel name slices come back:
//
//   - aliases: the SQL SELECT-list alias. For a scoped attribute this is
//     the bare `attr.Name` (e.g. `service.name` for `resource.service.name`)
//     so the emitter renders `AS \`service.name\“ — keeping the column
//     name short and identifying the carrier-side payload only. Locking
//     in the bare form preserves the existing TXTAR SQL fixtures.
//   - display: the Tempo-canonical wire label name (`attr.String()`).
//     For a resource-scoped attribute this is `resource.service.name`;
//     for a span-scoped attribute, `span.http.method`; for an intrinsic
//     such as `kind`, just `kind`. The Tempo metrics-query handler
//     uses this name when projecting the response's `Labels` map so the
//     wire shape matches upstream Tempo's `pkg/traceql.Labels.String`
//     output (which calls `Attribute.String()` per the engine_metrics
//     `labelsFor` loop). The SQL emitter ignores this slice; only the
//     /api/metrics handler reads it.
//
// Returns (nil, nil, nil, nil) when the `by(...)` list is empty — that
// matches chplan.Aggregate's convention for "no grouping".
func lowerMetricsGroupBy(attrs []traceql.Attribute, s schema.Traces) ([]chplan.Expr, []string, []string, error) {
	if len(attrs) == 0 {
		return nil, nil, nil, nil
	}
	exprs := make([]chplan.Expr, 0, len(attrs))
	aliases := make([]string, 0, len(attrs))
	display := make([]string, 0, len(attrs))
	for _, a := range attrs {
		if a == (traceql.Attribute{}) {
			return nil, nil, nil, fmt.Errorf("traceql: empty by(...) group key")
		}
		expr, err := lowerAttribute(a, s)
		if err != nil {
			return nil, nil, nil, err
		}
		exprs = append(exprs, expr)
		// SQL alias is the bare attribute path (`a.Name` is set by
		// traceql.NewAttribute / NewScopedAttribute / NewIntrinsic to
		// either the carrier-map key, e.g. `service.name`, or the
		// intrinsic's source-text name, e.g. `kind`). Locking in the
		// bare form keeps every TXTAR SQL fixture stable across this
		// change. The Tempo-canonical wire name (the scope-prefixed
		// `Attribute.String()` form) ships on GroupByDisplayNames so
		// the /api/metrics handler can surface it without the chsql
		// emitter caring.
		aliases = append(aliases, a.Name)
		display = append(display, a.String())
	}
	return exprs, aliases, display, nil
}
