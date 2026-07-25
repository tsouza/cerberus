package migrateverify

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// maxDerivedTraceFetchesPerSearch bounds how many trace-by-id fetches one
// trace-search probe spawns, so a corpus of broad searches cannot turn one
// verify run into thousands of round-trips. IDs are taken in the trace-search
// comparator's own deterministic sort order (see compareTraceSearch), so the
// same run always derives the same fetches.
const maxDerivedTraceFetchesPerSearch = 5

// spanIDByteLen is a span/parent-span ID's length in raw BYTES — half of
// canonicalSpanIDHexLen (compare_tracesearch.go), which counts HEX
// CHARACTERS. canonicalHexOrB64ID's disambiguation is byte-length-exact (a
// canonical-width hex string is also valid base64 but decodes to a different
// byte count), so passing the hex-character count here instead of this would
// silently defeat that check and never hex-encode a genuine base64 ID.
const spanIDByteLen = canonicalSpanIDHexLen / 2

// spanKindUnspecified / statusCodeUnset are the canonical proto enum NAMES
// this comparator normalises both backends' encodings onto. Reference Tempo's
// protojson omits a zero-value enum field entirely (proto3 JSON convention);
// cerberus's own CH-literal columns are similarly empty/unrecognised for the
// same "no value" case. Both collapse to the same canonical token here, so an
// absent field on one side and an explicit unspecified/unset on the other are
// never reported as a divergence that does not exist.
const (
	spanKindUnspecified = "SPAN_KIND_UNSPECIFIED"
	statusCodeUnset     = "STATUS_CODE_UNSET"
)

// normalizeSpanKindCH maps cerberus's CH SpanKind string literal (the
// LowCardinality(String) value the OTel-CH exporter writes: "Internal" /
// "Server" / "Client" / "Producer" / "Consumer", or empty/unrecognised) onto
// the canonical proto enum name reference Tempo's protojson emits. This is
// the same table internal/api/tempo/handler_trace_proto.go's spanKindFromCH
// encodes for cerberus's OWN proto responses; migrateverify cannot import
// that package (architecture boundary — see .go-arch-lint.yml), so the table
// is re-declared here rather than reached into via an unexported symbol.
func normalizeSpanKindCH(s string) string {
	switch s {
	case "Internal":
		return "SPAN_KIND_INTERNAL"
	case "Server":
		return "SPAN_KIND_SERVER"
	case "Client":
		return "SPAN_KIND_CLIENT"
	case "Producer":
		return "SPAN_KIND_PRODUCER"
	case "Consumer":
		return "SPAN_KIND_CONSUMER"
	default:
		return spanKindUnspecified
	}
}

// normalizeSpanKindRef normalises the reference's already-canonical enum
// name: proto3 JSON omits the zero value, so an empty field means
// unspecified, exactly as a missing CH literal does on the cerberus side.
func normalizeSpanKindRef(s string) string {
	if s == "" {
		return spanKindUnspecified
	}
	return s
}

// normalizeStatusCodeCH maps cerberus's CH StatusCode string literal
// ("Unset" / "Ok" / "Error", or empty/unrecognised) onto the canonical proto
// enum name, mirroring internal/api/tempo/handler_trace_proto.go's
// statusCodeFromCH for the same architecture-boundary reason as
// normalizeSpanKindCH.
func normalizeStatusCodeCH(s string) string {
	switch s {
	case "Ok":
		return "STATUS_CODE_OK"
	case "Error":
		return "STATUS_CODE_ERROR"
	default:
		return statusCodeUnset
	}
}

// normalizeStatusCodeRef mirrors normalizeSpanKindRef for the status enum.
func normalizeStatusCodeRef(s string) string {
	if s == "" {
		return statusCodeUnset
	}
	return s
}

// traceByIDSpan is one flattened span from a trace-by-id fetch: its identity,
// the fields the comparator judges, and its FULL attribute set (resource +
// span, merged — the batch partition carries no information the compared
// dimensions need, so flattening loses nothing).
//
// AttrValues carries only the COMPARABLE renderings: every key on cerberus's
// side (its Map(String,String) carrier is always string-typed) and, on the
// reference side, only the keys whose AnyValue variant is stringValue /
// intValue / boolValue — a deterministic textual round-trip. A key present in
// AttrKeys but absent from AttrValues is one whose VALUE this comparator
// declines to judge (see LimitSpanAttrValueType); its KEY is still compared.
type traceByIDSpan struct {
	SpanID            string
	ParentSpanID      string
	Name              string
	Kind              string
	StartTimeUnixNano int64
	DurationNanos     int64
	StatusCode        string
	StatusMessage     string
	AttrKeys          map[string]struct{}
	AttrValues        map[string]string
}

// traceByIDResult is one backend's decoded /api/traces/{id} response: every
// span in the trace, flattened out of its batch.
type traceByIDResult struct {
	Spans []traceByIDSpan
}

// traceByIDSpanJSON is one span as either backend renders it. StartTimeUnixNano
// / EndTimeUnixNano / DurationNanos share traceFlexInt64 (defined alongside the
// trace-search decoder) because they arrive quoted on the reference (64-bit
// proto3 JSON) and bare on cerberus (a plain Go int64 in TraceByIDResponse).
type traceByIDSpanJSON struct {
	SpanID            string              `json:"spanId"`
	ParentSpanID      string              `json:"parentSpanId"`
	Name              string              `json:"name"`
	Kind              string              `json:"kind"`
	StartTimeUnixNano traceFlexInt64      `json:"startTimeUnixNano"`
	EndTimeUnixNano   traceFlexInt64      `json:"endTimeUnixNano"`
	DurationNanos     traceFlexInt64      `json:"durationNanos"`
	Status            traceByIDStatusJSON `json:"status"`
	Attributes        json.RawMessage     `json:"attributes"`
}

type traceByIDStatusJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// traceByIDScopeSpansJSON is one scope-spans bucket, the reference's extra
// nesting level cerberus's flat "spans" list skips.
type traceByIDScopeSpansJSON struct {
	Spans []traceByIDSpanJSON `json:"spans"`
}

// traceByIDBatchJSON is one batch (ResourceSpans) entry. Cerberus populates
// Spans directly; the reference nests one level deeper under ScopeSpans (and,
// on older proto builds, InstrumentationLibrarySpans) — mirroring
// compatibility/tempo/driver/proto_fetch.go's tolerance of both shapes.
type traceByIDBatchJSON struct {
	Resource struct {
		Attributes json.RawMessage `json:"attributes"`
	} `json:"resource"`
	Spans                       []traceByIDSpanJSON       `json:"spans"`
	ScopeSpans                  []traceByIDScopeSpansJSON `json:"scopeSpans"`
	InstrumentationLibrarySpans []traceByIDScopeSpansJSON `json:"instrumentationLibrarySpans"`
}

// traceByIDEnvelope tolerates both top-level keys a trace-by-id body can
// carry: cerberus's own "batches", and "resourceSpans" (the field name
// reference Tempo's protojson sometimes top-keys with instead, depending on
// protojson version) — the same dual-key tolerance
// compatibility/tempo/driver/proto_fetch.go's projectJSONShape documents.
type traceByIDEnvelope struct {
	Batches       []traceByIDBatchJSON `json:"batches"`
	ResourceSpans []traceByIDBatchJSON `json:"resourceSpans"`
}

// decodeTraceByID parses a 200 /api/traces/{id} body into the flat span list
// the comparator works on.
func decodeTraceByID(body []byte) (any, error) {
	var raw traceByIDEnvelope
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode trace-by-id response: %w", err)
	}
	batches := raw.Batches
	if len(batches) == 0 {
		batches = raw.ResourceSpans
	}

	var spans []traceByIDSpan
	for _, b := range batches {
		resKeys, resVals, err := decodeTraceByIDAttrs(b.Resource.Attributes)
		if err != nil {
			return nil, err
		}
		var spanJSONs []traceByIDSpanJSON
		spanJSONs = append(spanJSONs, b.Spans...)
		for _, ss := range b.ScopeSpans {
			spanJSONs = append(spanJSONs, ss.Spans...)
		}
		for _, ss := range b.InstrumentationLibrarySpans {
			spanJSONs = append(spanJSONs, ss.Spans...)
		}
		for _, sj := range spanJSONs {
			span, err := buildTraceByIDSpan(sj, resKeys, resVals)
			if err != nil {
				return nil, err
			}
			spans = append(spans, span)
		}
	}
	return traceByIDResult{Spans: spans}, nil
}

// buildTraceByIDSpan merges one span's own attributes with its batch's
// resource attributes into the single flattened key/value set the comparator
// judges, and normalises identity, kind, status and duration.
func buildTraceByIDSpan(sj traceByIDSpanJSON, resKeys map[string]struct{}, resVals map[string]string) (traceByIDSpan, error) {
	spanKeys, spanVals, err := decodeTraceByIDAttrs(sj.Attributes)
	if err != nil {
		return traceByIDSpan{}, err
	}

	keys := make(map[string]struct{}, len(resKeys)+len(spanKeys))
	vals := make(map[string]string, len(resVals)+len(spanVals))
	for k := range resKeys {
		keys[k] = struct{}{}
	}
	for k, v := range resVals {
		vals[k] = v
	}
	for k := range spanKeys {
		keys[k] = struct{}{}
	}
	for k, v := range spanVals {
		vals[k] = v
	}

	start := int64(sj.StartTimeUnixNano)
	end := int64(sj.EndTimeUnixNano)
	duration := int64(sj.DurationNanos)
	if duration == 0 && end > start {
		// The reference's Span carries start/end, not a duration field;
		// cerberus's SpanEntry carries duration, not end. Deriving one from the
		// other is lossless: both express the same interval.
		duration = end - start
	}

	return traceByIDSpan{
		SpanID:            canonicalHexOrB64ID(sj.SpanID, spanIDByteLen),
		ParentSpanID:      canonicalHexOrB64ID(sj.ParentSpanID, spanIDByteLen),
		Name:              sj.Name,
		Kind:              normalizeSpanKind(sj.Kind),
		StartTimeUnixNano: start,
		DurationNanos:     duration,
		StatusCode:        normalizeStatusCode(sj.Status.Code),
		StatusMessage:     sj.Status.Message,
		AttrKeys:          keys,
		AttrValues:        vals,
	}, nil
}

// normalizeSpanKind picks the CH-literal or already-canonical normaliser: a
// CH literal is a plain title-case word ("Internal", "Server", …); a
// reference proto enum name always carries the SPAN_KIND_ prefix. An empty
// string is ambiguous between "cerberus emitted no kind" and "the reference
// omitted the zero value", and both normalisers agree on unspecified for it.
func normalizeSpanKind(s string) string {
	if hasProtoEnumPrefix(s, "SPAN_KIND_") {
		return normalizeSpanKindRef(s)
	}
	return normalizeSpanKindCH(s)
}

// normalizeStatusCode is normalizeSpanKind's sibling for the status enum.
func normalizeStatusCode(s string) string {
	if hasProtoEnumPrefix(s, "STATUS_CODE_") {
		return normalizeStatusCodeRef(s)
	}
	return normalizeStatusCodeCH(s)
}

func hasProtoEnumPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// decodeTraceByIDAttrs decodes one attributes blob, which arrives as a flat
// map[string]string on cerberus (its Map(String,String) carrier) or as a
// []KeyValue{key, value:AnyValue} list on the reference. It returns every
// key seen (keys) and, separately, only the keys whose value this comparator
// can round-trip as a string (vals) — see LimitSpanAttrValueType.
func decodeTraceByIDAttrs(raw json.RawMessage) (keys map[string]struct{}, vals map[string]string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]struct{}{}, map[string]string{}, nil
	}
	if trimmed[0] == '{' {
		var m map[string]string
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, nil, fmt.Errorf("decode trace-by-id attributes (flat map form): %w", err)
		}
		keys = make(map[string]struct{}, len(m))
		vals = make(map[string]string, len(m))
		for k, v := range m {
			keys[k] = struct{}{}
			vals[k] = v
		}
		return keys, vals, nil
	}
	var kvs []refKeyValueJSON
	if err := json.Unmarshal(trimmed, &kvs); err != nil {
		return nil, nil, fmt.Errorf("decode trace-by-id attributes (typed KeyValue form): %w", err)
	}
	keys = make(map[string]struct{}, len(kvs))
	vals = make(map[string]string, len(kvs))
	for _, kv := range kvs {
		keys[kv.Key] = struct{}{}
		if v, ok := kv.Value.comparable(); ok {
			vals[kv.Key] = v
		}
	}
	return keys, vals, nil
}

// refKeyValueJSON is one OTLP common.v1.KeyValue entry as reference Tempo's
// protojson renders it.
type refKeyValueJSON struct {
	Key   string          `json:"key"`
	Value refAnyValueJSON `json:"value"`
}

// refAnyValueJSON is the OTLP common.v1.AnyValue oneof, shared with the
// metrics-lane label decoder's tempoAnyValue shape (dialect_tempo.go) — kept
// as its own type here because trace-by-id reads the raw/typed distinction
// itself (comparable), where the metrics lane only ever renders a value.
type refAnyValueJSON struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	BoolValue   *bool           `json:"boolValue"`
	DoubleValue *float64        `json:"doubleValue"`
	ArrayValue  json.RawMessage `json:"arrayValue"`
	KvlistValue json.RawMessage `json:"kvlistValue"`
	BytesValue  json.RawMessage `json:"bytesValue"`
}

// comparable renders v as a string when its type round-trips deterministically
// (string / int / bool), or reports ok=false for a type the OTel-CH carrier
// erases (double / array / kvlist / bytes) — see LimitSpanAttrValueType.
func (v refAnyValueJSON) comparable() (string, bool) {
	switch {
	case v.StringValue != nil:
		return *v.StringValue, true
	case len(v.IntValue) > 0:
		var n traceFlexInt64
		if err := n.UnmarshalJSON(v.IntValue); err != nil {
			return "", false
		}
		return fmt.Sprintf("%d", int64(n)), true
	case v.BoolValue != nil:
		if *v.BoolValue {
			return jsonpbTrueText, true
		}
		return jsonpbFalseText, true
	default:
		return "", false
	}
}

// canonicalHexOrB64ID normalises a span/parent-span ID to its canonical
// lowercase-hex form of byteLen*2 characters. Hex is tried first: a
// canonical-width hex string is also valid base64 but decodes to a different
// byte count, so the exact byteLen check disambiguates — mirroring
// compatibility/tempo/driver/proto_fetch.go's canonicalJSONSpanID. Anything
// that decodes as neither passes through unchanged (lowercased) so a
// genuinely malformed ID still diffs loudly rather than silently
// disappearing. An empty string (no parent = root span) passes through
// unchanged.
func canonicalHexOrB64ID(s string, byteLen int) string {
	if s == "" {
		return ""
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == byteLen {
		return strings.ToLower(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == byteLen {
		return hex.EncodeToString(b)
	}
	return strings.ToLower(s)
}

// missingAttrValue is what a first-diff prints on the side that did not
// carry an attribute key, mirroring the matrix lane's "<missing series>".
const missingAttrValue = "<missing attribute>"

// compareTraceByID judges one trace-by-id fetch.
//
// The parity rule, in the operator's words: a trace is a SET OF SPANS, not a
// sequence and not a batch layout, so span order and batch partition are
// never compared. Two backends agree on a trace when they return the same
// span-ID set, the same total span count (so a duplicate on one side
// surfaces), and, for every span, the same name / kind / parent / start /
// duration / status, and the same attribute KEY set. An attribute's VALUE is
// compared only when the reference's type round-trips deterministically
// through cerberus's always-string carrier (string / int / bool); a
// double / array / kvlist / bytes value is counted, never diffed — see
// LimitSpanAttrValueType — but its KEY is still compared, so a genuinely
// missing attribute is still a divergence.
//
// Field diffs are judged before the total-count check, mirroring the
// trace-search comparator's ordering: a specific, named field bug is always
// the more useful signal, and a count mismatch despite matching ID sets
// (fewer distinct IDs than total spans) is the LEAST specific signal —
// evidence of a duplicate, reported only once nothing more specific was found.
func compareTraceByID(q Query, refAny, cerAny any, _ Params) Outcome {
	ref, refOK := refAny.(traceByIDResult)
	cer, cerOK := cerAny.(traceByIDResult)
	if !refOK || !cerOK {
		return Outcome{
			Verdict: VerdictError,
			Detail:  "trace-by-id comparator received a payload it cannot read; the lane is wired to the wrong dialect",
		}
	}

	refByID := indexTraceByIDSpans(ref.Spans)
	cerByID := indexTraceByIDSpans(cer.Spans)
	both := commonSpanIDs(refByID, cerByID)

	out := Outcome{Verdict: VerdictMatch, Compared: len(both)}
	nonComparableAttrs := 0
	for _, id := range both {
		r, c := refByID[id], cerByID[id]
		if fd := diffTraceByIDSpanFields(q.TraceID, id, r, c); fd != nil {
			out.Verdict, out.FirstDiff = VerdictDiverge, fd
			return out
		}
		n, fd := diffTraceByIDAttrs(q.TraceID, id, r, c)
		nonComparableAttrs += n
		if fd != nil {
			out.Verdict, out.FirstDiff = VerdictDiverge, fd
			return out
		}
	}
	if nonComparableAttrs > 0 {
		out.Limitations = append(out.Limitations, limitation(LimitSpanAttrValueType, nonComparableAttrs))
	}

	if onlyRef := exclusiveSpanIDs(refByID, cerByID); len(onlyRef) > 0 {
		out.Verdict = VerdictDiverge
		out.FirstDiff = missingTraceByIDSpanDiff(q.TraceID, onlyRef[0], "reference only")
		return out
	}
	if onlyCer := exclusiveSpanIDs(cerByID, refByID); len(onlyCer) > 0 {
		out.Verdict = VerdictDiverge
		out.FirstDiff = missingTraceByIDSpanDiff(q.TraceID, onlyCer[0], "cerberus only")
		return out
	}
	if len(ref.Spans) != len(cer.Spans) {
		out.Verdict = VerdictDiverge
		out.FirstDiff = &FirstDiff{
			Kind: KindTraceByID, TraceID: q.TraceID, Field: "span-count",
			RefValue: strconv.Itoa(len(ref.Spans)), CerberusValue: strconv.Itoa(len(cer.Spans)),
			Reason:     "trace span count differs despite matching span-ID sets (likely a duplicate span on one side)",
			ReasonCode: ReasonTraceFieldDiffers,
		}
	}
	return out
}

// diffTraceByIDSpanFields returns the first scalar field two same-span
// records disagree on, or nil when they agree on every one.
func diffTraceByIDSpanFields(traceID, spanID string, r, c traceByIDSpan) *FirstDiff {
	fields := []traceSummaryField{
		{"name", r.Name, c.Name},
		{"kind", r.Kind, c.Kind},
		{"parentSpanId", r.ParentSpanID, c.ParentSpanID},
		{"startTimeUnixNano", strconv.FormatInt(r.StartTimeUnixNano, 10), strconv.FormatInt(c.StartTimeUnixNano, 10)},
		{"durationNanos", strconv.FormatInt(r.DurationNanos, 10), strconv.FormatInt(c.DurationNanos, 10)},
		{"status.code", r.StatusCode, c.StatusCode},
		{"status.message", r.StatusMessage, c.StatusMessage},
	}
	for _, f := range fields {
		if f.ref != f.cerberus {
			return &FirstDiff{
				Kind: KindTraceByID, TraceID: traceID, SpanID: spanID, Field: f.name,
				RefValue: f.ref, CerberusValue: f.cerberus,
				Reason: "span field differs", ReasonCode: ReasonSpanFieldDiffers,
			}
		}
	}
	return nil
}

// diffTraceByIDAttrs compares one span pair's flattened attribute set: every
// key is checked for presence on both sides, and every key whose value is
// comparable on BOTH sides is value-diffed. It returns the number of keys
// this call declined to value-diff (present on both, non-comparable on at
// least one side) alongside the first divergence found, if any.
func diffTraceByIDAttrs(traceID, spanID string, r, c traceByIDSpan) (int, *FirstDiff) {
	keys := make([]string, 0, len(r.AttrKeys)+len(c.AttrKeys))
	seen := make(map[string]struct{}, len(r.AttrKeys)+len(c.AttrKeys))
	for k := range r.AttrKeys {
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for k := range c.AttrKeys {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	nonComparable := 0
	for _, k := range keys {
		_, inRef := r.AttrKeys[k]
		_, inCer := c.AttrKeys[k]
		switch {
		case !inRef:
			return nonComparable, attrFieldDiff(traceID, spanID, k, missingAttrValue, presentAttrMarker(c, k),
				"attribute present in cerberus only")
		case !inCer:
			return nonComparable, attrFieldDiff(traceID, spanID, k, presentAttrMarker(r, k), missingAttrValue,
				"attribute present in reference only")
		}
		rv, rOK := r.AttrValues[k]
		cv, cOK := c.AttrValues[k]
		if !rOK || !cOK {
			// cerberus's carrier is always string-typed, so a non-comparable
			// value here is necessarily the reference's typed AnyValue — see
			// LimitSpanAttrValueType.
			nonComparable++
			continue
		}
		if rv != cv {
			return nonComparable, attrFieldDiff(traceID, spanID, k, rv, cv, "attribute value differs")
		}
	}
	return nonComparable, nil
}

// presentAttrMarker renders the present side's value for an attribute one
// side lacks: the actual comparable string when there is one, or a marker
// naming that its type could not be rendered.
func presentAttrMarker(s traceByIDSpan, key string) string {
	if v, ok := s.AttrValues[key]; ok {
		return v
	}
	return "<non-comparable type>"
}

// attrFieldDiff anchors an attribute-set divergence at the exact span and key.
func attrFieldDiff(traceID, spanID, key, refVal, cerVal, reason string) *FirstDiff {
	return &FirstDiff{
		Kind: KindTraceByID, TraceID: traceID, SpanID: spanID, Field: "attributes." + key,
		RefValue: refVal, CerberusValue: cerVal, Reason: reason, ReasonCode: ReasonSpanFieldDiffers,
	}
}

// indexTraceByIDSpans keys a trace's spans by canonical span ID. A repeated
// ID within one response last-wins here; it is the SPAN-COUNT check in
// compareTraceByID, not this index, that surfaces the duplicate — collapsing
// silently at this layer would otherwise make a genuine double-emit
// invisible to every dimension this comparator judges.
func indexTraceByIDSpans(spans []traceByIDSpan) map[string]traceByIDSpan {
	out := make(map[string]traceByIDSpan, len(spans))
	for _, s := range spans {
		out[s.SpanID] = s
	}
	return out
}

// commonSpanIDs returns the span IDs both backends returned, sorted, so the
// "first" difference this comparator reports is stable across runs.
func commonSpanIDs(a, b map[string]traceByIDSpan) []string {
	out := make([]string, 0, len(a))
	for id := range a {
		if _, ok := b[id]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// exclusiveSpanIDs returns the span IDs in have that lacks does not carry, sorted.
func exclusiveSpanIDs(have, lacks map[string]traceByIDSpan) []string {
	var out []string
	for id := range have {
		if _, ok := lacks[id]; !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// missingTraceByIDSpanDiff anchors a divergence on a span exactly one backend
// returned.
func missingTraceByIDSpanDiff(traceID, spanID, presentSide string) *FirstDiff {
	refVal, cerVal := missingSpanValue, missingSpanValue
	if presentSide == "reference only" {
		refVal = "<present>"
	} else {
		cerVal = "<present>"
	}
	return &FirstDiff{
		Kind: KindTraceByID, TraceID: traceID, SpanID: spanID,
		RefValue: refVal, CerberusValue: cerVal,
		Reason: fmt.Sprintf("span present in %s", presentSide), ReasonCode: ReasonSpanMissing,
	}
}
