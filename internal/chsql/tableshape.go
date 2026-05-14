package chsql

import "github.com/tsouza/cerberus/internal/schema"

// TableShape captures the ClickHouse-side facts the codegen needs to
// produce sort-key-aware filter orderings and PREWHERE promotion.
//
// SortColumns lists the table's ORDER BY columns in declaration order;
// the codegen prefers emitting predicates whose LHS references columns
// that appear earlier in this list. WideColumns names the "fat"
// columns (large strings, Maps, Arrays) that PREWHERE wants to avoid
// reading until predicates have culled the row set.
//
// SkipIndexColumns is reserved for future use (registered Bloom-filter
// or set indexes). The current default tables expose none — the field
// is plumbed so an override can populate it without a schema migration.
type TableShape struct {
	SortColumns      []string
	WideColumns      []string
	SkipIndexColumns []string
}

// IsSortColumn reports whether name is listed in SortColumns.
func (s TableShape) IsSortColumn(name string) bool {
	for _, c := range s.SortColumns {
		if c == name {
			return true
		}
	}
	return false
}

// SortRank returns the position of name in SortColumns, or -1 when
// the column is not part of the sort key. Lower rank = earlier in the
// ORDER BY, i.e. more selective for granule pruning.
func (s TableShape) SortRank(name string) int {
	for i, c := range s.SortColumns {
		if c == name {
			return i
		}
	}
	return -1
}

// IsWideColumn reports whether name is listed in WideColumns.
func (s TableShape) IsWideColumn(name string) bool {
	for _, c := range s.WideColumns {
		if c == name {
			return true
		}
	}
	return false
}

// IsSkipIndexColumn reports whether name has a registered skip index.
// Always false today (no defaults populate SkipIndexColumns) — kept as
// part of the public surface so future schema overrides can plug in.
func (s TableShape) IsSkipIndexColumn(name string) bool {
	for _, c := range s.SkipIndexColumns {
		if c == name {
			return true
		}
	}
	return false
}

// defaultTableShapes is the static lookup populated from the OTel-CH
// schema defaults. Keyed by table name so emitFilter can resolve a
// Scan's table to its shape without threading an explicit schema
// pointer through the emitter — the chsql package stays
// schema-agnostic at its public surface; this bridge is the one
// place that knows about both worlds.
//
// Callers should treat the value as read-only.
var defaultTableShapes = buildDefaultTableShapes()

func buildDefaultTableShapes() map[string]TableShape {
	logs := schema.DefaultOTelLogs()
	traces := schema.DefaultOTelTraces()
	metrics := schema.DefaultOTelMetrics()

	out := make(map[string]TableShape)

	// OTel-CH `otel_logs` ORDER BY: (ServiceName, SeverityText,
	// toUnixTimestamp(Timestamp), TraceId). Wide columns are the large
	// per-row payloads (Body / ResourceAttributes / LogAttributes /
	// ScopeAttributes); PREWHERE wants to defer reading them until
	// granule pruning has narrowed the candidate set.
	out[logs.LogsTable] = TableShape{
		SortColumns: []string{
			logs.ServiceNameColumn,
			logs.SeverityColumn,
			logs.TimestampColumn,
			logs.TraceIDColumn,
		},
		WideColumns: []string{
			logs.BodyColumn,
			logs.ResourceAttributesColumn,
			logs.AttributesColumn,
			logs.ScopeAttributesColumn,
		},
	}

	// OTel-CH `otel_traces` ORDER BY: (ServiceName, SpanName,
	// toUnixTimestamp(Timestamp), TraceId). Wide columns include the
	// SpanAttributes / ResourceAttributes maps and the Nested Events /
	// Links columns.
	out[traces.SpansTable] = TableShape{
		SortColumns: []string{
			traces.ServiceNameColumn,
			traces.SpanNameColumn,
			traces.TimestampColumn,
			traces.TraceIDColumn,
		},
		WideColumns: []string{
			traces.AttributesColumn,
			traces.ResourceAttributesColumn,
			traces.EventsColumn,
			traces.LinksColumn,
		},
	}

	// OTel-CH metrics tables: all share ORDER BY (ServiceName, MetricName,
	// Attributes, toUnixTimestamp64Nano(TimeUnix)). The schema package
	// doesn't carry a Metrics.WideColumns field yet, so the wide set is
	// supplied inline — ResourceAttributes and ScopeAttributes are the
	// large maps; Exemplars is a Nested column. A future refactor can
	// extend schema.Metrics with WideColumns + RowKey and move this list
	// there.
	metricsShape := TableShape{
		SortColumns: []string{
			metrics.ServiceNameColumn,
			metrics.MetricNameColumn,
			metrics.AttributesColumn,
			metrics.TimestampColumn,
		},
		WideColumns: []string{
			metrics.ResourceAttributesColumn,
			metrics.ScopeAttributesColumn,
			metrics.ExemplarsColumn,
		},
	}
	for _, tbl := range []string{
		metrics.GaugeTable,
		metrics.SumTable,
		metrics.HistogramTable,
		metrics.ExpHistogramTable,
		metrics.SummaryTable,
	} {
		out[tbl] = metricsShape
	}

	return out
}

// tableShapeFor returns the TableShape for table, or the zero value if
// the table is not registered. The zero TableShape has no sort columns
// and no wide columns, which makes the predicate classifier a no-op —
// callers get the unmodified WHERE clause back.
func tableShapeFor(table string) TableShape {
	if s, ok := defaultTableShapes[table]; ok {
		return s
	}
	return TableShape{}
}
