package schema

// Traces describes how cerberus reads spans from ClickHouse. The default
// (returned by DefaultOTelTraces) matches the OpenTelemetry ClickHouse
// Exporter v0.x traces schema; users with custom layouts override
// individual fields.
//
// Column names mirror the upstream `traces_table.sql` template
// verbatim, plus a synthetic `EndTimeColumn` (`Timestamp` — OTel-CH
// stores duration; end = start + duration) for the TraceQL emitter.
type Traces struct {
	// SpansTable is the table holding span records.
	SpansTable string

	// TraceIDColumn names the trace-id column (FixedString(16) hex).
	TraceIDColumn string
	// SpanIDColumn names the span-id column (FixedString(8) hex).
	SpanIDColumn string
	// ParentSpanIDColumn names the parent-span-id column.
	ParentSpanIDColumn string
	// TraceStateColumn names the W3C trace-state column.
	TraceStateColumn string
	// SpanNameColumn names the span name column.
	SpanNameColumn string
	// SpanKindColumn names the span kind column ("Client", "Server", ...).
	SpanKindColumn string
	// ServiceNameColumn names the dedicated service.name column.
	ServiceNameColumn string

	// DurationColumn names the span-duration column (UInt64 nanoseconds).
	DurationColumn string
	// StartTimeColumn names the span-start timestamp column.
	StartTimeColumn string
	// EndTimeColumn is a cerberus synthetic — OTel-CH stores duration;
	// end-time is derived as `Timestamp + Duration`. The emitter substitutes
	// the literal computation when this string equals StartTimeColumn.
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
	// ScopeNameColumn names the instrumentation-scope name column.
	ScopeNameColumn string
	// ScopeVersionColumn names the instrumentation-scope version column.
	ScopeVersionColumn string
	// ScopeAttributesColumn names the instrumentation-scope attribute map.
	// NOTE: the upstream `traces_table.sql` template does not currently
	// declare a ScopeAttributes column; cerberus carries this field so
	// custom-schema users can point it at their own column. The default
	// is the empty string so emitters can skip it when unset.
	ScopeAttributesColumn string

	// EventsColumn names the Nested span-events column (Timestamp /
	// Name / Attributes per row).
	EventsColumn string
	// LinksColumn names the Nested span-links column (TraceId / SpanId /
	// TraceState / Attributes per row).
	LinksColumn string

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
		TraceStateColumn:         "TraceState",
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
		ScopeNameColumn:          "ScopeName",
		ScopeVersionColumn:       "ScopeVersion",
		// Upstream traces_table.sql has no ScopeAttributes column; leave
		// empty so callers that consult it can skip the projection.
		ScopeAttributesColumn: "",
		EventsColumn:          "Events",
		LinksColumn:           "Links",
		TimestampColumn:       "Timestamp",
	}
}
