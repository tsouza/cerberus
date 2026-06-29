package chplan

// MetricsCompare is the lowered form of TraceQL's
// `| compare({<selection>}, topN[, start, end])` metrics first-stage
// operator (Grafana Traces Drilldown's "Comparison" tab).
//
// Reference semantics (grafana/tempo pkg/traceql/engine_metrics_compare.go,
// the AGPL upstream cerberus reimplements clean-room — consulted only as a
// test-only oracle, never linked): every span
// produced by the query's pipeline is split into two cohorts —
// "selection" (spans matching the compare() filter, optionally further
// restricted to a unix-nanosecond start/end window) and "baseline"
// (everything else). For every attribute (name, value) carried by a
// span, the engine counts occurrences per cohort per time interval.
// The wire result is, per (cohort, attribute), the top-N values by
// total count — series labelled with the `__meta_type` scheme
// (baseline / selection / baseline_total / selection_total) — which
// Tempo's BaselineAggregator assembles at the query frontend.
//
// Cerberus splits the same work between SQL and the Tempo HTTP handler:
//
//   - SQL (this node, emitted by internal/chsql/metrics_compare.go)
//     produces the raw per-(cohort, attribute, value[, anchor]) counts:
//     the span-attribute explosion is an arrayJoin over Pairs, the
//     cohort flag is the Selection predicate projected as a 0/1 column.
//   - The handler (internal/api/tempo/metrics_query_range_compare.go)
//     mirrors Tempo's BaselineAggregator: top-N per (cohort, attribute)
//     by total count, per-attribute totals series, zero-filled anchor
//     grids, and the `__meta_type` label scheme.
//
// Fields:
//
//   - Selection: the lowered compare({...}) filter predicate — a boolean
//     expression over a span row of Inner. When StartNs/EndNs are both
//     set, the lowering has already AND-ed the timestamp window into
//     this expression (mirroring MetricsCompare.isSelection upstream,
//     which only evaluates the filter for spans inside the window and
//     assigns everything else to the baseline).
//   - TopN: the per-attribute series cap (upstream default 10). Carried
//     on the IR for the handler's BaselineAggregator-equivalent; the
//     SQL emits ALL observed values because the totals series must
//     count every occurrence, not just the top-N survivors.
//   - StartNs / EndNs: the optional selection window (unix nanoseconds,
//     0 = unset). Carried for plan printing / Equal; the executable
//     window predicate already lives inside Selection.
//   - Pairs: the Array(Tuple(String, String)) expression enumerating
//     every (attribute wire name, value) pair of a span row — the
//     OTel-CH analogue of Tempo's `span.AllAttributesFunc` under
//     `SecondPassSelectAll` (vparquet4): intrinsics (name / status /
//     statusMessage / kind, trace-level rootName / rootServiceName,
//     instrumentation:name / instrumentation:version), the well-known
//     dedicated attributes with a 'nil' fallback for absent values,
//     and the two attribute maps with scope prefixes. Built by the
//     lowering (internal/traceql/metrics_compare.go) because it is
//     schema-dependent.
//   - RootLookup: optional relation resolving each trace's root span —
//     one row per TraceId with the columns RootNameAlias /
//     RootServiceAlias. The emitter LEFT JOINs it against Inner on
//     TraceIDColumn so Pairs can reference the per-trace rootName /
//     rootServiceName values (vparquet stores them as trace-level
//     columns; OTel-CH must derive them). Nil disables the join (used
//     by schemas where Pairs doesn't reference root columns).
//   - TraceIDColumn: join key for RootLookup (OTel-CH: "TraceId").
//   - RootNameAlias / RootServiceAlias: SELECT-list aliases RootLookup
//     exposes and Pairs references ("__root_name" / "__root_service_name").
//   - SelAlias / AttrAlias / ValAlias / ValueAlias: output column
//     aliases — the cohort flag, attribute wire name, attribute value,
//     and per-group count ("is_selection" / "attr" / "val" / "Value").
//   - Inner: the underlying spanset relation (Filter / Scan tree from
//     the query's `{...}` pipeline).
//
// Wrapping by chplan.RangeWindow produces the `/api/metrics/query_range`
// matrix shape — one row per (cohort, attr, val, anchor_ts). Bare
// emission collapses time — one row per (cohort, attr, val), ordered
// deterministically for the TXTAR roundtrip layer.
type MetricsCompare struct {
	Selection        Expr
	TopN             int
	StartNs          int64
	EndNs            int64
	Pairs            Expr
	RootLookup       Node
	TraceIDColumn    string
	RootNameAlias    string
	RootServiceAlias string
	SelAlias         string
	AttrAlias        string
	ValAlias         string
	ValueAlias       string
	Inner            Node
}

func (*MetricsCompare) planNode() {}

func (m *MetricsCompare) Children() []Node {
	if m.RootLookup == nil {
		return []Node{m.Inner}
	}
	return []Node{m.Inner, m.RootLookup}
}

func (m *MetricsCompare) Equal(other Node) bool {
	o, ok := other.(*MetricsCompare)
	if !ok {
		return false
	}
	if m.TopN != o.TopN || m.StartNs != o.StartNs || m.EndNs != o.EndNs ||
		m.TraceIDColumn != o.TraceIDColumn ||
		m.RootNameAlias != o.RootNameAlias || m.RootServiceAlias != o.RootServiceAlias ||
		m.SelAlias != o.SelAlias || m.AttrAlias != o.AttrAlias ||
		m.ValAlias != o.ValAlias || m.ValueAlias != o.ValueAlias {
		return false
	}
	if (m.Selection == nil) != (o.Selection == nil) {
		return false
	}
	if m.Selection != nil && !m.Selection.Equal(o.Selection) {
		return false
	}
	if (m.Pairs == nil) != (o.Pairs == nil) {
		return false
	}
	if m.Pairs != nil && !m.Pairs.Equal(o.Pairs) {
		return false
	}
	if (m.RootLookup == nil) != (o.RootLookup == nil) {
		return false
	}
	if m.RootLookup != nil && !m.RootLookup.Equal(o.RootLookup) {
		return false
	}
	if (m.Inner == nil) != (o.Inner == nil) {
		return false
	}
	if m.Inner == nil {
		return true
	}
	return m.Inner.Equal(o.Inner)
}
