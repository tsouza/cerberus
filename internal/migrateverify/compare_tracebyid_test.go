package migrateverify

import (
	"encoding/base64"
	"strings"
	"testing"
)

// traceByIDQuery is one derived trace-by-id probe, mirroring what
// compareTraceSearch's Derived actually produces.
func traceByIDQuery(traceID string) Query {
	return Query{
		Expr: "GET /api/traces/" + traceID, Source: "derived-from:panel:traces",
		Head: HeadTempo, Lang: "traceql", Kind: KindTraceByID, TraceID: traceID,
	}
}

func decodeTraceByIDResult(t *testing.T, body string) traceByIDResult {
	t.Helper()
	decoded, err := decodeTraceByID([]byte(body))
	if err != nil {
		t.Fatalf("decodeTraceByID: %v", err)
	}
	res, ok := decoded.(traceByIDResult)
	if !ok {
		t.Fatalf("decodeTraceByID returned %T, want traceByIDResult", decoded)
	}
	return res
}

// refTraceBody is a matching pair's REFERENCE-shaped body: the protojson
// envelope nests under resourceSpans/scopeSpans, IDs are hex here for
// readability (canonicalHexOrB64ID's base64 path is pinned separately —
// see TestCanonicalHexOrB64ID), attributes are the typed KeyValue array, kind
// and status are the proto enum names.
const refTraceBody = `{"resourceSpans":[{"resource":{"attributes":[` +
	`{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[` +
	`{"traceId":"0000000000000000000000000000a1b2","spanId":"00000000000000c3","parentSpanId":"",` +
	`"name":"GET /cart","kind":"SPAN_KIND_SERVER","startTimeUnixNano":"1000000000","endTimeUnixNano":"1000000100",` +
	`"status":{"code":"STATUS_CODE_OK"},` +
	`"attributes":[{"key":"http.status_code","value":{"intValue":"200"}}]}]}]}]}`

// cerTraceBody is the SAME trace in cerberus's own flat shape: batches[].spans[]
// directly, hex IDs, the CH-literal kind/status vocabulary, durationNanos as a
// bare int, and flat string-valued attribute maps.
const cerTraceBody = `{"batches":[{"resource":{"attributes":{"service.name":"checkout"}},"spans":[` +
	`{"traceId":"0000000000000000000000000000a1b2","spanId":"00000000000000c3","parentSpanId":"",` +
	`"name":"GET /cart","kind":"Server","startTimeUnixNano":"1000000000","durationNanos":100,` +
	`"status":{"code":"Ok"},"attributes":{"http.status_code":"200"}}]}]}`

func TestCompareTraceByID_Match(t *testing.T) {
	ref := decodeTraceByIDResult(t, refTraceBody)
	cer := decodeTraceByIDResult(t, cerTraceBody)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (first-diff: %+v)", out.Verdict, out.FirstDiff)
	}
	if out.Compared != 1 {
		t.Errorf("Compared = %d, want 1 (the one span field-diffed)", out.Compared)
	}
	if len(out.Limitations) != 0 {
		t.Errorf("Limitations = %+v, want none: every attribute here was comparable", out.Limitations)
	}
}

func TestCompareTraceByID_EmptyVsEmptyIsNotProvenParity(t *testing.T) {
	ref := decodeTraceByIDResult(t, `{"batches":[]}`)
	cer := decodeTraceByIDResult(t, `{"batches":[]}`)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (two empty traces do agree)", out.Verdict)
	}
	if out.Compared != 0 {
		t.Errorf("Compared = %d, want 0: nothing was diffed", out.Compared)
	}
}

func TestCompareTraceByID_FieldDiffers(t *testing.T) {
	cerWrongName := strings.Replace(cerTraceBody, `"name":"GET /cart"`, `"name":"GET /checkout"`, 1)
	ref := decodeTraceByIDResult(t, refTraceBody)
	cer := decodeTraceByIDResult(t, cerWrongName)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.Field != "name" || out.FirstDiff.ReasonCode != ReasonSpanFieldDiffers {
		t.Errorf("FirstDiff = %+v, want field=name reason=%s", out.FirstDiff, ReasonSpanFieldDiffers)
	}
	if out.FirstDiff.RefValue != "GET /cart" || out.FirstDiff.CerberusValue != "GET /checkout" {
		t.Errorf("FirstDiff values = ref=%q cerberus=%q, want the two names", out.FirstDiff.RefValue, out.FirstDiff.CerberusValue)
	}
}

func TestCompareTraceByID_StatusAndKindVocabulariesNormalize(t *testing.T) {
	// refTraceBody says SPAN_KIND_SERVER / STATUS_CODE_OK; cerTraceBody says
	// Server / Ok — different vocabularies for the SAME value. A match here
	// pins that the normalisation, not a lucky string coincidence, is what
	// makes them agree.
	ref := decodeTraceByIDResult(t, refTraceBody)
	cer := decodeTraceByIDResult(t, cerTraceBody)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: SPAN_KIND_SERVER/Server and STATUS_CODE_OK/Ok name the same value", out.Verdict)
	}
}

func TestCompareTraceByID_MissingAttributeKeyStillDiverges(t *testing.T) {
	cerNoAttr := strings.Replace(cerTraceBody, `"attributes":{"http.status_code":"200"}`, `"attributes":{}`, 1)
	ref := decodeTraceByIDResult(t, refTraceBody)
	cer := decodeTraceByIDResult(t, cerNoAttr)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: cerberus dropped a key the reference has", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.Field != "attributes.http.status_code" {
		t.Errorf("FirstDiff = %+v, want field=attributes.http.status_code", out.FirstDiff)
	}
}

func TestCompareTraceByID_NonComparableAttributeValueIsCountedNotDiverged(t *testing.T) {
	// The reference's http.status_code is a doubleValue this time: the OTel-CH
	// carrier cannot round-trip it, so its VALUE must not be diffed — but the
	// KEY is still present on both sides, so the run still matches.
	refDouble := strings.Replace(refTraceBody,
		`"attributes":[{"key":"http.status_code","value":{"intValue":"200"}}]`,
		`"attributes":[{"key":"http.status_code","value":{"doubleValue":200.5}}]`, 1)
	ref := decodeTraceByIDResult(t, refDouble)
	cer := decodeTraceByIDResult(t, cerTraceBody)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match: a non-comparable-type attribute value must not diverge", out.Verdict)
	}
	lim := limitationByCode(t, out.Limitations, LimitSpanAttrValueType)
	if lim.Count != 1 {
		t.Errorf("LimitSpanAttrValueType count = %d, want 1", lim.Count)
	}
}

func TestCompareTraceByID_SpanReturnedByOneBackendOnlyDiverges(t *testing.T) {
	refExtra := strings.Replace(refTraceBody,
		`"attributes":[{"key":"http.status_code","value":{"intValue":"200"}}]}]}]}]}`,
		`"attributes":[{"key":"http.status_code","value":{"intValue":"200"}}]},`+
			`{"traceId":"0000000000000000000000000000a1b2","spanId":"00000000000000c4","name":"child",`+
			`"startTimeUnixNano":"1000000050","endTimeUnixNano":"1000000060"}]}]}]}`, 1)
	ref := decodeTraceByIDResult(t, refExtra)
	cer := decodeTraceByIDResult(t, cerTraceBody)
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: reference has a span cerberus does not", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.ReasonCode != ReasonSpanMissing || out.FirstDiff.SpanID != "00000000000000c4" {
		t.Errorf("FirstDiff = %+v, want the extra span named by ID with reason=%s", out.FirstDiff, ReasonSpanMissing)
	}
}

// TestCompareTraceByID_DuplicateSpanSameIDDivergesOnSpanCount pins the
// trailing span-count check: indexTraceByIDSpans keys spans by SpanID with
// last-write-wins, so a literal duplicate span (a double-ingest /
// duplicate-batch bug) leaves the ID SET unaffected — both sides still key
// down to the same one span ID, so the field-diff loop and both exclusive-ID
// checks all see agreement — but the raw span COUNT is not: cerberus here
// returns the very same span twice while the reference returns it once. Only
// the trailing `len(ref.Spans) != len(cer.Spans)` check can catch this, and a
// regression that dropped or reordered that check ahead of the exclusive-ID
// checks would silently turn a real double-emit defect into VerdictMatch.
func TestCompareTraceByID_DuplicateSpanSameIDDivergesOnSpanCount(t *testing.T) {
	const cerSpan = `{"traceId":"0000000000000000000000000000a1b2","spanId":"00000000000000c3","parentSpanId":"",` +
		`"name":"GET /cart","kind":"Server","startTimeUnixNano":"1000000000","durationNanos":100,` +
		`"status":{"code":"Ok"},"attributes":{"http.status_code":"200"}}`
	cerDuplicate := strings.Replace(cerTraceBody, cerSpan+`]}]}`, cerSpan+`,`+cerSpan+`]}]}`, 1)

	ref := decodeTraceByIDResult(t, refTraceBody)
	cer := decodeTraceByIDResult(t, cerDuplicate)
	if len(cer.Spans) != 2 {
		t.Fatalf("fixture sanity: decoded %d cerberus spans, want 2 (the duplicate must survive decode)", len(cer.Spans))
	}
	out := compareTraceByID(traceByIDQuery("0000000000000000000000000000a1b2"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: cerberus returned the same span ID twice, a real double-emit "+
			"the ID-set comparison alone cannot see (first-diff: %+v)", out.Verdict, out.FirstDiff)
	}
	if out.FirstDiff == nil || out.FirstDiff.Field != "span-count" || out.FirstDiff.ReasonCode != ReasonTraceFieldDiffers {
		t.Errorf("FirstDiff = %+v, want field=span-count reason=%s", out.FirstDiff, ReasonTraceFieldDiffers)
	}
	if out.FirstDiff.RefValue != "1" || out.FirstDiff.CerberusValue != "2" {
		t.Errorf("FirstDiff span counts = ref=%q cerberus=%q, want 1 vs 2", out.FirstDiff.RefValue, out.FirstDiff.CerberusValue)
	}
}

func TestCompareTraceByID_PayloadMismatchIsAnError(t *testing.T) {
	out := compareTraceByID(traceByIDQuery("x"), "not a traceByIDResult", traceByIDResult{}, Params{})
	if out.Verdict != VerdictError {
		t.Errorf("verdict = %q, want error on a wired-to-the-wrong-dialect payload", out.Verdict)
	}
}

func TestDecodeTraceByID_AcceptsBothEnvelopeKeys(t *testing.T) {
	viaBatches := decodeTraceByIDResult(t, cerTraceBody)
	viaResourceSpans := decodeTraceByIDResult(t, refTraceBody)
	if len(viaBatches.Spans) != 1 || len(viaResourceSpans.Spans) != 1 {
		t.Fatalf("want exactly 1 span decoded from each envelope, got %d and %d", len(viaBatches.Spans), len(viaResourceSpans.Spans))
	}
	if viaBatches.Spans[0].SpanID != viaResourceSpans.Spans[0].SpanID {
		t.Errorf("span IDs disagree across envelopes: %q vs %q", viaBatches.Spans[0].SpanID, viaResourceSpans.Spans[0].SpanID)
	}
}

// TestDecodeTraceByID_CanonicalizesRealBase64SpanIDs exercises the FULL
// decode → buildTraceByIDSpan pipeline against a body whose span/parent IDs
// are genuinely base64-encoded (reference Tempo's actual protojson bytes
// encoding — see canonicalJSONSpanID in
// compatibility/tempo/driver/proto_fetch.go), not the hex-for-readability IDs
// every other fixture in this file uses. It exists specifically to catch a
// wrong byte-length argument at the buildTraceByIDSpan call site: passing a
// HEX-CHARACTER count where canonicalHexOrB64ID expects a BYTE count makes the
// base64 branch's length check fail silently, leaving the ID lowercased but
// never hex-decoded — which every hex-only fixture is blind to, because
// hex.DecodeString's fallback happens to look identical to "do nothing" on
// hex input.
func TestDecodeTraceByID_CanonicalizesRealBase64SpanIDs(t *testing.T) {
	spanIDBytes := []byte{0xc9, 0xd5, 0xa3, 0x14, 0xdd, 0xef, 0x21, 0x2b}
	parentIDBytes := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	wantSpanHex := "c9d5a314ddef212b"
	wantParentHex := "1122334455667788"

	body := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"traceId":"0000000000000000000000000000a1b2",` +
		`"spanId":"` + base64.StdEncoding.EncodeToString(spanIDBytes) + `",` +
		`"parentSpanId":"` + base64.StdEncoding.EncodeToString(parentIDBytes) + `",` +
		`"name":"GET /cart","startTimeUnixNano":"1000000000","endTimeUnixNano":"1000000100"}]}]}]}`

	res := decodeTraceByIDResult(t, body)
	if len(res.Spans) != 1 {
		t.Fatalf("want exactly 1 span, got %d", len(res.Spans))
	}
	sp := res.Spans[0]
	if sp.SpanID != wantSpanHex {
		t.Errorf("SpanID = %q, want %q (the base64 bytes, hex-encoded)", sp.SpanID, wantSpanHex)
	}
	if sp.ParentSpanID != wantParentHex {
		t.Errorf("ParentSpanID = %q, want %q (the base64 bytes, hex-encoded)", sp.ParentSpanID, wantParentHex)
	}
}

func TestDecodeTraceByID_RejectsUnusableBody(t *testing.T) {
	if _, err := decodeTraceByID([]byte("not json")); err == nil {
		t.Error("want a decode error on unparseable JSON")
	}
}

// TestCanonicalHexOrB64ID_DisambiguatesHexVsBase64 pins the exact-byte-length
// rule canonicalHexOrB64ID uses: a canonical-width hex string is ALSO valid
// base64 but decodes to a different byte count, so trying hex first with an
// exact length check is what keeps the two from being confused.
func TestCanonicalHexOrB64ID_DisambiguatesHexVsBase64(t *testing.T) {
	const spanIDBytes = 8
	raw := []byte{0xc9, 0xd5, 0xa3, 0x14, 0xdd, 0xef, 0x21, 0x2b}
	hexForm := "c9d5a314ddef212b"
	b64Form := base64.StdEncoding.EncodeToString(raw) // "ydWjFN3vISs="

	if got := canonicalHexOrB64ID(hexForm, spanIDBytes); got != hexForm {
		t.Errorf("canonicalHexOrB64ID(hex) = %q, want %q unchanged", got, hexForm)
	}
	if got := canonicalHexOrB64ID(b64Form, spanIDBytes); got != hexForm {
		t.Errorf("canonicalHexOrB64ID(base64) = %q, want %q (the decoded bytes, hex-encoded)", got, hexForm)
	}
	if got := canonicalHexOrB64ID("", spanIDBytes); got != "" {
		t.Errorf("canonicalHexOrB64ID(\"\") = %q, want empty (no parent = root span)", got)
	}
}

func TestNormalizeSpanKindAndStatusCode(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) string
		in   string
		want string
	}{
		{"CH literal", normalizeSpanKind, "Server", "SPAN_KIND_SERVER"},
		{"proto enum name", normalizeSpanKind, "SPAN_KIND_CLIENT", "SPAN_KIND_CLIENT"},
		{"empty CH literal", normalizeSpanKind, "", spanKindUnspecified},
		{"unrecognised CH literal", normalizeSpanKind, "Bogus", spanKindUnspecified},
		{"status CH literal", normalizeStatusCode, "Error", "STATUS_CODE_ERROR"},
		{"status proto enum name", normalizeStatusCode, "STATUS_CODE_OK", "STATUS_CODE_OK"},
		{"status empty", normalizeStatusCode, "", statusCodeUnset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
