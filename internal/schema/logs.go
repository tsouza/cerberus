package schema

// Logs describes how cerberus reads logs from ClickHouse. The default
// (returned by DefaultOTelLogs) matches the OpenTelemetry ClickHouse
// Exporter v0.x logs schema; users with custom layouts override
// individual fields via Config.
//
// Column names mirror the upstream `logs_table.sql` template verbatim.
type Logs struct {
	// LogsTable is the table holding log records.
	LogsTable string

	// BodyColumn names the column carrying the log message body (String).
	BodyColumn string
	// SeverityColumn names the column carrying the severity text
	// (e.g. "INFO", "ERROR").
	SeverityColumn string
	// SeverityNumberColumn names the numeric severity column (UInt8 upstream).
	SeverityNumberColumn string
	// AttributesColumn names the log-level attribute map. Mirrors the
	// upstream `LogAttributes` column.
	AttributesColumn string
	// ResourceAttributesColumn names the resource attribute map
	// (carries stream-identity labels like service.name, job, etc.).
	ResourceAttributesColumn string
	// ScopeNameColumn names the instrumentation-scope name column.
	ScopeNameColumn string
	// ScopeVersionColumn names the instrumentation-scope version column.
	ScopeVersionColumn string
	// ScopeAttributesColumn names the instrumentation-scope attribute map.
	ScopeAttributesColumn string
	// TimestampColumn names the per-record timestamp column (DateTime64).
	TimestampColumn string
	// TraceIDColumn names the trace-id correlation column.
	TraceIDColumn string
	// SpanIDColumn names the span-id correlation column.
	SpanIDColumn string
	// TraceFlagsColumn names the OTel TraceFlags column (UInt8).
	TraceFlagsColumn string
	// ServiceNameColumn names the dedicated service name column.
	ServiceNameColumn string
	// EventNameColumn names the OTel log-record event name column
	// (String; populated when the LogRecord carries a structured event
	// name distinct from the body).
	EventNameColumn string

	// WideColumns lists the "fat" columns on the logs table — columns
	// whose per-row payload is large enough that fetching them dominates
	// the IO cost of a Scan. The chsql late-materialisation rewrite
	// checks this list when a Project+Limit+Filter+Scan stack lands on
	// this table: if any of the projection's columns are wide, the
	// inner SELECT skips them and an outer JOIN back fetches them only
	// for the surviving rows.
	//
	// For the OTel-CH default this is `Body` + `ResourceAttributes` +
	// `LogAttributes` — each can carry hundreds of bytes per row.
	WideColumns []string

	// MaterializedResourceColumns maps a ResourceAttributes map key
	// (e.g. `k8s.namespace.name`) to the dedicated top-level
	// LowCardinality(String) column the OTel ClickHouse Exporter
	// MATERIALIZEs from that key (e.g. `__otel_materialized_k8s.namespace.name`).
	//
	// The exporter's `logs_table.sql` template defines each such column
	// as `LowCardinality(String) MATERIALIZED ResourceAttributes['<key>']`,
	// so reading the column is byte-for-byte equivalent to reading the
	// map key — including the empty-string default a missing key yields —
	// but avoids decompressing the wide ResourceAttributes Map. A
	// stream-selector matcher (or inner range-aggregation group-by) whose
	// label resolves through this table emits a bare ColumnRef against the
	// materialized column instead of a Map access.
	//
	// This is the opt-in gate: DefaultOTelLogs() populates it from the
	// exporter's exact DDL set; a custom-schema user whose otel_logs has
	// no `__otel_materialized_*` columns (or who renamed
	// ResourceAttributesColumn) leaves it nil and stays on the map read,
	// mirroring the resourceFallbackColumn opt-out. Only the logs table
	// carries these columns — the traces / metrics tables ship a plain
	// ResourceAttributes Map with no materialized siblings — so the
	// routing is LogQL-only by construction.
	MaterializedResourceColumns map[string]string

	// RowKey is the tuple of columns that uniquely identifies a row in
	// the logs table. Used by the chsql late-materialisation rewrite
	// as the JOIN key between the thin inner SELECT and the outer
	// "fetch wide columns" SELECT. The tuple must be globally
	// unique — duplicates would yield a row-multiplication bug.
	//
	// For the OTel-CH default this is `(Timestamp, TraceId, SpanId)` —
	// timestamps alone are not unique (multiple log records can share a
	// timestamp, especially at coarser-than-ns precision), but the
	// (TraceId, SpanId) suffix supplies the entropy. Logs ingested
	// without trace context all share TraceId=000…0 / SpanId=000…0; in
	// that degenerate case the rewrite cannot apply — callers should
	// guard with HasUniqueRowKey().
	RowKey []string
}

// HasUniqueRowKey reports whether RowKey is non-empty — the precondition
// for the late-materialisation rewrite. Custom-schema users with
// non-unique storage layouts leave RowKey empty to bypass the rewrite.
func (l Logs) HasUniqueRowKey() bool { return len(l.RowKey) > 0 }

// materializedColumnPrefix is the literal prefix the OTel ClickHouse
// Exporter prepends to a ResourceAttributes map key to name the
// MATERIALIZED column it hoists that key into. The exporter's
// `logs_table.sql` template spells the full column name as
// `__otel_materialized_<key>` (the key verbatim, dots preserved) — see
// the `MATERIALIZED ResourceAttributes['<key>']` column definitions.
const materializedColumnPrefix = "__otel_materialized_"

// defaultMaterializedResourceKeys lists the ResourceAttributes map keys
// the OTel ClickHouse Exporter MATERIALIZEs into dedicated top-level
// LowCardinality(String) columns on the logs table. Read verbatim from
// the exporter's `logs_table.sql` template — the column for each key is
// defined as `MATERIALIZED ResourceAttributes['<key>']`, so reading the
// column is equivalent to reading the map key. The set is exporter DDL,
// not a cerberus-chosen allow-list: it tracks exactly which keys the
// shipped schema promotes.
var defaultMaterializedResourceKeys = []string{
	"k8s.cluster.name",
	"k8s.container.name",
	"k8s.deployment.name",
	"k8s.namespace.name",
	"k8s.node.name",
	"k8s.pod.name",
	"k8s.pod.uid",
	"deployment.environment.name",
}

// defaultMaterializedResourceColumns builds the {map-key →
// materialized-column} table from defaultMaterializedResourceKeys by
// prepending materializedColumnPrefix to each key — mirroring the
// exporter's column-naming rule exactly.
func defaultMaterializedResourceColumns() map[string]string {
	out := make(map[string]string, len(defaultMaterializedResourceKeys))
	for _, key := range defaultMaterializedResourceKeys {
		out[key] = materializedColumnPrefix + key
	}
	return out
}

// DefaultOTelLogs returns the schema produced by the upstream OTel
// ClickHouse Exporter for logs.
func DefaultOTelLogs() Logs {
	return Logs{
		LogsTable:                "otel_logs",
		BodyColumn:               "Body",
		SeverityColumn:           "SeverityText",
		SeverityNumberColumn:     "SeverityNumber",
		AttributesColumn:         "LogAttributes",
		ResourceAttributesColumn: "ResourceAttributes",
		ScopeNameColumn:          "ScopeName",
		ScopeVersionColumn:       "ScopeVersion",
		ScopeAttributesColumn:    "ScopeAttributes",
		TimestampColumn:          "Timestamp",
		TraceIDColumn:            "TraceId",
		SpanIDColumn:             "SpanId",
		TraceFlagsColumn:         "TraceFlags",
		ServiceNameColumn:        "ServiceName",
		EventNameColumn:          "EventName",
		// Wide columns — large per-row payloads. Late materialisation
		// defers fetching these until after filter+limit.
		WideColumns: []string{"Body", "ResourceAttributes", "LogAttributes"},
		// (Timestamp, TraceId, SpanId) uniquely identifies an OTel-CH
		// log row when ingestion carries trace context. See the
		// WideColumns godoc above.
		RowKey: []string{"Timestamp", "TraceId", "SpanId"},
		// The 8 k8s.* / deployment.environment.name resource attributes
		// the exporter MATERIALIZEs into dedicated LowCardinality columns
		// on the logs table — routing a matcher/group-by here avoids
		// decompressing the wide ResourceAttributes Map.
		MaterializedResourceColumns: defaultMaterializedResourceColumns(),
	}
}
