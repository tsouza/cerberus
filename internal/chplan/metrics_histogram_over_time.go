package chplan

// MetricsHistogramOverTime is the lowered form of TraceQL's
// `| histogram_over_time(<attr>)` metrics aggregator.
//
// Distinct from MetricsAggregate because the per-bucket value is a
// distribution — one row per (group-by, bucket) tuple carrying the
// count of spans whose <attr> falls in that bucket — rather than a
// scalar. The scalar `*_over_time` shapes live on MetricsAggregate;
// modelling histograms separately keeps the per-Op switch in
// MetricsAggregate's emitter from sprouting an array-shaped branch.
//
// Bucketing follows Tempo's runtime semantics (pkg/traceql/ast_metrics.go,
// `bucketizeFnFor`): each span's <attr> is rounded up to the nearest
// power of two and that log2-bucket key becomes an extra group-by
// column alongside the user-supplied GroupBy. Durations are emitted in
// seconds (matches Tempo's `Log2Bucketize(d) / float64(time.Second)`);
// other numeric attributes carry the raw `log2(ceil(v))` value.
// Spans with <attr> < 2 are dropped (Tempo's bucketizeDuration /
// bucketizeAttribute return `NewStaticNil()` for that range).
//
// Wrapping by chplan.RangeWindow is supported and produces the
// `/api/metrics/query_range` matrix shape: each per-span row fans out
// across N evaluation anchors via arrayJoin and the outer SELECT
// groups by (<user group-by>, bucket, anchor_ts). Bare emission (no
// RangeWindow wrapper) collapses across time — one row per
// (<user group-by>, bucket).
//
// Fields:
//
//   - Attr: the operand expression (the lowered `duration` /
//     `span.<attr>` / `resource.<attr>` reference). Required.
//   - IsDuration: when true, the emitter renders the bucket key as
//     log2(<attr>) / 1e9 so the bucket label reads in seconds (matches
//     Tempo's bucketizeDuration); when false, the bucket key is the
//     bare log2(<attr>) (matches Tempo's bucketizeAttribute on int /
//     duration cases — duration attrs encode as nanos already).
//   - GroupBy: the user-supplied `by(...)` label expressions; parallel
//     to GroupByAliases for SELECT-list aliasing.
//   - GroupByAliases: SQL SELECT-list alias for each GroupBy entry.
//     Bare-named for resource / span attributes (`service.name`,
//     `http.method`) so the chsql emitter renders `AS \`<bare>\“ with no
//     scope prefix in the column name.
//   - GroupByDisplayNames: optional parallel slice carrying the
//     TraceQL-canonical wire label name for each GroupBy entry — the
//     scope-prefixed form (`resource.service.name`, `span.http.method`)
//     that the Tempo metrics-query response surfaces. When empty the
//     Tempo handler falls back to GroupByAliases. Mirrors the same field
//     on MetricsAggregate; see that type for the cross-reference to
//     Tempo's upstream wire shape.
//   - BucketAlias: the SELECT-list alias for the synthesised bucket
//     column. The TraceQL runtime uses the internal label name
//     "__bucket"; cerberus mirrors that so downstream `/api/metrics/query_range`
//     wrap code can pick the bucket out of the row by a stable name.
//   - ValueAlias: the SELECT-list alias for the per-bucket count
//     (today always "Value", matching MetricsAggregate).
//   - Inner: the underlying spanset relation — typically a Filter
//     / Scan tree from the `{...}` selector.
type MetricsHistogramOverTime struct {
	Attr                Expr
	IsDuration          bool
	GroupBy             []Expr
	GroupByAliases      []string
	GroupByDisplayNames []string
	BucketAlias         string
	ValueAlias          string
	Inner               Node
}

func (*MetricsHistogramOverTime) planNode() {}

func (m *MetricsHistogramOverTime) Children() []Node { return []Node{m.Inner} }

func (m *MetricsHistogramOverTime) Equal(other Node) bool {
	o, ok := other.(*MetricsHistogramOverTime)
	if !ok {
		return false
	}
	if m.IsDuration != o.IsDuration ||
		m.BucketAlias != o.BucketAlias ||
		m.ValueAlias != o.ValueAlias {
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
	if (m.Inner == nil) != (o.Inner == nil) {
		return false
	}
	if m.Inner == nil {
		return true
	}
	return m.Inner.Equal(o.Inner)
}
