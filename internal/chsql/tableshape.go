package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TableShape is an alias of chplan.TableShape, declared there so
// SchemaStrategies can carry one per deployed table: the sort columns,
// wide columns and integer discriminators PREWHERE promotion and the
// conjunct ordering read off the scanned table (prewhere.go).
type TableShape = chplan.TableShape

// defaultTableShapes is the shape of every table of the default OTel-CH
// schema, by its default name, ordered by the sorting key cerberus
// provisions it with. tableShapeFor reads it for a table the request's
// SchemaStrategies carry no shape for: the requirements check did not run,
// or ran before the table existed.
//
// Callers should treat the value as read-only.
var defaultTableShapes = buildDefaultTableShapes()

func buildDefaultTableShapes() map[string]TableShape {
	logs := schema.DefaultOTelLogs()
	traces := schema.DefaultOTelTraces()
	metrics := schema.DefaultOTelMetrics()

	out := map[string]TableShape{
		logs.LogsTable:    LogsTableShape(logs, nil),
		traces.SpansTable: TracesTableShape(traces, nil),
	}
	for _, tbl := range []string{
		metrics.GaugeTable, metrics.SumTable, metrics.HistogramTable, metrics.ExpHistogramTable, metrics.SummaryTable,
	} {
		out[tbl] = MetricsTableShape(metrics, nil)
	}
	return out
}

// LogsTableShape is the shape of a logs table of l whose sorting key sorts
// by sortColumns (schema.SortingKeyColumnsOf of its deployed key), or by the
// key cerberus provisions when sortColumns is nil.
//
// The provisioned OTel-CH `otel_logs` ORDER BY (exporter v0.152.0) is
// (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp) — pinned by
// internal/schema/ddl/ddl_test.go. The leading key is a monotonic bucketing
// of Timestamp, so ClickHouse prunes granules on it from a plain Timestamp
// predicate; the ranking only ever consumes bare column names
// (collectColumnRefs strips expressions), so the sort columns are the
// key's effective column order: Timestamp at rank 0, ServiceName at rank 1.
// Wide columns are the large per-row payloads (Body / ResourceAttributes /
// LogAttributes / ScopeAttributes); PREWHERE wants to defer reading them
// until granule pruning has narrowed the candidate set.
func LogsTableShape(l schema.Logs, sortColumns []string) TableShape {
	if sortColumns == nil {
		sortColumns = []string{l.TimestampColumn, l.ServiceNameColumn}
	}
	return TableShape{
		SortColumns: sortColumns,
		WideColumns: []string{
			l.BodyColumn,
			l.ResourceAttributesColumn,
			l.AttributesColumn,
			l.ScopeAttributesColumn,
		},
	}
}

// TracesTableShape is the shape of a spans table of t whose sorting key
// sorts by sortColumns, or by the key cerberus provisions when sortColumns
// is nil: ORDER BY (ServiceName, SpanName, toDateTime(Timestamp)), whose
// columns are ServiceName, SpanName, Timestamp. Wide columns include the
// SpanAttributes / ResourceAttributes maps and the Nested Events / Links
// columns.
func TracesTableShape(t schema.Traces, sortColumns []string) TableShape {
	if sortColumns == nil {
		sortColumns = []string{t.ServiceNameColumn, t.SpanNameColumn, t.TimestampColumn}
	}
	return TableShape{
		SortColumns: sortColumns,
		WideColumns: []string{
			t.AttributesColumn,
			t.ResourceAttributesColumn,
			t.EventsColumn,
			t.LinksColumn,
		},
	}
}

// MetricsTableShape is the shape of a metrics table of m whose sorting key
// sorts by sortColumns, or by the key cerberus provisions when sortColumns
// is nil: every metrics table shares ORDER BY (MetricName, Attributes,
// ServiceName, toUnixTimestamp64Nano(TimeUnix)) — the cerberus-ddl fork
// leads with MetricName so the common metric-name-first query (no
// service.name matcher) binary-searches the PK instead of falling to a
// generic-exclusion granule scan (measured 8-17x fewer granules). A stock
// exporter's table leads with ServiceName instead, which is why the
// deployed key, when read, decides the order. The schema package carries no
// per-signal WideColumns notion, so the wide set is supplied here —
// ResourceAttributes and ScopeAttributes are the large maps; Exemplars is a
// Nested column.
func MetricsTableShape(m schema.Metrics, sortColumns []string) TableShape {
	if sortColumns == nil {
		sortColumns = []string{m.MetricNameColumn, m.AttributesColumn, m.ServiceNameColumn, m.TimestampColumn}
	}
	return TableShape{
		SortColumns: sortColumns,
		WideColumns: []string{
			m.ResourceAttributesColumn,
			m.ScopeAttributesColumn,
			m.ExemplarsColumn,
		},
		IntegerDiscriminatorColumns: []string{
			m.AggregationTemporalityColumn,
		},
	}
}

// tableShapeFor returns the TableShape of table under strategies: the shape
// the requirements check resolved for the deployed table
// (SchemaStrategies.TableShapes), else the provisioned shape of a
// default-named table (defaultTableShapes), else the zero value. The zero
// TableShape has no sort columns and no wide columns, which makes the
// predicate classifier a no-op — callers get the unmodified WHERE clause
// back.
func tableShapeFor(table string, strategies SchemaStrategies) TableShape {
	if s, ok := strategies.TableShapes[table]; ok {
		return s
	}
	return defaultTableShapes[table]
}
