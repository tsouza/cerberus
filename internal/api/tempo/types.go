package tempo

// Tempo HTTP API wire-format types. The shapes mirror Tempo's
// documented schema so Grafana parses cerberus's responses without
// datasource-specific quirks.

// SearchResponse is the body of `GET /api/search`. Tempo's `traces`
// array contains one trace summary per match; `metrics` carries
// aggregate counts (cerberus reports an empty metrics block until the
// per-search aggregate plumbing lands in RC2).
type SearchResponse struct {
	Traces  []TraceSummary `json:"traces"`
	Metrics SearchMetrics  `json:"metrics,omitempty"`
}

// TraceSummary is one element of SearchResponse.Traces. Field names
// match Tempo's documented schema (camelCase JSON, even though Tempo's
// internal Go type uses snake_case in some places).
type TraceSummary struct {
	TraceID           string `json:"traceID"`
	RootServiceName   string `json:"rootServiceName,omitempty"`
	RootTraceName     string `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`
	DurationMs        int    `json:"durationMs,omitempty"`
}

// SearchMetrics is Tempo's per-search aggregate block. Cerberus
// reports zeros until the aggregate plumbing lands in RC2; the shape
// is here so the response stays parseable by Grafana.
type SearchMetrics struct {
	InspectedTraces int `json:"inspectedTraces"`
	InspectedBytes  int `json:"inspectedBytes"`
	TotalBlocks     int `json:"totalBlocks"`
}

// TraceByIDResponse is the body of `GET /api/traces/<id>`. Tempo
// returns a Resource Spans envelope with one entry per service
// contributing to the trace; cerberus collapses that to a flat list of
// span rows (Grafana's trace-view tolerates either shape).
type TraceByIDResponse struct {
	Batches []ResourceSpans `json:"batches"`
}

// ResourceSpans is one envelope entry containing the resource attribute
// map and a flat span list (no scope-spans nesting in cerberus's shape).
type ResourceSpans struct {
	Resource Resource    `json:"resource"`
	Spans    []SpanEntry `json:"spans"`
}

// Resource carries the per-service identity attributes pulled from
// otel_traces.ResourceAttributes.
type Resource struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

// SpanEntry is one span row in TraceByIDResponse.
type SpanEntry struct {
	TraceID           string            `json:"traceId"`
	SpanID            string            `json:"spanId"`
	ParentSpanID      string            `json:"parentSpanId,omitempty"`
	Name              string            `json:"name,omitempty"`
	Kind              string            `json:"kind,omitempty"`
	StartTimeUnixNano string            `json:"startTimeUnixNano,omitempty"`
	DurationNanos     int64             `json:"durationNanos,omitempty"`
	Status            SpanStatus        `json:"status,omitempty"`
	Attributes        map[string]string `json:"attributes,omitempty"`
}

// SpanStatus mirrors the OTel status block (Code: "Unset" / "Ok" /
// "Error", optional Message).
type SpanStatus struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ErrorResponse mirrors Tempo's distinct error shape:
//
//	{"traceID":"","spanID":"","error":true,"message":"..."}
//
// Used for `/api/traces/<id>` lookups that come back empty as well as
// for handler-level validation failures, so Grafana renders the right
// "trace not found" UI rather than a generic JSON error.
type ErrorResponse struct {
	TraceID string `json:"traceID"`
	SpanID  string `json:"spanID"`
	Error   bool   `json:"error"`
	Message string `json:"message"`
}

// EchoResponse is the body of `GET /api/echo`. Tempo returns the literal
// string "echo" — used by Grafana's datasource health-check.
type EchoResponse string

// VersionResponse is the body of `GET /api/status/version`. Tempo
// returns build metadata; cerberus surfaces its own version string.
type VersionResponse struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	Branch    string `json:"branch,omitempty"`
	BuildUser string `json:"buildUser,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
}

// SearchTagsV1Response is the body of `GET /api/search/tags` — the
// v1 shape that returns a flat list of tag names available across
// the indexed spans. Used by Grafana's TraceQL Search UI for the tag
// autocomplete dropdown.
type SearchTagsV1Response struct {
	TagNames []string `json:"tagNames"`
}

// SearchTagsV2Response is the body of `GET /api/v2/search/tags` —
// the v2 shape Grafana 11+ uses. Each scope groups tags by where
// they live: `intrinsic` (cerberus's TraceQL keywords), `resource`
// (ResourceAttributes), `span` (SpanAttributes).
type SearchTagsV2Response struct {
	Scopes []TagScope `json:"scopes"`
}

// TagScope is one entry in SearchTagsV2Response.Scopes.
type TagScope struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// SearchTagValuesV1Response is the body of
// `GET /api/search/tag/<name>/values` — flat list of values seen for
// the named tag.
type SearchTagValuesV1Response struct {
	TagValues []string `json:"tagValues"`
}

// SearchTagValuesV2Response is the body of
// `GET /api/v2/search/tag/<name>/values` — list of typed values used
// by Grafana 11+.
type SearchTagValuesV2Response struct {
	TagValues []TagValue `json:"tagValues"`
}

// TagValue is one entry in SearchTagValuesV2Response.TagValues. Type
// is one of `string`, `int`, `float`, `bool`, `duration`, `status`,
// `kind`.
type TagValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
