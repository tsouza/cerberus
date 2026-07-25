package migrateverify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
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
