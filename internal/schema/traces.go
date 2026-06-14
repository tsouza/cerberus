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

	// TraceIDTsTable names the `<spans>_trace_id_ts` lookup table the
	// OTel-CH exporter populates via a materialized view: one row per
	// TraceId carrying (Start, End) = (min, max) of the trace's span
	// Timestamps. Cerberus reads it to inject a Timestamp-window
	// pre-filter into the trace-by-ID spans scan so the spans table can
	// Partition/PrimaryKey/MinMax-prune to ~1 granule instead of scanning
	// every part to apply the bloom filter. Defaults to
	// `<SpansTable>_trace_id_ts`.
	TraceIDTsTable string
	// TraceIDTsStartColumn names the trace-window lower-bound column on
	// TraceIDTsTable (min of the trace's span Timestamps). OTel-CH default
	// "Start".
	TraceIDTsStartColumn string
	// TraceIDTsEndColumn names the trace-window upper-bound column on
	// TraceIDTsTable (max of the trace's span Timestamps). OTel-CH default
	// "End".
	TraceIDTsEndColumn string

	// TraceIDTsEnabled gates the Timestamp-window pre-filter described on
	// TraceIDTsTable. OFF by default: the window reads the lookup table via
	// scalar subqueries, and if the MV is absent or unpopulated those
	// subqueries yield NULL and the windowed scan matches nothing. The
	// operator opts in (CERBERUS_SCHEMA_TRACES_TS_LOOKUP) only after
	// confirming the MV is populated — matching the AutoCreateSchema
	// "operator owns DDL" posture and avoiding an emit-time table-existence
	// probe (a layering violation). With the gate off, lowerTraceByID emits
	// today's plain `TraceId = ?` filter unchanged.
	TraceIDTsEnabled bool

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

	// WideColumns lists the "fat" columns on the spans table — columns
	// whose per-row payload is large enough that fetching them dominates
	// the IO cost of a Scan. The chsql late-materialisation rewrite
	// checks this list when a Project+Limit+Filter+Scan stack lands on
	// this table: if any of the projection's columns are wide, the
	// inner SELECT skips them and an outer JOIN back fetches them only
	// for the surviving rows.
	//
	// For the OTel-CH default this is `SpanAttributes` +
	// `ResourceAttributes` + the two Nested columns (`Events`, `Links`).
	WideColumns []string

	// RowKey is the tuple of columns that uniquely identifies a span row.
	// (TraceId, SpanId) is the natural primary key per OTel spec; the
	// Timestamp prefix matches the OTel-CH table's PREWHERE-friendly
	// ORDER BY tuple so the late-materialisation JOIN benefits from
	// the same sort-key pruning the inner SELECT does.
	RowKey []string
}

// HasUniqueRowKey reports whether RowKey is non-empty — the precondition
// for the late-materialisation rewrite.
func (t Traces) HasUniqueRowKey() bool { return len(t.RowKey) > 0 }

// DefaultOTelTraces returns the schema produced by the upstream OTel
// ClickHouse Exporter for traces.
func DefaultOTelTraces() Traces {
	return Traces{
		SpansTable:               "otel_traces",
		TraceIDTsTable:           "otel_traces_trace_id_ts",
		TraceIDTsStartColumn:     "Start",
		TraceIDTsEndColumn:       "End",
		TraceIDTsEnabled:         false,
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
		// Wide columns — large per-row payloads. Late materialisation
		// defers fetching these until after filter+limit.
		WideColumns: []string{"SpanAttributes", "ResourceAttributes", "Events", "Links"},
		// (Timestamp, TraceId, SpanId) — TraceId+SpanId is the OTel
		// natural key; Timestamp prefix matches the table sort order.
		RowKey: []string{"Timestamp", "TraceId", "SpanId"},
	}
}
