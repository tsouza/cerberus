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
//
// EncodingFlags echoes the response-encoding flags the client requested
// via the `X-Loki-Response-Encoding-Flags` header. It is set to
// `["categorize-labels"]` ONLY when the client asked for it AND the
// result is a metadata-bearing stream — Grafana's Loki datasource always
// requests it, and its shared Prometheus-style response parser
// (`promlib/converter.ReadPrometheusStyleResult`) switches to the
// categorized-stream reader exactly when the response carries this field.
// A plain client (the loki-compat harness, curl) that omits the header
// gets no `encodingFlags` and the byte-identical two-element value shape
// reference Loki returns. Omitted (nil) → field absent from the JSON.
type QueryData struct {
	ResultType    string   `json:"resultType"`              // "streams" | "matrix" | "vector"
	EncodingFlags []string `json:"encodingFlags,omitempty"` // e.g. ["categorize-labels"]
	Result        any      `json:"result"`                  // shape depends on ResultType
}

// encodingFlagCategorizeLabels is the single response-encoding flag
// cerberus honours: when the client sends it via
// `X-Loki-Response-Encoding-Flags`, structured metadata rides each stream
// value as a categorized `{structuredMetadata: {...}}` third element and
// the response advertises the flag back so Grafana's parser takes the
// categorized-stream branch. Mirrors reference Loki's
// `loghttp.LabelsCategorizationFlag`.
const encodingFlagCategorizeLabels = "categorize-labels"

// Stream is one element of a "streams"-type Result. Values are
// [unix_nanoseconds_string, log_line] tuples by default, or
// [unix_nanoseconds_string, log_line, {"structuredMetadata": {...}}]
// tuples when the client requested `categorize-labels`. The categorized
// third element carries per-entry structured metadata (the OTel-CH
// LogAttributes map) which Grafana's Logs Drilldown reads to render clean
// per-line columns — see [StreamValue.MarshalJSON] for the exact shape
// each path emits.
type Stream struct {
	Stream map[string]string `json:"stream"`
	Values []StreamValue     `json:"values"`
}

// StreamValue is one log entry inside a [Stream]: a nanosecond timestamp
// string, the log line, and an optional structured-metadata map. Its
// marshalled shape depends on Categorize:
//
//   - Categorize == false (default — plain clients, the loki-compat
//     harness, and every non-categorize query): a two-element
//     `[ts, line]` array, byte-identical to reference Loki's default
//     wire format. Metadata is NOT surfaced; without the categorize
//     request a non-Loki-aware parser (Grafana's shared Prometheus-style
//     converter on the `readStream` branch) rejects a bare third map
//     element with `ReadArray: expect [ or , or ] or n, but found {`.
//   - Categorize == true: ALWAYS a three-element
//     `[ts, line, {"structuredMetadata": {...}}]` array — the categorized
//     shape reference Loki returns under `X-Loki-Response-Encoding-Flags:
//     categorize-labels`, which Grafana's `readCategorizedStream` parser
//     reads to render structured-metadata columns in Logs Drilldown. The
//     third element is mandatory for EVERY value once the response
//     advertises the flag: Grafana's parser
//     (`pkg/promlib/converter.readCategorizedStream`) unconditionally
//     reads `[ts, line, {…}]` — after the line it calls `iter.ReadArray()`
//     then `readCategorizedStreamField`, so a two-element value (closing
//     `]` where the object is expected) 400s with
//     `ReadObject: expect { or , or } or n, but found ]`. When this row
//     carries no attributes the object is the empty
//     `{"structuredMetadata": {}}` — the converter reads a missing /
//     empty `structuredMetadata` as an empty label set, so the row simply
//     surfaces no extra columns.
type StreamValue struct {
	Timestamp string
	Line      string
	Metadata  map[string]string
	// Categorize gates the categorized three-element marshalling. Set by
	// the handler from the request's `X-Loki-Response-Encoding-Flags`
	// header so the wire shape matches what the client's parser expects.
	Categorize bool
}

// categorizedValue is the third element of a categorized stream value:
// `{"structuredMetadata": {...}}`. Grafana's `readCategorizedStreamField`
// reads the `structuredMetadata` (and `parsed`) sub-objects to tag each
// surfaced key with its label type ("S" / "P"). Cerberus's OTel-CH
// LogAttributes are all structured metadata; the `parsed` slot is omitted
// (parser-stage extraction isn't surfaced here). The field has NO
// `omitempty`: under categorize-labels the key must always be present so
// the converter's `iter.ReadVal(&structuredMetadata)` finds a `{}` (an
// empty label set) rather than the unexpected closing `]` of a bare
// two-element tuple.
type categorizedValue struct {
	StructuredMetadata map[string]string `json:"structuredMetadata"`
}

// MarshalJSON renders the entry as Loki's positional array.
//
//   - Default (Categorize == false): a two-element `[ts, line]` array —
//     byte-identical to reference Loki's wire format, the shape every
//     non-categorize-labels client (the loki-compat harness, curl)
//     expects and Grafana's plain `readStream` parser accepts.
//   - Categorize == true: ALWAYS the three-element
//     `[ts, line, {"structuredMetadata": {...}}]` array reference Loki
//     emits under the flag. The object is emitted for EVERY value even
//     when this row has no attributes (an empty `{}` map) — Grafana's
//     `readCategorizedStream` reads the third element unconditionally, so
//     a two-element tuple under the advertised flag 400s its parser.
func (v StreamValue) MarshalJSON() ([]byte, error) {
	if !v.Categorize {
		return json.Marshal([2]string{v.Timestamp, v.Line})
	}
	meta := v.Metadata
	if meta == nil {
		// A nil map marshals to JSON `null`; the converter's
		// `iter.ReadVal(&data.Labels)` wants an object. Emit `{}` so the
		// empty-metadata row stays a well-formed categorized tuple.
		meta = map[string]string{}
	}
	return json.Marshal([3]any{
		v.Timestamp,
		v.Line,
		categorizedValue{StructuredMetadata: meta},
	})
}

// UnmarshalJSON parses Loki's positional value array back into the
// struct, accepting the two-element `[ts, line]` shape, the categorized
// three-element `[ts, line, {"structuredMetadata": {...}}]` shape, and the
// legacy flat `[ts, line, {metadata}]` shape so a round-trip (a
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
		// Prefer the categorized `{"structuredMetadata": {...}}` envelope;
		// fall back to a flat `{key: value}` map for legacy callers.
		var cat categorizedValue
		if err := json.Unmarshal(raw[2], &cat); err == nil && cat.StructuredMetadata != nil {
			v.Metadata = cat.StructuredMetadata
			v.Categorize = true
			return nil
		}
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
