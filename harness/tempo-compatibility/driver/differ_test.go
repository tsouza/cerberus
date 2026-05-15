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

func TestCompare_IgnoresTraceIDDifference(t *testing.T) {
	t.Parallel()
	// Tempo returns a real trace ID; cerberus today returns a synthetic
	// key. The differ canonicalises on (rootServiceName, rootTraceName)
	// so the two should still match.
	a := []byte(`{"traces":[
        {"traceID":"aaaaaaaaaaaaaaaa","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/0","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	b := []byte(`{"traces":[
        {"traceID":"synthetic|key","rootServiceName":"checkout","rootTraceName":"GET /api/checkout/0","durationMs":150,"startTimeUnixNano":"1000"}
    ]}`)
	d, err := Compare(a, b, "tempo", "cerberus", DefaultDiffOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Equal {
		t.Fatalf("expected canonical-key match, got reasons=%v", d.Reasons)
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

func TestCanonicalKey_DeterministicAcrossInvocations(t *testing.T) {
	t.Parallel()
	t1 := TraceSummary{RootServiceName: "checkout", RootTraceName: "GET /api/checkout/0"}
	t2 := TraceSummary{RootServiceName: "checkout", RootTraceName: "GET /api/checkout/0"}
	if CanonicalKey(t1) != CanonicalKey(t2) {
		t.Fatal("expected identical canonical key for identical inputs")
	}
	t3 := TraceSummary{RootServiceName: "checkout", RootTraceName: "GET /api/checkout/1"}
	if CanonicalKey(t1) == CanonicalKey(t3) {
		t.Fatal("expected different canonical keys for different root names")
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
