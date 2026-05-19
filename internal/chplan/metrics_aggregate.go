package chplan

// MetricsOp is the TraceQL metrics-pipeline operator that produced a
// MetricsAggregate node. Carries the semantic distinction collapsed by
// the previous lowering (which used a bare chplan.Aggregate and mapped
// rate / count_over_time to the same CH `count`): when a RangeWindow
// wraps a MetricsAggregate the emitter needs to discriminate the
// per-bucket normalisation (rate divides by the bucket's seconds,
// count_over_time does not).
type MetricsOp int

const (
	// MetricsOpInvalid is the zero value; surfaces as an emitter error.
	MetricsOpInvalid MetricsOp = iota
	// MetricsOpRate corresponds to TraceQL `| rate()` — count of spans
	// per bucket divided by the bucket duration (seconds).
	MetricsOpRate
	// MetricsOpCountOverTime corresponds to TraceQL `| count_over_time()` —
	// raw count of spans per bucket.
	MetricsOpCountOverTime
	// MetricsOpSumOverTime corresponds to TraceQL
	// `| sum_over_time(<attr>)`.
	MetricsOpSumOverTime
	// MetricsOpAvgOverTime corresponds to TraceQL
	// `| avg_over_time(<attr>)`.
	MetricsOpAvgOverTime
	// MetricsOpMinOverTime corresponds to TraceQL
	// `| min_over_time(<attr>)`.
	MetricsOpMinOverTime
	// MetricsOpMaxOverTime corresponds to TraceQL
	// `| max_over_time(<attr>)`.
	MetricsOpMaxOverTime
	// MetricsOpQuantileOverTime corresponds to TraceQL
	// `| quantile_over_time(<attr>, q)`; the quantile values are
	// carried on MetricsAggregate.Quantiles.
	MetricsOpQuantileOverTime
	// MetricsOpHistogramOverTime corresponds to TraceQL
	// `| histogram_over_time(<attr>)`. This Op never appears on a
	// MetricsAggregate — `histogram_over_time` lowers to its own
	// MetricsHistogramOverTime node because the per-bucket value is
	// a distribution rather than a scalar. The constant exists so
	// fork-side error reporting (e.g. unsupported parser shapes) can
	// reuse the same enum surface.
	MetricsOpHistogramOverTime
)

// String returns the TraceQL-source name of the operator (`rate`,
// `count_over_time`, etc.). Useful for error messages.
func (o MetricsOp) String() string {
	switch o {
	case MetricsOpRate:
		return "rate"
	case MetricsOpCountOverTime:
		return "count_over_time"
	case MetricsOpSumOverTime:
		return "sum_over_time"
	case MetricsOpAvgOverTime:
		return "avg_over_time"
	case MetricsOpMinOverTime:
		return "min_over_time"
	case MetricsOpMaxOverTime:
		return "max_over_time"
	case MetricsOpQuantileOverTime:
		return "quantile_over_time"
	case MetricsOpHistogramOverTime:
		return "histogram_over_time"
	}
	return "invalid"
}

// MetricsAggregate is the TraceQL metrics-pipeline aggregate: the lowered
// form of `| rate() / *_over_time(<attr>) [by(<labels>)]`. Distinct from
// chplan.Aggregate because:
//
//  1. The CH aggregate function picked at emit time depends on Op
//     (rate / count_over_time → count; *_over_time → the matching CH
//     aggregate). The previous lowering collapsed this distinction
//     into a bare Aggregate with `count` for both rate and
//     count_over_time, losing the per-step normalisation needed by a
//     wrapping RangeWindow.
//
//  2. A wrapping chplan.RangeWindow can plug a MetricsAggregate into
//     its Input slot (alongside the row-shape PromQL path) and the
//     emitter renders a time-bucketed matrix: one row per
//     (group-by-labels, anchor) tuple across [Start, End] spaced by
//     Step. See internal/chsql/range_window.go for the SQL skeleton.
//
// When emitted standalone (no RangeWindow wrapper), the SQL shape is
// the same as the bare Aggregate the previous lowering produced —
// keeps the TraceQL instant-metric fixtures byte-identical across the
// IR change.
//
// Fields:
//
//   - Op: the source TraceQL operator (rate / count_over_time / *_over_time
//     / quantile_over_time).
//   - Attr: the operand expression for *_over_time / quantile_over_time
//     (the lowered `duration` / `span.<attr>` / `resource.<attr>` reference).
//     Nil for rate and count_over_time (no operand at the source).
//   - GroupBy: the `by(...)` label expressions; parallel to GroupByAliases
//     for SELECT-list aliasing (mirrors chplan.Aggregate).
//   - GroupByAliases: SQL SELECT-list alias for each GroupBy entry.
//     Bare-named (e.g. `service.name`) so the chsql emitter renders
//     `AS \`service.name\`` regardless of TraceQL scope.
//   - GroupByDisplayNames: optional parallel slice carrying the
//     TraceQL-canonical wire label name for each GroupBy entry — the
//     scope-prefixed form (`resource.service.name`, `span.http.method`,
//     intrinsic name like `kind`) that the Tempo metrics-query response
//     surfaces. When empty (PromQL/LogQL paths never populate it) the
//     Tempo handler falls back to GroupByAliases, preserving the
//     historical wire shape for non-TraceQL callers. Tempo emits the
//     full scope-prefixed key (see upstream
//     `pkg/traceql.Attribute.String` + the metrics-engine label-build
//     loop) so cerberus mirrors that shape on the response without
//     altering the SQL-side alias used for column projection.
//   - Quantiles: the quantile values for MetricsOpQuantileOverTime;
//     today the lowering accepts a single quantile, so len(Quantiles)
//     is always 1 for that op. Empty for all other ops.
//   - ValueAlias: the SELECT-list alias for the metric output column
//     (today always "Value" from metricsValueAlias in
//     internal/traceql/metrics_pipeline.go).
//   - Inner: the underlying spanset relation — typically
//     Scan(<traces-table>) or Filter(<predicate>, Scan(<traces-table>)).
//
// For the matrix path (wrapping RangeWindow), the emitter also reads
// the underlying timestamp column from the wrapping RangeWindow's
// TimestampColumn slot; the per-span Timestamp column is the matrix
// shape's bucket key.
type MetricsAggregate struct {
	Op                  MetricsOp
	Attr                Expr
	GroupBy             []Expr
	GroupByAliases      []string
	GroupByDisplayNames []string
	Quantiles           []float64
	ValueAlias          string
	Inner               Node
}

func (*MetricsAggregate) planNode() {}

func (m *MetricsAggregate) Children() []Node { return []Node{m.Inner} }

func (m *MetricsAggregate) Equal(other Node) bool {
	o, ok := other.(*MetricsAggregate)
	if !ok {
		return false
	}
	if m.Op != o.Op || m.ValueAlias != o.ValueAlias {
		return false
	}
	if (m.Attr == nil) != (o.Attr == nil) {
		return false
	}
	if m.Attr != nil && !m.Attr.Equal(o.Attr) {
		return false
	}
	if len(m.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i := range m.GroupBy {
		if !m.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	if len(m.GroupByAliases) != len(o.GroupByAliases) {
		return false
	}
	for i := range m.GroupByAliases {
		if m.GroupByAliases[i] != o.GroupByAliases[i] {
			return false
		}
	}
	if len(m.GroupByDisplayNames) != len(o.GroupByDisplayNames) {
		return false
	}
	for i := range m.GroupByDisplayNames {
		if m.GroupByDisplayNames[i] != o.GroupByDisplayNames[i] {
			return false
		}
	}
	if len(m.Quantiles) != len(o.Quantiles) {
		return false
	}
	for i := range m.Quantiles {
		if m.Quantiles[i] != o.Quantiles[i] {
			return false
		}
	}
	if (m.Inner == nil) != (o.Inner == nil) {
		return false
	}
	if m.Inner == nil {
		return true
	}
	return m.Inner.Equal(o.Inner)
}
