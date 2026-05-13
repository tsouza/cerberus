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
	}
}
