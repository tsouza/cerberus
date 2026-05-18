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
	// Since PR #439, cerberus and Tempo both emit the real hex(TraceId),
	// so the differ keys directly on TraceID. The root names are not
	// part of the key — only the TraceID is — guaranteeing two traces
	// with the same TraceID align even if their root names differ
	// (which would then surface as a field_mismatch under the match).
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
			Case:    CorpusCase{Name: "skip_case", Endpoint: "search", Query: "{ z = 3 }", SkipReason: "pr5"},
			Skipped: true,
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
		"- Cases: 4",
		"- Passed: 1",
		"- Skipped: 1",
		"- Diff: 1",
		"- Hard errors: 1",
		"### `pass_case`",
		"### `diff_case`",
		"### `skip_case`",
		"### `err_case`",
		"status: PASS",
		"status: DIFF (1 reasons)",
		"status: SKIPPED (pr5)",
		"status: ERROR — tempo fetch: connection refused",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n%s", want, report)
		}
	}
}
