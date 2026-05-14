// Package prom serves the subset of the Prometheus HTTP API that Grafana
// exercises, translating it into cerberus query plans.
package prom

// Prometheus API response wire-format types. Field names + JSON tags match
// the upstream Prometheus HTTP API so Grafana parses our responses without
// any datasource-specific quirks.

// Response is the top-level wrapper for every /api/v1/* response.
// For query endpoints `Data` is a *QueryData; for metadata endpoints
// (`labels`, `label/.../values`, `series`) it's a direct slice.
type Response struct {
	Status    string `json:"status"`              // "success" | "error"
	Data      any    `json:"data,omitempty"`      // nil on errors
	ErrorType string `json:"errorType,omitempty"` // present on errors
	Error     string `json:"error,omitempty"`     // present on errors
}

// QueryData wraps a /api/v1/query or /api/v1/query_range response body.
// Kept as a named type (rather than inlining the fields on Response) so
// metadata-endpoint Data can stay a plain slice without polluting the
// query-endpoint shape.
type QueryData struct {
	ResultType string `json:"resultType"` // "vector" | "matrix" | "scalar" | "string"
	Result     any    `json:"result"`     // shape depends on ResultType
}

// VectorSample is one element of a "vector"-type Result.
type VectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  Sample            `json:"value"` // [timestamp_seconds_float, value_string]
}

// MatrixSample is one element of a "matrix"-type Result.
type MatrixSample struct {
	Metric map[string]string `json:"metric"`
	Values []Sample          `json:"values"`
}

// Sample is the Prometheus on-the-wire representation of one observation:
// [timestamp_in_seconds_as_float, value_as_string]. Prometheus serialises
// numeric values as strings to preserve precision; we match that exactly.
type Sample [2]any

// errorType constants mirror Prometheus's documented error vocabulary.
const (
	ErrBadData   = "bad_data"
	ErrInternal  = "internal"
	ErrTimeout   = "timeout"
	ErrCanceled  = "canceled"
	ErrExecution = "execution"
	// ErrUnavailable is the Prom-vocabulary errorType for HTTP 503
	// responses cerberus emits when its downstream-CH circuit breaker
	// is OPEN. The Prom vocabulary doesn't have a dedicated "downstream
	// out" code; "unavailable" is the closest match in the family.
	ErrUnavailable = "unavailable"
)
