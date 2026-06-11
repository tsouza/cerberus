package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDiffDetectedFields_Match — identical field sets (order-
// insensitive, parser-order-insensitive) produce no diff.
func TestDiffDetectedFields_Match(t *testing.T) {
	expected := detectedFieldsWire{
		Fields: []detectedFieldWire{
			{Label: "status", Type: "int", Cardinality: 7, Parsers: []string{"json", "logfmt"}, JSONPath: []string{"status"}},
			{Label: "detected_level", Type: "string", Cardinality: 4, Parsers: nil},
		},
		Limit: 1000,
	}
	actual := detectedFieldsWire{
		Fields: []detectedFieldWire{
			{Label: "detected_level", Type: "string", Cardinality: 4, Parsers: nil},
			{Label: "status", Type: "int", Cardinality: 7, Parsers: []string{"logfmt", "json"}, JSONPath: []string{"status"}},
		},
		Limit: 1000,
	}
	if diff := diffDetectedFields(expected, actual); diff != "" {
		t.Fatalf("unexpected diff: %s", diff)
	}
}

// TestDiffDetectedFields_Divergences — every compared dimension
// (presence, type, cardinality, parsers, jsonPath, limit) surfaces in
// the diff string.
func TestDiffDetectedFields_Divergences(t *testing.T) {
	expected := detectedFieldsWire{
		Fields: []detectedFieldWire{
			{Label: "status", Type: "int", Cardinality: 7, Parsers: []string{"json"}, JSONPath: []string{"status"}},
			{Label: "missing", Type: "string", Cardinality: 1, Parsers: nil},
		},
		Limit: 1000,
	}
	actual := detectedFieldsWire{
		Fields: []detectedFieldWire{
			{Label: "status", Type: "string", Cardinality: 9, Parsers: []string{"logfmt"}, JSONPath: nil},
			{Label: "extra", Type: "string", Cardinality: 1, Parsers: nil},
		},
		Limit: 500,
	}
	diff := diffDetectedFields(expected, actual)
	for _, want := range []string{
		`field "missing" missing from test endpoint`,
		`field "status" type: expected="int" actual="string"`,
		`field "status" cardinality: expected=7 actual=9`,
		`field "status" parsers: expected=[json] actual=[logfmt]`,
		`field "status" jsonPath: expected=[status] actual=[]`,
		`field "extra" unexpected on test endpoint`,
		`limit: expected=1000 actual=500`,
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q; got: %s", want, diff)
		}
	}
}

// TestFetchDetectedFields_RejectsEnvelope — a {status, data} envelope
// (the exact bug class this pass exists for) is a hard fetch error,
// not a silent zero-field decode.
func TestFetchDetectedFields_RejectsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"fields":[{"label":"a","type":"string","cardinality":1}],"limit":1000}}`))
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchDetectedFields(c, srv.URL, `{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err == nil {
		t.Fatal("expected envelope rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("error should name the envelope: %v", err)
	}
}

// TestFetchDetectedFields_DecodesBareShape — the bare upstream shape
// decodes with fields + limit intact and the line_limit/window params
// reach the backend.
func TestFetchDetectedFields_DecodesBareShape(t *testing.T) {
	var gotQuery, gotLineLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotLineLimit = r.URL.Query().Get("line_limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fields":[{"label":"status","type":"int","cardinality":7,"parsers":["logfmt"]}],"limit":1000}`))
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Timeout: 5 * time.Second}
	body, err := fetchDetectedFields(c, srv.URL, `{service_name="api"}`, time.Unix(0, 0), time.Unix(60, 0))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotQuery != `{service_name="api"}` {
		t.Errorf("query param: %q", gotQuery)
	}
	if gotLineLimit != "2000" {
		t.Errorf("line_limit param: %q want 2000", gotLineLimit)
	}
	if len(body.Fields) != 1 || body.Fields[0].Label != "status" || body.Limit != 1000 {
		t.Errorf("decoded body: %+v", body)
	}
}
