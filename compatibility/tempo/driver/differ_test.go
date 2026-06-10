package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestCompare_Identical(t *testing.T) {
	t.Parallel()
	body := []byte(`{"traces":[
        {"traceID":"abc","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/0","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	d, err := Compare(body, body, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal, got %+v", d)
	}
	if d.MatchedCount != 1 {
		t.Fatalf("MatchedCount = %d, want 1", d.MatchedCount)
	}
}

func TestCompare_TraceIDMismatchSurfaces(t *testing.T) {
	t.Parallel()
	// As of PR #439 cerberus emits the real hex(TraceId), so the differ
	// keys directly on TraceID. Two traces with the same root names but
	// different TraceIDs are no longer canonicalised together — they
	// surface as missing_in_a / missing_in_b. This guards the contract
	// that the differ relies on byte-equal TraceIDs across backends.
	a := []byte(`{"traces":[
        {"traceID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/0","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/0","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatalf("expected Equal=false on differing TraceIDs, got %+v", d)
	}
	foundMissingInA, foundMissingInB := false, false
	for _, r := range d.Reasons {
		if r.Kind == "missing_in_a" && strings.Contains(r.Detail, "bbbbbbbb") {
			foundMissingInA = true
		}
		if r.Kind == "missing_in_b" && strings.Contains(r.Detail, "aaaaaaaa") {
			foundMissingInB = true
		}
	}
	if !foundMissingInA {
		t.Fatalf("expected missing_in_a reason for tempo-only TraceID, got %+v", d.Reasons)
	}
	if !foundMissingInB {
		t.Fatalf("expected missing_in_b reason for cerberus-only TraceID, got %+v", d.Reasons)
	}
}

func TestCompare_CardinalityMismatch(t *testing.T) {
	t.Parallel()
	a := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"checkout","rootTraceName":"R"},
        {"traceID":"b","rootServiceName":"payments","rootTraceName":"R"}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"checkout","rootTraceName":"R"}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false")
	}
	foundCard := false
	foundMissing := false
	for _, r := range d.Reasons {
		if r.Kind == "cardinality" {
			foundCard = true
		}
		if r.Kind == "missing_in_b" {
			foundMissing = true
		}
	}
	if !foundCard {
		t.Fatalf("expected cardinality reason, got %+v", d.Reasons)
	}
	if !foundMissing {
		t.Fatalf("expected missing_in_b reason, got %+v", d.Reasons)
	}
}

func TestCompare_FieldMismatchDuration(t *testing.T) {
	t.Parallel()
	a := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"s","rootTraceName":"n","durationMs":150}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"s","rootTraceName":"n","durationMs":175}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false on duration drift")
	}
	found := false
	for _, r := range d.Reasons {
		if r.Kind == "field_mismatch" && strings.Contains(r.Detail, "durationMs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected durationMs reason, got %+v", d.Reasons)
	}
}

func TestCompare_EpsilonAbsorbsIdenticalFields(t *testing.T) {
	t.Parallel()
	a := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"s","rootTraceName":"n","durationMs":150,"startTimeUnixNano":"1000000"}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"s","rootTraceName":"n","durationMs":150,"startTimeUnixNano":"1000000"}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected equal at identity, got %+v", d.Reasons)
	}
}

func TestCompare_InvalidJSONFails(t *testing.T) {
	t.Parallel()
	a := []byte(`{not json`)
	b := []byte(`{"traces":[]}`)
	if _, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions()); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestTraceKey_UsesRawTraceID(t *testing.T) {
	t.Parallel()
	// The differ keys on the canonical form of TraceID. The root names
	// are not part of the key — only the TraceID is — guaranteeing two
	// traces with the same TraceID align even if their root names
	// differ (which would then surface as a field_mismatch under the
	// match).
	t1 := TraceSummary{TraceID: "00000000000000000000000000000001", RootServiceName: "checkout", RootTraceName: "GET /api/checkout/0"}
	t2 := TraceSummary{TraceID: "00000000000000000000000000000001", RootServiceName: "payments", RootTraceName: "POST /api/payments/0"}
	if traceKey(t1) != traceKey(t2) {
		t.Fatal("expected identical traceKey for identical TraceID")
	}
	t3 := TraceSummary{TraceID: "00000000000000000000000000000002", RootServiceName: "checkout", RootTraceName: "GET /api/checkout/0"}
	if traceKey(t1) == traceKey(t3) {
		t.Fatal("expected different traceKey for different TraceID")
	}
}

// TestTraceKey_CanonicalisesStrippedLeadingZeros pins the
// zero-padding alignment that flattened 17 of 33 compat cases to DIFF:
// cerberus emits the spec-canonical 32-hex-char TraceID (PR #656)
// while reference Tempo strips leading zeros on the wire, so the same
// 16-byte value arrived as `00af…` (32 chars) on one side and `af…`
// (30 chars) on the other and the raw-string key filed it as
// missing_in_a + missing_in_b. Equal values must align; genuinely
// different IDs must still diff.
func TestTraceKey_CanonicalisesStrippedLeadingZeros(t *testing.T) {
	t.Parallel()
	padded := TraceSummary{TraceID: "00af843259b0a78f5cbe59e11cbaf66b"}
	stripped := TraceSummary{TraceID: "af843259b0a78f5cbe59e11cbaf66b"}
	if traceKey(padded) != traceKey(stripped) {
		t.Fatalf("expected stripped and padded forms of the same TraceID to key identically: %q vs %q",
			traceKey(padded), traceKey(stripped))
	}
	other := TraceSummary{TraceID: "01af843259b0a78f5cbe59e11cbaf66b"}
	if traceKey(padded) == traceKey(other) {
		t.Fatal("expected different TraceIDs to key differently")
	}
	upper := TraceSummary{TraceID: "00AF843259B0A78F5CBE59E11CBAF66B"}
	if traceKey(padded) != traceKey(upper) {
		t.Fatal("expected case-insensitive TraceID keying")
	}
}

// TestCompare_StrippedVsPaddedTraceIDsAlign pins the end-to-end
// Compare behaviour for the zero-padding divergence: a search response
// pair where tempo stripped the leading zero and cerberus emitted the
// canonical form must compare Equal.
func TestCompare_StrippedVsPaddedTraceIDsAlign(t *testing.T) {
	t.Parallel()
	tempoBody := []byte(`{"traces":[
        {"traceID":"af843259b0a78f5cbe59e11cbaf66b","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/22","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	cerbBody := []byte(`{"traces":[
        {"traceID":"00af843259b0a78f5cbe59e11cbaf66b","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/22","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	d, err := Compare(tempoBody, cerbBody, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected stripped/padded TraceID forms to align, got %+v", d)
	}
	if d.MatchedCount != 1 {
		t.Fatalf("MatchedCount = %d, want 1", d.MatchedCount)
	}
}

func TestAssertCase_MinTraces(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "t", Query: "x", Endpoint: "search", ExpectedMinTraces: 5}
	body := []byte(`{"traces":[{"traceID":"a","rootServiceName":"s","rootTraceName":"n"}]}`)
	reasons, err := AssertCase(tc, body, "cerberus")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want >= 5") {
		t.Fatalf("expected min-traces reason, got %+v", reasons)
	}
}

func TestAssertCase_MaxTraces(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "t", Query: "x", Endpoint: "search", ExpectedMaxTraces: 1}
	body := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"s","rootTraceName":"n"},
        {"traceID":"b","rootServiceName":"s","rootTraceName":"m"}
    ]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want <= 1") {
		t.Fatalf("expected max-traces reason, got %+v", reasons)
	}
}

func TestAssertCase_ExpectedServicesMet(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "t", Query: "x", Endpoint: "search", ExpectedServices: []string{"checkout"}}
	body := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"checkout","rootTraceName":"n"},
        {"traceID":"b","rootServiceName":"payments","rootTraceName":"m"}
    ]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %+v", reasons)
	}
}

func TestAssertCase_ExpectedServicesMissing(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "t", Query: "x", Endpoint: "search", ExpectedServices: []string{"shipping"}}
	body := []byte(`{"traces":[
        {"traceID":"a","rootServiceName":"checkout","rootTraceName":"n"}
    ]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "shipping") {
		t.Fatalf("expected reason flagging shipping, got %+v", reasons)
	}
}

func TestValuesClose_NaNEqualsNaN(t *testing.T) {
	t.Parallel()
	if !valuesClose(math.NaN(), math.NaN(), DefaultDiffOptions()) {
		t.Fatal("NaN should compare equal to NaN under shadow semantics")
	}
}

func TestCanonicalizeJSON_StableKeyOrder(t *testing.T) {
	t.Parallel()
	a := []byte(`{"z":1,"a":2,"m":{"y":3,"b":4}}`)
	b := []byte(`{"a":2,"m":{"b":4,"y":3},"z":1}`)
	ca, err := canonicalizeJSON(a)
	if err != nil {
		t.Fatalf("canonicalizeJSON(a): %v", err)
	}
	cb, err := canonicalizeJSON(b)
	if err != nil {
		t.Fatalf("canonicalizeJSON(b): %v", err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical forms differ:\n  a=%s\n  b=%s", string(ca), string(cb))
	}
}

func TestCompareTagNames_V1Identical(t *testing.T) {
	t.Parallel()
	a := []byte(`{"tagNames":["a","b","c"]}`)
	b := []byte(`{"tagNames":["c","b","a"]}`) // different order
	d, err := CompareTagNames(a, b, "tempo", "cerberus", false)
	if err != nil {
		t.Fatalf("CompareTagNames: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal on identical sets, got %+v", d)
	}
	if d.MatchedCount != 3 {
		t.Fatalf("MatchedCount = %d, want 3", d.MatchedCount)
	}
}

func TestCompareTagNames_V1MissingInB(t *testing.T) {
	t.Parallel()
	a := []byte(`{"tagNames":["a","b","c"]}`)
	b := []byte(`{"tagNames":["a","b"]}`)
	d, err := CompareTagNames(a, b, "tempo", "cerberus", false)
	if err != nil {
		t.Fatalf("CompareTagNames: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false on differing sets")
	}
	foundMissing := false
	foundCard := false
	for _, r := range d.Reasons {
		if r.Kind == "missing_in_b" && strings.Contains(r.Detail, `"c"`) {
			foundMissing = true
		}
		if r.Kind == "cardinality" {
			foundCard = true
		}
	}
	if !foundMissing {
		t.Errorf("expected missing_in_b reason for 'c', got %+v", d.Reasons)
	}
	if !foundCard {
		t.Errorf("expected cardinality reason, got %+v", d.Reasons)
	}
}

func TestCompareTagNames_V2FlattensScopes(t *testing.T) {
	t.Parallel()
	a := []byte(`{"scopes":[
        {"name":"resource","tags":["service.name"]},
        {"name":"span","tags":["http.method"]},
        {"name":"intrinsic","tags":["name","kind"]}
    ]}`)
	b := []byte(`{"scopes":[
        {"name":"resource","tags":["service.name"]},
        {"name":"span","tags":["http.method"]},
        {"name":"intrinsic","tags":["name","kind"]}
    ]}`)
	d, err := CompareTagNames(a, b, "tempo", "cerberus", true)
	if err != nil {
		t.Fatalf("CompareTagNames: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal, got %+v", d)
	}
}

func TestCompareTagNames_V2ScopeMismatchSurfaces(t *testing.T) {
	t.Parallel()
	// Same tag-names, different scope partition — only happens when the
	// `?scope=` filter is honoured on one side but not the other. The
	// differ reports a scope_mismatch reason in addition to the (zero)
	// tag-name set diff.
	a := []byte(`{"scopes":[
        {"name":"resource","tags":["service.name"]}
    ]}`)
	b := []byte(`{"scopes":[
        {"name":"resource","tags":["service.name"]},
        {"name":"span","tags":[]},
        {"name":"intrinsic","tags":[]}
    ]}`)
	d, err := CompareTagNames(a, b, "tempo", "cerberus", true)
	if err != nil {
		t.Fatalf("CompareTagNames: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false on differing scope partition")
	}
	foundScope := false
	for _, r := range d.Reasons {
		if r.Kind == "scope_mismatch" {
			foundScope = true
		}
	}
	if !foundScope {
		t.Fatalf("expected scope_mismatch reason, got %+v", d.Reasons)
	}
}

func TestCompareTagValues_V1SetDiff(t *testing.T) {
	t.Parallel()
	a := []byte(`{"tagValues":["checkout","payments","search","shipping"]}`)
	b := []byte(`{"tagValues":["checkout","payments"]}`)
	d, err := CompareTagValues(a, b, "tempo", "cerberus", false)
	if err != nil {
		t.Fatalf("CompareTagValues: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false on differing sets")
	}
	count := 0
	for _, r := range d.Reasons {
		if r.Kind == "missing_in_b" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 missing_in_b reasons, got %d (%+v)", count, d.Reasons)
	}
}

func TestCompareTagValues_V2TypeMismatchFlagged(t *testing.T) {
	t.Parallel()
	a := []byte(`{"tagValues":[{"type":"string","value":"checkout"}]}`)
	b := []byte(`{"tagValues":[{"type":"int","value":"checkout"}]}`)
	d, err := CompareTagValues(a, b, "tempo", "cerberus", true)
	if err != nil {
		t.Fatalf("CompareTagValues: %v", err)
	}
	if d.Equal {
		t.Fatal("expected Equal=false on type mismatch")
	}
	found := false
	for _, r := range d.Reasons {
		if r.Kind == "field_mismatch" && strings.Contains(r.Detail, "type") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected type field_mismatch, got %+v", d.Reasons)
	}
}

func TestCompareTagValues_V2EmptyBothSides(t *testing.T) {
	t.Parallel()
	a := []byte(`{"tagValues":[]}`)
	b := []byte(`{"tagValues":[]}`)
	d, err := CompareTagValues(a, b, "tempo", "cerberus", true)
	if err != nil {
		t.Fatalf("CompareTagValues: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal on empty both sides, got %+v", d)
	}
}

func TestAssertCase_TagsV2ExpectedValuesAndScopes(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:           "x",
		Endpoint:       "tags_v2",
		ExpectedValues: []string{"service.name"},
		ExpectedScopes: []string{"resource"},
	}
	body := []byte(`{"scopes":[
        {"name":"resource","tags":["service.name"]}
    ]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %+v", reasons)
	}
}

func TestAssertCase_TagsV2MissingExpectedScope(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:           "x",
		Endpoint:       "tags_v2",
		ExpectedScopes: []string{"resource"},
	}
	body := []byte(`{"scopes":[
        {"name":"span","tags":[]}
    ]}`)
	reasons, err := AssertCase(tc, body, "cerberus")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	found := false
	for _, r := range reasons {
		if strings.Contains(r.Detail, `expected scope "resource"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected scope-missing reason, got %+v", reasons)
	}
}

func TestAssertCase_TagValuesMinValues(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:              "x",
		Endpoint:          "tag_values_v1",
		TagName:           "service.name",
		ExpectedMinValues: 3,
	}
	body := []byte(`{"tagValues":["checkout","payments"]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want >= 3") {
		t.Fatalf("expected min-values reason, got %+v", reasons)
	}
}

func TestAssertCase_TagValuesMaxValues(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:              "x",
		Endpoint:          "tag_values_v2",
		TagName:           "service.name",
		ExpectedMaxValues: 1,
	}
	body := []byte(`{"tagValues":[
        {"type":"string","value":"checkout"},
        {"type":"string","value":"payments"}
    ]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0].Detail, "want <= 1") {
		t.Fatalf("expected max-values reason, got %+v", reasons)
	}
}

func TestAssertCase_TagValuesExpectedValuesMissing(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{
		Name:           "x",
		Endpoint:       "tag_values_v1",
		TagName:        "service.name",
		ExpectedValues: []string{"checkout", "shipping"},
	}
	body := []byte(`{"tagValues":["checkout","payments"]}`)
	reasons, err := AssertCase(tc, body, "tempo")
	if err != nil {
		t.Fatalf("AssertCase: %v", err)
	}
	found := false
	for _, r := range reasons {
		if strings.Contains(r.Detail, `"shipping"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected reason flagging shipping, got %+v", reasons)
	}
}

func TestRenderReport_Roundtrip(t *testing.T) {
	t.Parallel()
	results := []CaseResult{
		{
			Case:           CorpusCase{Name: "pass_case", Endpoint: "search", Query: "{ x = 1 }"},
			TempoStatus:    200,
			CerberusStatus: 200,
			Diff:           Diff{Equal: true, MatchedCount: 3},
		},
		{
			Case:       CorpusCase{Name: "diff_case", Endpoint: "search", Query: "{ y = 2 }"},
			Diff:       Diff{Equal: false, Reasons: []DiffReason{{Kind: "cardinality", Detail: "tempo=5 cerberus=4"}}},
			Assertions: []DiffReason{{Kind: "assertion", Detail: "got 4, want >= 5"}},
		},
		{
			Case:      CorpusCase{Name: "err_case", Endpoint: "search", Query: "{ q = 4 }"},
			HardError: "tempo fetch: connection refused",
		},
	}
	var buf bytes.Buffer
	if err := renderReport(&buf, results); err != nil {
		t.Fatalf("renderReport: %v", err)
	}
	report := buf.String()
	for _, want := range []string{
		"# Tempo / TraceQL compatibility — diff report",
		"## Summary",
		"- Cases: 3",
		"- Passed: 1",
		"- Diff: 1",
		"- Hard errors: 1",
		"### `pass_case`",
		"### `diff_case`",
		"### `err_case`",
		"status: PASS",
		"status: DIFF (1 reasons)",
		"status: ERROR — tempo fetch: connection refused",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}
}

// spanSetsOpts returns DiffOptions with the spanSets comparison
// switched on, as compareForEndpoint does for corpus cases that set
// -- spss --.
func spanSetsOpts() DiffOptions {
	opts := DefaultDiffOptions()
	opts.CompareSpanSets = true
	return opts
}

func TestCompare_SpanSets_Identical(t *testing.T) {
	t.Parallel()
	body := []byte(`{"traces":[
        {"traceID":"abc","rootServiceName":"checkout","rootTraceName":"GET /","durationMs":150,"startTimeUnixNano":"1000",
         "spanSets":[{"spans":[
            {"spanID":"0000000000000001","name":"GET /","startTimeUnixNano":"1000","durationNanos":"150000000"},
            {"spanID":"0000000000000002","name":"db.query","startTimeUnixNano":"2000","durationNanos":"50000000"}
         ],"matched":2}]}
    ]}`)
	d, err := Compare(body, body, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected Equal, got %+v", d)
	}
}

// TestCompare_SpanSets_AsymmetricPresenceFails pins the exact
// regression the spss corpus cases exist for: a backend that returns
// trace summaries WITHOUT spanSets (the pre-fix cerberus shape) must
// diff against reference Tempo, because Grafana's tableType='spans'
// transform renders zero rows for such summaries.
func TestCompare_SpanSets_AsymmetricPresenceFails(t *testing.T) {
	t.Parallel()
	withSets := []byte(`{"traces":[
        {"traceID":"abc","rootServiceName":"checkout","rootTraceName":"GET /","durationMs":150,"startTimeUnixNano":"1000",
         "spanSets":[{"spans":[{"spanID":"0000000000000001","name":"GET /","startTimeUnixNano":"1000","durationNanos":"150000000"}],"matched":1}]}
    ]}`)
	withoutSets := []byte(`{"traces":[
        {"traceID":"abc","rootServiceName":"checkout","rootTraceName":"GET /","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	d, err := Compare(withSets, withoutSets, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatalf("expected diff (cerberus side omitted spanSets), got Equal")
	}
	found := false
	for _, r := range d.Reasons {
		if r.Kind == "field_mismatch" && strings.Contains(r.Detail, "asymmetric presence") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected asymmetric-presence reason, got %+v", d.Reasons)
	}
	// Without the opt-in, the same pair must still compare Equal (the
	// non-spss corpus cases keep their historical comparison surface).
	d, err = Compare(withSets, withoutSets, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("CompareSpanSets off: expected Equal, got %+v", d.Reasons)
	}
}

func TestCompare_SpanSets_MatchedMismatchFails(t *testing.T) {
	t.Parallel()
	a := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"01","startTimeUnixNano":"1000"}],"matched":5}]}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"01","startTimeUnixNano":"1000"}],"matched":3}]}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatalf("expected matched-total diff, got Equal")
	}
}

// TestCompare_SpanSets_CappedComparesCountsOnly: when the spss cap
// truncated both sides (len(spans) < matched), the per-ID subset each
// backend kept is unspecified upstream — equal counts + equal matched
// totals must compare Equal even when the kept spanIDs differ.
func TestCompare_SpanSets_CappedComparesCountsOnly(t *testing.T) {
	t.Parallel()
	a := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"0000000000000001","startTimeUnixNano":"1000"}],"matched":4}]}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"0000000000000002","startTimeUnixNano":"2000"}],"matched":4}]}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("capped sets with equal counts must compare Equal; got %+v", d.Reasons)
	}
}

// TestCompare_SpanSets_CompleteSetsDiffPerSpan: complete sets (matched
// == len(spans)) compare spanID-by-spanID, with leading-zero
// canonicalisation aligning Tempo's stripped form against cerberus's
// fixed-width form, and per-span fields compared numerically.
func TestCompare_SpanSets_CompleteSetsDiffPerSpan(t *testing.T) {
	t.Parallel()
	// Tempo side: stripped spanID + legacy single spanSet field only.
	a := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSet":{"spans":[{"spanID":"1f","name":"GET /","startTimeUnixNano":"1000","durationNanos":"150000000"}],"matched":1}}
    ]}`)
	// Cerberus side: padded spanID, same span — must align.
	b := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"000000000000001f","name":"GET /","startTimeUnixNano":"1000","durationNanos":"150000000"}],"matched":1}]}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("padded vs stripped spanIDs must align; got %+v", d.Reasons)
	}

	// Now flip one durationNanos — must surface as a per-span diff.
	c := []byte(`{"traces":[
        {"traceID":"abc","startTimeUnixNano":"1000","durationMs":150,
         "spanSets":[{"spans":[{"spanID":"000000000000001f","name":"GET /","startTimeUnixNano":"1000","durationNanos":"999"}],"matched":1}]}
    ]}`)
	d, err = Compare(a, c, "tempo", "cerberus", spanSetsOpts())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if d.Equal {
		t.Fatalf("durationNanos divergence must diff, got Equal")
	}
}
