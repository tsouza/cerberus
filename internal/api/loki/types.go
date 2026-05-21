package loki

// Loki HTTP API wire-format types. The shape mirrors Loki's documented
// schema so Grafana parses cerberus's responses without datasource-
// specific quirks.

// Response is the top-level wrapper for every /loki/api/v1/* response.
// Data shape varies by endpoint (QueryData for /query{,_range}, plain
// slices for /labels and /label/<name>/values).
type Response struct {
	Status    string `json:"status"`              // "success" | "error"
	Data      any    `json:"data,omitempty"`      // nil on errors
	ErrorType string `json:"errorType,omitempty"` // present on errors
	Error     string `json:"error,omitempty"`     // present on errors
}

// QueryData wraps a /loki/api/v1/query or /loki/api/v1/query_range body.
// ResultType is "streams" for raw log-line queries, or "matrix" /
// "vector" for the LogQL metric form (rate, count_over_time, ...).
type QueryData struct {
	ResultType string `json:"resultType"` // "streams" | "matrix" | "vector"
	Result     any    `json:"result"`     // shape depends on ResultType
}

// Stream is one element of a "streams"-type Result. Values are
// [unix_nanoseconds_string, log_line] tuples — Loki's documented
// on-the-wire format.
type Stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// MatrixSample is one element of a "matrix"-type Result. Values are
// [seconds_float, value_string] tuples — same convention as Prometheus
// for the metric form.
type MatrixSample struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

// VectorSample is one element of a "vector"-type Result.
type VectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// BuildInfo is the body of `/loki/api/v1/status/buildinfo`. Mirrors
// the upstream Loki `BuildInfo` shape (pkg/ui/cluster.go). Unlike the
// `{status, data}` envelope the rest of the Loki API uses, the
// buildinfo response is a flat top-level JSON object — see
// docs/sources/reference/loki-http-api.md "Show build information"
// — so Grafana's Loki datasource per-page probe can decode it without
// peeling an extra layer.
type BuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// errorType constants mirror Loki's documented error vocabulary, which
// is itself aligned with Prometheus's.
const (
	ErrBadData   = "bad_data"
	ErrInternal  = "internal"
	ErrTimeout   = "timeout"
	ErrCanceled  = "canceled"
	ErrExecution = "execution"
	// ErrUnavailable is the Loki-vocabulary errorType for HTTP 503
	// responses cerberus emits when its downstream-CH circuit breaker
	// is OPEN. Mirrors prom.ErrUnavailable for consistency.
	ErrUnavailable = "unavailable"
)
