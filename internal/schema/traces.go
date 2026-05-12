package schema

// Traces describes how cerberus reads spans from ClickHouse. The default
// (returned by DefaultOTelTraces) matches the OpenTelemetry ClickHouse
// Exporter v0.x traces schema; users with custom layouts override
// individual fields.
type Traces struct {
	// SpansTable is the table holding span records.
	SpansTable string

	// TraceIDColumn names the trace-id column (FixedString(16) hex).
	TraceIDColumn string
	// SpanIDColumn names the span-id column (FixedString(8) hex).
	SpanIDColumn string
	// ParentSpanIDColumn names the parent-span-id column.
	ParentSpanIDColumn string
	// SpanNameColumn names the span name column.
	SpanNameColumn string
	// SpanKindColumn names the span kind column ("Client", "Server", ...).
	SpanKindColumn string
	// ServiceNameColumn names the dedicated service.name column.
	ServiceNameColumn string

	// DurationColumn names the span-duration column (Int64 nanoseconds).
	DurationColumn string
	// StartTimeColumn names the span-start timestamp column.
	StartTimeColumn string
	// EndTimeColumn names the span-end timestamp column.
	EndTimeColumn string

	// StatusCodeColumn names the status code column ("Unset", "Ok", "Error").
	StatusCodeColumn string
	// StatusMessageColumn names the status message column.
	StatusMessageColumn string

	// AttributesColumn names the span-level attribute map.
	AttributesColumn string
	// ResourceAttributesColumn names the resource attribute map (carries
	// service-identity labels).
	ResourceAttributesColumn string
	// ScopeAttributesColumn names the instrumentation-scope attribute map.
	ScopeAttributesColumn string

	// TimestampColumn names the canonical event timestamp column. For
	// OTel-CH this is the same as StartTimeColumn ("Timestamp" in newer
	// schemas, often "StartTimeUnix" in older).
	TimestampColumn string
}

// DefaultOTelTraces returns the schema produced by the upstream OTel
// ClickHouse Exporter for traces.
func DefaultOTelTraces() Traces {
	return Traces{
		SpansTable:               "otel_traces",
		TraceIDColumn:            "TraceId",
		SpanIDColumn:             "SpanId",
		ParentSpanIDColumn:       "ParentSpanId",
		SpanNameColumn:           "SpanName",
		SpanKindColumn:           "SpanKind",
		ServiceNameColumn:        "ServiceName",
		DurationColumn:           "Duration",
		StartTimeColumn:          "Timestamp",
		EndTimeColumn:            "Timestamp", // OTel-CH stores duration; end = start + duration
		StatusCodeColumn:         "StatusCode",
		StatusMessageColumn:      "StatusMessage",
		AttributesColumn:         "SpanAttributes",
		ResourceAttributesColumn: "ResourceAttributes",
		ScopeAttributesColumn:    "ScopeAttributes",
		TimestampColumn:          "Timestamp",
	}
}
