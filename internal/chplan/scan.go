package chplan

// Scan reads rows from a ClickHouse table. If Columns is empty, the
// emitter projects `*` and downstream Project nodes can narrow it; otherwise
// only the listed columns are read.
//
// Database is optional; when non-empty the emitter renders the table
// reference as `<Database>`.`<Table>` (both parts backtick-quoted) — used
// for synthetic single-row sources like `system.one` that the no-arg
// date functions scan. Empty Database emits just the bare table name,
// matching the original behaviour for every user-facing table.
//
// UnionTables — non-empty when the scan resolves across multiple
// physical tables that share the metric-row shape (MetricName, Attributes,
// TimeUnix, Value). The PromQL `__name__` matcher path uses this for
// unsuffixed names that could live in either the Gauge or Sum table:
// OTel hostmetrics / sqlquery emitters ship cumulative sums under bare
// names (`system_cpu_time`, `clickhouse_event`) that the Prom-naming
// heuristic alone routes to the Gauge table, returning empty. When
// UnionTables is set, the emitter renders a `(SELECT <projection> FROM
// <t_i> UNION ALL ...)` over the listed tables — one row stream per
// arm. Table is left empty in this mode; the emitter rejects the
// ambiguous case where both Table and UnionTables are set.
//
// Each arm is projected through the union-stable column subset
// (MetricName, Attributes, TimeUnix, Value, StartTimeUnix, ServiceName)
// so the Sum-only columns (AggregationTemporality, IsMonotonic) don't
// break the union's column-list match. Downstream wrappers (LWR,
// RangeWindow) read only the common columns, so the projection narrow
// is lossless for the metric-row pipeline.
type Scan struct {
	Database    string
	Table       string
	UnionTables []string
	Columns     []string
}

func (*Scan) planNode() {}

func (*Scan) Children() []Node { return nil }

func (s *Scan) Equal(other Node) bool {
	o, ok := other.(*Scan)
	if !ok || s.Database != o.Database || s.Table != o.Table {
		return false
	}
	if len(s.Columns) != len(o.Columns) {
		return false
	}
	for i := range s.Columns {
		if s.Columns[i] != o.Columns[i] {
			return false
		}
	}
	if len(s.UnionTables) != len(o.UnionTables) {
		return false
	}
	for i := range s.UnionTables {
		if s.UnionTables[i] != o.UnionTables[i] {
			return false
		}
	}
	return true
}
