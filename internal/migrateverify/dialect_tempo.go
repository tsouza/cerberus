package migrateverify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// millisPerSecond converts Tempo's integer-millisecond sample timestamps to the
// Unix SECONDS every lane's Sample.T carries. Normalising in the decoder (rather
// than leaving the Tempo lane in milliseconds) keeps one unit across the whole
// report: FirstDiff.Timestamp is printed raw into the text report and the JSON
// diagnostic, so a mixed-unit report would show ts=1.7e9 for one lane and
// ts=1.7e12 for another with nothing telling the operator which is which.
const millisPerSecond = 1000.0

// tempoStatusComplete is the only PartialStatus a trustworthy baseline carries.
// Reference Tempo marshals the enum by NAME and omits it when COMPLETE (the zero
// value), so an EMPTY status also means complete.
const tempoStatusComplete = "COMPLETE"

// jsonpb non-finite float encodings. gogo/protobuf renders a non-finite double
// as one of these quoted tokens rather than a JSON number (JSON has no literal
// for them), so the sample-value decoder must map them back.
const (
	jsonpbNaN       = "NaN"
	jsonpbInf       = "Infinity"
	jsonpbNegInf    = "-Infinity"
	jsonpbTrueText  = "true"
	jsonpbFalseText = "false"
)

// The two internal labels Tempo's metrics engine synthesises from a NUMBER — a
// histogram bucket edge and a quantile phi — and that cerberus's own Tempo head
// re-emits as a pre-formatted string label. They are named here because the two
// sides only key the same series if this decoder renders the reference's
// doubleValue with the same strconv verb the cerberus writer used for that exact
// label, and cerberus uses a different verb for each (see tempoFloatVerb).
const (
	tempoBucketLabel   = "__bucket"
	tempoQuantileLabel = "p"
)

// strconv verbs for a doubleValue label, chosen per label key to mirror
// cerberus's own Tempo head byte-for-byte:
//
//   - tempoBucketFloatVerb ('g') mirrors normalizeHistogramBucketLabels, which
//     writes `__bucket` as strconv.FormatFloat(f, 'g', -1, 64) precisely so a
//     sub-100µs duration edge (1.28e-07) does not read as ClickHouse's
//     "0.000000128" and split one series in two.
//   - tempoScalarFloatVerb ('f') mirrors formatPhi, which writes `p` as
//     strconv.FormatFloat(v, 'f', -1, 64), and ClickHouse's own toString of a
//     grouped Float64 attribute, which is likewise non-exponential.
const (
	tempoBucketFloatVerb = 'g'
	tempoScalarFloatVerb = 'f'
)

// tempoFloatVerb picks the strconv verb that renders a numeric label the way
// cerberus's Tempo head writes that SAME label. A single global verb cannot work:
// cerberus's two numeric-label writers disagree by design, so keying every
// doubleValue with one of them would report every series carrying the other label
// as present-on-one-side-only — a divergence that does not exist.
func tempoFloatVerb(key string) byte {
	if key == tempoBucketLabel {
		return tempoBucketFloatVerb
	}
	return tempoScalarFloatVerb
}

type tempoDialect struct{}

func (tempoDialect) Name() string { return HeadTempo }

func (tempoDialect) Path() string { return tempoQueryRangePath }

// Encode sends the TraceQL query under `q`, not `query`: that is the parameter
// name both reference Tempo and cerberus's Tempo head read, and cerberus 400s a
// request that carries no `q`. Exemplars are never requested — they are not
// comparable (see decodeTempoMetrics).
func (tempoDialect) Encode(expr string, p Params) url.Values {
	q := url.Values{}
	q.Set("q", expr)
	q.Set("start", formatInstantRFC3339(p.Start))
	q.Set("end", formatInstantRFC3339(p.End))
	q.Set("step", formatStep(p.Step))
	return q
}

func (tempoDialect) Decode(body []byte) (RangeResult, error) { return decodeTempoMetrics(body) }

// tempoSearchPath is Tempo's TraceQL search endpoint, served identically by the
// reference and by cerberus's own Tempo head.
const tempoSearchPath = "/api/search"

// traceSearchReplayLimit is the `limit` BOTH sides receive on a trace-search
// replay. It sits AT cerberus's MaxSearchLimit ceiling, which is the highest
// value both backends honour verbatim (above it cerberus clamps, so the two would
// answer different questions) and therefore the value that maximises how many
// searches answer UNDER the limit — the only regime in which set parity is
// decidable at all.
//
// It is pinned rather than exposed as a flag, for the same reason the log-stream
// limit is: the limit decides how much of a result is truncated, i.e. how much
// the gate can judge, and a smaller one hides divergence in the tail.
const traceSearchReplayLimit = 1000

// traceSearchReplaySpansPerSet is the `spss` BOTH sides receive. It sits far
// above Tempo's default of 3 so a realistic trace's matched spanset arrives
// complete and its span MEMBERS are comparable; which spans survive a cap is
// unspecified upstream, so a capped spanset is comparable on counts alone.
const traceSearchReplaySpansPerSet = 100

// TempoSearchDialect speaks Tempo's TraceQL search API: /api/search with the
// query under `q`, limit and spans-per-set pinned, and a trace-summary envelope
// decoded into the flat summary list the trace-search comparator judges.
func TempoSearchDialect() KindDialect { return tempoSearchDialect{} }

type tempoSearchDialect struct{}

func (tempoSearchDialect) Kind() string { return KindTraceSearch }

func (tempoSearchDialect) Head() string { return HeadTempo }

func (tempoSearchDialect) Path(Query) string { return tempoSearchPath }

// Encode sends q/start/end/limit/spss.
//
// start/end are Unix SECONDS, NOT the RFC3339Nano the metrics lane sends: upstream
// Tempo's search request parser accepts only an integer instant here (its own
// client emits strconv.FormatInt seconds), and an RFC3339 value is rejected or
// read as zero. That failure is silent in the worst possible way — an empty
// result on both sides, which the comparator scores as agreement while comparing
// nothing — so the unit is a property of THIS endpoint, not of the head.
//
// A window is always stamped, on both sides: cerberus silently re-bounds a
// windowless search to the last hour, so replaying a corpus expression verbatim
// with no bounds would compare two different questions.
func (tempoSearchDialect) Encode(q Query, p Params) url.Values {
	v := url.Values{}
	v.Set("q", q.Expr)
	v.Set("start", formatTimestamp(p.Start))
	v.Set("end", formatTimestamp(p.End))
	v.Set("limit", strconv.Itoa(traceSearchReplayLimit))
	v.Set("spss", strconv.Itoa(traceSearchReplaySpansPerSet))
	return v
}

// Header sends none: both backends serve /api/search as JSON by default, and a
// per-kind header is a request for a different encoding, which is exactly what
// the log-stream lane learned not to ask for.
func (tempoSearchDialect) Header() http.Header { return nil }

func (tempoSearchDialect) Decode(body []byte) (any, error) { return decodeTraceSearch(body) }

// tempoTraceByIDPathPrefix is the endpoint /api/traces/{id} hangs off, served
// identically by the reference and by cerberus's own Tempo head.
const tempoTraceByIDPathPrefix = "/api/traces/"

// tempoAcceptJSON is the Accept header the trace-by-id probe sends explicitly:
// cerberus's own handler negotiates a protobuf body for
// Accept: application/protobuf (Grafana's datasource plugin), and this lane
// needs the JSON shape every other dialect already reads.
var tempoAcceptJSON = http.Header{"Accept": []string{"application/json"}}

// TempoTraceByIDDialect speaks Tempo's single-trace fetch API:
// GET /api/traces/{id} decoded into the flat span list the trace-by-id
// comparator judges. It is DERIVED-only — no corpus entry carries a trace ID,
// so this dialect is reached only through a Query the trace-search comparator
// produced.
func TempoTraceByIDDialect() KindDialect { return tempoTraceByIDDialect{} }

type tempoTraceByIDDialect struct{}

func (tempoTraceByIDDialect) Kind() string { return KindTraceByID }

func (tempoTraceByIDDialect) Head() string { return HeadTempo }

func (tempoTraceByIDDialect) Path(q Query) string { return tempoTraceByIDPathPrefix + q.TraceID }

// Encode sends start/end as Unix SECONDS, the same unit the search dialect
// uses for the same reason (F6): reference Tempo's trace-by-id lookup also
// takes start/end as an optional search-window HINT in this unit. Cerberus's
// own handler ignores both entirely (a trace-by-id fetch is a direct
// row-by-id lookup, unbounded by any window), so sending them is a no-op on
// that side and a genuine hint on the reference side.
func (tempoTraceByIDDialect) Encode(_ Query, p Params) url.Values {
	v := url.Values{}
	v.Set("start", formatTimestamp(p.Start))
	v.Set("end", formatTimestamp(p.End))
	return v
}

func (tempoTraceByIDDialect) Header() http.Header { return tempoAcceptJSON }

func (tempoTraceByIDDialect) Decode(body []byte) (any, error) { return decodeTraceByID(body) }

// tempoTagsV1Path / tempoTagsV2Path / tempoTagValuesV1PathPrefix /
// tempoTagValuesV2PathPrefix are the four tag/tag-value discovery endpoints,
// served identically by the reference and by cerberus's own Tempo head.
const (
	tempoTagsV1Path            = "/api/search/tags"
	tempoTagsV2Path            = "/api/v2/search/tags"
	tempoTagValuesV1PathPrefix = "/api/search/tag/"
	tempoTagValuesV2PathPrefix = "/api/v2/search/tag/"
)

// Tempo tag-discovery Surface tokens, naming which of the four endpoints a
// KindTagDiscovery Query targets.
const (
	SurfaceTempoTagsV1      = "tags-v1"
	SurfaceTempoTagsV2      = "tags-v2"
	SurfaceTempoTagValuesV1 = "tag-values-v1"
	SurfaceTempoTagValuesV2 = "tag-values-v2"
)

// tempoTagDiscoveryScopeAll requests every scope bucket (resource, span,
// intrinsic) on the v2 tags endpoint. The unfiltered per-scope tag-NAME diff
// is exact and complete on its own — see corpus_tags.go — so this probe is
// never narrowed to a single scope.
const tempoTagDiscoveryScopeAll = "none"

// TempoTagDiscoveryDialect speaks Tempo's four tag/tag-value discovery
// endpoints, selected per Query by Surface (and, for the two tag-VALUES
// surfaces, by TagName).
func TempoTagDiscoveryDialect() KindDialect { return tempoTagDiscoveryDialect{} }

type tempoTagDiscoveryDialect struct{}

func (tempoTagDiscoveryDialect) Kind() string { return KindTagDiscovery }

func (tempoTagDiscoveryDialect) Head() string { return HeadTempo }

func (tempoTagDiscoveryDialect) Path(q Query) string {
	switch q.Surface {
	case SurfaceTempoTagsV2:
		return tempoTagsV2Path
	case SurfaceTempoTagValuesV1:
		return tempoTagValuesV1PathPrefix + url.PathEscape(q.TagName) + "/values"
	case SurfaceTempoTagValuesV2:
		return tempoTagValuesV2PathPrefix + url.PathEscape(q.TagName) + "/values"
	default: // SurfaceTempoTagsV1
		return tempoTagsV1Path
	}
}

// Encode sends start/end as Unix SECONDS (F6, same reasoning as the search
// and trace-by-id dialects) and, for the v2 tag-NAMES surface only,
// scope=none so every scope bucket comes back in one probe.
func (tempoTagDiscoveryDialect) Encode(q Query, p Params) url.Values {
	v := url.Values{}
	v.Set("start", formatTimestamp(p.Start))
	v.Set("end", formatTimestamp(p.End))
	if q.Surface == SurfaceTempoTagsV2 {
		v.Set("scope", tempoTagDiscoveryScopeAll)
	}
	return v
}

func (tempoTagDiscoveryDialect) Header() http.Header { return nil }

// Decode is shape-agnostic: it captures whichever of the four response
// envelopes' fields are present into one tempoDiscoveryPayload, without
// needing to know which Surface asked for it. The comparator, which DOES
// carry the Query and therefore its Surface, reads only the field that
// surface's endpoint populates — see compareTagDiscovery. This is what keeps
// KindDialect.Decode's signature (body only, no Query) sufficient here: an
// empty "tagNames":[] and an absent "scopes" key both decode to a nil slice
// regardless of which endpoint answered, and that ambiguity never matters
// because the comparator never reads the field the surface did not ask for.
func (tempoTagDiscoveryDialect) Decode(body []byte) (any, error) { return decodeTempoDiscovery(body) }

// tempoRangeResponse is the subset of Tempo's metrics range envelope this lane
// reads. Unknown fields (metrics, exemplars, …) are deliberately tolerated: the
// decoder must not reject a reference body for carrying data it does not diff.
type tempoRangeResponse struct {
	Series  []tempoSeries `json:"series"`
	Status  string        `json:"status"`
	Message string        `json:"message"`
}

type tempoSeries struct {
	Labels  []tempoLabel  `json:"labels"`
	Samples []tempoSample `json:"samples"`
}

type tempoSample struct {
	TimestampMs tempoFlexInt64 `json:"timestampMs"`
	Value       tempoFlexFloat `json:"value"`
}

// tempoLabel is one (key, AnyValue) pair. Value is held raw so the AnyValue
// variant can be inspected: the flattener must distinguish a string from a
// double from an unsupported array, and a single typed struct cannot.
type tempoLabel struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// tempoAnyValue is the scalar subset of OTLP AnyValue this lane can flatten into
// a label string. The unsupported variants are carried as raw so their presence
// can be reported by name instead of silently collapsing to "".
type tempoAnyValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
	ArrayValue  json.RawMessage `json:"arrayValue"`
	KvlistValue json.RawMessage `json:"kvlistValue"`
	BytesValue  json.RawMessage `json:"bytesValue"`
}

// tempoFlexInt64 decodes an integer that may arrive as a JSON number or as a
// quoted base-10 integer. Reference Tempo emits the QUOTED form (gogo/protobuf's
// jsonpb quotes every int64 by default) while cerberus's own head emits a plain
// number, so a decoder that accepts only one shape would score every Tempo lane
// as an error on one side.
type tempoFlexInt64 int64

func (v *tempoFlexInt64) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode tempo integer: %w", err)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("parse tempo integer %q: %w", s, err)
		}
		*v = tempoFlexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("decode tempo integer: %w", err)
	}
	*v = tempoFlexInt64(n)
	return nil
}

// tempoFlexFloat decodes a double that may arrive as a JSON number or as one of
// jsonpb's three quoted non-finite tokens. Any other string is an error: a value
// this lane cannot read must fail loudly rather than compare as zero.
type tempoFlexFloat float64

func (v *tempoFlexFloat) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("decode tempo double: %w", err)
		}
		switch s {
		case jsonpbNaN:
			*v = tempoFlexFloat(math.NaN())
		case jsonpbInf:
			*v = tempoFlexFloat(math.Inf(1))
		case jsonpbNegInf:
			*v = tempoFlexFloat(math.Inf(-1))
		default:
			return fmt.Errorf("tempo sample value %q is not a number or a jsonpb non-finite token", s)
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("decode tempo double: %w", err)
	}
	*v = tempoFlexFloat(f)
	return nil
}

// decodeTempoMetrics parses a 200 Tempo metrics-range body into the shared matrix
// shape.
//
// Tempo carries no resultType field, so a successful decode SYNTHESISES
// resultTypeMatrix: without it verifyOne's matrix check would class every TraceQL
// query "unsupported" and the lane would compare nothing while looking green.
//
// A truncated result (status PARTIAL) is an ERROR, not a divergence: the
// reference did not produce a trustworthy baseline, and blaming cerberus for the
// reference's own truncation would manufacture false divergences.
//
// Exemplars are dropped rather than diffed. Cerberus's are best-effort (a second
// SQL query that degrades to empty on failure) and reference Tempo samples its
// own under its own cap, so they are structurally incomparable.
func decodeTempoMetrics(body []byte) (RangeResult, error) {
	var raw tempoRangeResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return RangeResult{}, fmt.Errorf("decode tempo metrics response: %w", err)
	}
	if raw.Status != "" && raw.Status != tempoStatusComplete {
		return RangeResult{}, fmt.Errorf("tempo response status=%q (%s): a truncated result is not a trustworthy baseline", raw.Status, raw.Message)
	}
	res := RangeResult{ResultType: resultTypeMatrix}
	for _, rs := range raw.Series {
		labels, err := flattenTempoLabels(rs.Labels)
		if err != nil {
			return RangeResult{}, err
		}
		s := Series{Labels: labels, Samples: make([]Sample, 0, len(rs.Samples))}
		for _, sm := range rs.Samples {
			s.Samples = append(s.Samples, Sample{
				T: float64(int64(sm.TimestampMs)) / millisPerSecond,
				V: float64(sm.Value),
			})
		}
		res.Series = append(res.Series, s)
	}
	return res, nil
}

// flattenTempoLabels turns Tempo's ordered (key, AnyValue) list into the
// map[string]string the comparator keys series by.
//
// A duplicate key is an ERROR: an ordered list flattened into a map would
// last-wins-collapse two distinct labels into one, silently folding two series
// together. Cerberus can never emit one (its labels come from a Go map), so a
// duplicate is a genuine wire anomaly.
func flattenTempoLabels(labels []tempoLabel) (map[string]string, error) {
	out := make(map[string]string, len(labels))
	for _, l := range labels {
		if _, dup := out[l.Key]; dup {
			return nil, fmt.Errorf("tempo series repeats label key %q: an ordered label list cannot be flattened without collapsing two distinct series", l.Key)
		}
		v, err := tempoLabelValue(l.Key, l.Value)
		if err != nil {
			return nil, err
		}
		out[l.Key] = v
	}
	return out, nil
}

// tempoLabelValue renders one AnyValue as the label string both backends must
// agree on.
//
// doubleValue is rendered with the strconv verb tempoFloatVerb picks for that
// label key — the exact formatter cerberus's own writer for that label uses — so a
// reference `{"doubleValue":0.99}` and a cerberus `{"stringValue":"0.99"}` produce
// the same key and quantile_over_time series align, and a reference
// `{"key":"__bucket","doubleValue":1.28e-07}` keys identically to cerberus's
// `"1.28e-07"`. That normalises an ENCODING difference, not a value difference: a
// genuinely different phi or bucket edge still lands in a different key and
// surfaces as a missing series.
//
// arrayValue / kvlistValue / bytesValue are hard errors naming the variant.
// Collapsing them to "" (as a lenient default branch would) folds distinct series
// together, which is worse than failing.
func tempoLabelValue(key string, raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	// A flat `"value":"str"` — the shape cerberus's own decoder also accepts.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", fmt.Errorf("tempo label %q value: %w", key, err)
		}
		return s, nil
	}
	var av tempoAnyValue
	if err := json.Unmarshal(trimmed, &av); err != nil {
		return "", fmt.Errorf("tempo label %q value: %w", key, err)
	}
	switch {
	case len(av.ArrayValue) > 0:
		return "", fmt.Errorf("tempo label %q carries an arrayValue: this lane compares scalar label values only, and collapsing an array would fold distinct series together", key)
	case len(av.KvlistValue) > 0:
		return "", fmt.Errorf("tempo label %q carries a kvlistValue: this lane compares scalar label values only, and collapsing a kvlist would fold distinct series together", key)
	case len(av.BytesValue) > 0:
		return "", fmt.Errorf("tempo label %q carries a bytesValue: this lane compares scalar label values only, and collapsing bytes would fold distinct series together", key)
	case av.StringValue != nil:
		return *av.StringValue, nil
	case len(av.IntValue) > 0:
		var n tempoFlexInt64
		if err := n.UnmarshalJSON(av.IntValue); err != nil {
			return "", fmt.Errorf("tempo label %q intValue: %w", key, err)
		}
		return strconv.FormatInt(int64(n), 10), nil
	case av.DoubleValue != nil:
		return strconv.FormatFloat(*av.DoubleValue, tempoFloatVerb(key), -1, 64), nil
	case av.BoolValue != nil:
		if *av.BoolValue {
			return jsonpbTrueText, nil
		}
		return jsonpbFalseText, nil
	}
	// An empty AnyValue `{}` — jsonpb's rendering of an unset value.
	return "", nil
}
