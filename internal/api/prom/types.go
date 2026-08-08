// Package prom serves the subset of the Prometheus HTTP API that Grafana
// exercises, translating it into cerberus query plans.
package prom

import "encoding/json"

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
	// Value is the [timestamp_seconds_float, value_string] pair —
	// present for every FLOAT-valued sample, matching upstream
	// Prometheus's `value` key. nil (and omitted from the wire) when
	// Histogram is set instead: the two are mutually exclusive on the
	// wire, matching upstream's marshalSampleJSON (`if s.H == nil {
	// "value" } else { "histogram" }` —
	// prometheus/web/api/v1/json_codec.go).
	Value *Sample `json:"value,omitempty"`
	// Histogram is the [timestamp_seconds_float, {count, sum, buckets}]
	// pair upstream Prometheus emits for a HISTOGRAM-valued sample —
	// `rate()`, `increase()`, `sum()`, or a bare selector over a native
	// (exponential) histogram; see issue #1926. nil today for every
	// query: no lowering produces a histogram-valued
	// chclient.Sample yet (see chclient.Sample.Histogram's doc
	// comment), so this field exists as the shared wire landing spot
	// only, populated by histogramSample once a caller has one.
	Histogram *Sample `json:"histogram,omitempty"`
}

// MatrixSample is one element of a "matrix"-type Result.
type MatrixSample struct {
	Metric map[string]string `json:"metric"`
	// Values holds this series' FLOAT-valued points. Empty (and
	// omitted from the wire) for a series whose points are all
	// histogram-valued, mirroring upstream's marshalSeriesJSON (the
	// `values` key is written only `if len(s.Floats) > 0`).
	Values []Sample `json:"values,omitempty"`
	// Histograms holds this series' HISTOGRAM-valued points — see
	// VectorSample.Histogram. Empty (and omitted) for every query
	// today.
	Histograms []Sample `json:"histograms,omitempty"`
}

// Sample is the Prometheus on-the-wire representation of one observation:
// [timestamp_in_seconds_as_float, value_as_string] for a float-valued
// point, or [timestamp_in_seconds_as_float, *Histogram] for a
// histogram-valued one (the second slot's static type doesn't matter to
// encoding/json — a Histogram marshals as a JSON object in that slot the
// same way a pre-formatted float string marshals as a JSON string).
// Prometheus serialises numeric values as strings to preserve precision;
// we match that exactly.
type Sample [2]any

// Histogram is the Prometheus wire shape for one histogram-valued
// sample's value: `{count, sum, buckets}`, matching upstream's
// util/jsonutil.MarshalHistogram exactly. Count and Sum are
// pre-formatted strings — Prometheus serialises every numeric value as
// a string to preserve precision, the same convention [Sample] already
// follows for the float value slot.
type Histogram struct {
	Count   string            `json:"count"`
	Sum     string            `json:"sum"`
	Buckets []HistogramBucket `json:"buckets,omitempty"`
}

// HistogramBucket is one POPULATED bucket in a Histogram's Buckets list.
// histogramFromValue never appends a zero-count bucket — upstream drops
// them too (jsonutil.MarshalHistogram: `if bucket.Count == 0 { continue
// }`), so a bucket present in Buckets always carries a real count.
type HistogramBucket struct {
	// Boundaries encodes which edges are inclusive, matching upstream's
	// vocabulary exactly: 0 lower-exclusive/upper-inclusive, 1
	// lower-inclusive/upper-exclusive, 2 both exclusive, 3 both
	// inclusive.
	Boundaries int
	Lower      string
	Upper      string
	Count      string
}

// MarshalJSON renders b as upstream's compact 4-element array —
// `[boundaries, lower, upper, count]` — matching
// model.HistogramBucket.MarshalJSON in github.com/prometheus/common
// byte-for-byte in shape (field order and count).
func (b HistogramBucket) MarshalJSON() ([]byte, error) {
	return json.Marshal([4]any{b.Boundaries, b.Lower, b.Upper, b.Count})
}

// BuildInfo is the body of `/api/v1/status/buildinfo`. Mirrors the
// upstream Prometheus `PrometheusVersion` shape (web/api/v1/api.go) so
// Grafana's Prom datasource per-page probe parses cerberus's reply
// without datasource-specific quirks. Wrapped in the standard
// {status, data} envelope by handleBuildInfo.
type BuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

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
