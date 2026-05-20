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
	}
}
