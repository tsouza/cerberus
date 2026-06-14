package loki

// Loki HTTP API wire-format types. The shape mirrors Loki's documented
// schema so Grafana parses cerberus's responses without datasource-
// specific quirks.

import (
	"encoding/json"
)

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
// [unix_nanoseconds_string, log_line] or
// [unix_nanoseconds_string, log_line, {structured_metadata}] tuples —
// Loki's documented on-the-wire format. The optional third element
// carries per-entry structured metadata (the OTel-CH LogAttributes map),
// which Grafana's Logs Drilldown reads to render clean per-line columns.
type Stream struct {
	Stream map[string]string `json:"stream"`
	Values []StreamValue     `json:"values"`
}

// StreamValue is one log entry inside a [Stream]: a nanosecond timestamp
// string, the log line, and an optional structured-metadata map. It
// marshals to Loki's positional array shape — a two-element
// `[ts, line]` array when Metadata is empty, or a three-element
// `[ts, line, {metadata}]` array when structured metadata is present —
// so the wire format stays byte-compatible with reference Loki on both
// paths.
type StreamValue struct {
	Timestamp string
	Line      string
	Metadata  map[string]string
}

// MarshalJSON renders the entry as Loki's positional array. The
// structured-metadata object is emitted only when non-empty, keeping
// metadata-free streams (every prior log query, and rows whose
// LogAttributes map is empty) byte-identical to the two-element shape.
func (v StreamValue) MarshalJSON() ([]byte, error) {
	if len(v.Metadata) == 0 {
		return json.Marshal([2]string{v.Timestamp, v.Line})
	}
	return json.Marshal([3]any{v.Timestamp, v.Line, v.Metadata})
}

// UnmarshalJSON parses Loki's positional value array back into the
// struct, accepting both the two-element `[ts, line]` and the
// three-element `[ts, line, {metadata}]` shapes so a round-trip (e.g. a
// conformance test decoding cerberus's own output, or a client reading a
// reference-Loki response) recovers the structured metadata when present.
func (v *StreamValue) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) < 2 {
		return errStreamValueArity
	}
	if err := json.Unmarshal(raw[0], &v.Timestamp); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &v.Line); err != nil {
		return err
	}
	if len(raw) >= 3 {
		return json.Unmarshal(raw[2], &v.Metadata)
	}
	return nil
}

// errStreamValueArity is returned when a Loki stream value array carries
// fewer than the two mandatory [timestamp, line] elements.
var errStreamValueArity = errStreamValue("loki stream value: want at least [timestamp, line]")

type errStreamValue string

func (e errStreamValue) Error() string { return string(e) }

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
