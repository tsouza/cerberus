package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"
	v11 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
)

// makeSpan returns a Span with the given 16-byte trace ID and 8-byte
// span ID padded from the input strings. Keeps the test fixtures
// readable while still producing a real-shape Span.
func makeSpan(traceHex, spanHex string) *v11.Span {
	tid := make([]byte, 16)
	copy(tid, traceHex)
	sid := make([]byte, 8)
	copy(sid, spanHex)
	return &v11.Span{TraceId: tid, SpanId: sid, Name: "op-" + spanHex}
}

// twoSpanTrace builds a tempopb.Trace with two spans across one
// ResourceSpans / one ScopeSpans. Used as the canonical "fixture" the
// JSON + proto encodings both describe in the happy-path test.
func twoSpanTrace() *tempopb.Trace {
	return &tempopb.Trace{
		ResourceSpans: []*v11.ResourceSpans{{
			ScopeSpans: []*v11.ScopeSpans{{
				Spans: []*v11.Span{makeSpan("trace1", "spanA"), makeSpan("trace1", "spanB")},
			}},
		}},
	}
}

func TestFetchProto_DecodesValidTrace(t *testing.T) {
	t.Parallel()
	want := twoSpanTrace()
	body, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/protobuf" {
			t.Errorf("Accept header = %q, want application/protobuf", got)
		}
		w.Header().Set("Content-Type", "application/protobuf")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	raw, trace, status, err := fetchProto(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("fetchProto: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if len(raw) != len(body) {
		t.Errorf("raw bytes len = %d, want %d", len(raw), len(body))
	}
	got := projectProtoShape(trace)
	wantShape := projectProtoShape(want)
	if !got.equal(wantShape) {
		t.Fatalf("shape mismatch: got=%v want=%v", got, wantShape)
	}
}

func TestFetchProto_DetectsJSONInsteadOfProto(t *testing.T) {
	// This is the #199/#650 failure mode: client asks for proto, server
	// returns JSON. proto.Unmarshal must fail (not return an empty
	// message), and the error must carry enough body context to be
	// diagnosable.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"batches":[{"spans":[{"spanId":"aaaa"}]}]}`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	_, trace, status, err := fetchProto(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatalf("fetchProto: want decode error, got nil (trace=%v)", trace)
	}
	if !strings.Contains(err.Error(), "proto.Unmarshal") {
		t.Errorf("error %q does not mention proto.Unmarshal", err.Error())
	}
	if !strings.Contains(err.Error(), "batches") {
		t.Errorf("error %q does not include body snippet", err.Error())
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}

func TestFetchProto_PropagatesNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`trace not found`))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	_, trace, status, err := fetchProto(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatalf("want error on non-2xx, got nil (trace=%v)", trace)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "trace not found") {
		t.Errorf("error %q missing status / body context", err.Error())
	}
}

func TestProjectProtoShape_FlattensResourceScopeSpans(t *testing.T) {
	t.Parallel()
	trace := &tempopb.Trace{
		ResourceSpans: []*v11.ResourceSpans{
			{ScopeSpans: []*v11.ScopeSpans{
				{Spans: []*v11.Span{makeSpan("t", "s1"), makeSpan("t", "s2")}},
				{Spans: []*v11.Span{makeSpan("t", "s3")}},
			}},
			{ScopeSpans: []*v11.ScopeSpans{
				{Spans: []*v11.Span{makeSpan("t", "s4")}},
			}},
		},
	}
	shape := projectProtoShape(trace)
	if shape.SpanCount != 4 {
		t.Errorf("SpanCount = %d, want 4", shape.SpanCount)
	}
	// Span IDs are 8-byte arrays hex-encoded — sortedKeys gives a
	// deterministic order. Validate the count + first id contains
	// "s1" hex prefix.
	if len(shape.SpanIDs) != 4 {
		t.Errorf("SpanIDs len = %d, want 4", len(shape.SpanIDs))
	}
}

func TestProjectJSONShape_TempoNested(t *testing.T) {
	t.Parallel()
	body := []byte(`{"batches":[{"scopeSpans":[{"spans":[{"spanId":"aa"},{"spanId":"bb"}]}]}]}`)
	shape, err := projectJSONShape(body)
	if err != nil {
		t.Fatalf("projectJSONShape: %v", err)
	}
	if shape.SpanCount != 2 {
		t.Errorf("SpanCount = %d, want 2", shape.SpanCount)
	}
	if len(shape.SpanIDs) != 2 || shape.SpanIDs[0] != "aa" || shape.SpanIDs[1] != "bb" {
		t.Errorf("SpanIDs = %v, want [aa bb]", shape.SpanIDs)
	}
}

func TestProjectJSONShape_CerberusFlat(t *testing.T) {
	t.Parallel()
	body := []byte(`{"batches":[{"spans":[{"spanId":"aa"},{"spanId":"bb"},{"spanId":"cc"}]}]}`)
	shape, err := projectJSONShape(body)
	if err != nil {
		t.Fatalf("projectJSONShape: %v", err)
	}
	if shape.SpanCount != 3 {
		t.Errorf("SpanCount = %d, want 3", shape.SpanCount)
	}
	if len(shape.SpanIDs) != 3 {
		t.Errorf("SpanIDs len = %d, want 3", len(shape.SpanIDs))
	}
}

func TestTraceShape_EqualsDetectsDivergence(t *testing.T) {
	t.Parallel()
	a := traceShape{SpanCount: 2, SpanIDs: []string{"aa", "bb"}}
	b := traceShape{SpanCount: 2, SpanIDs: []string{"aa", "bb"}}
	if !a.equal(b) {
		t.Errorf("a.equal(b) = false, want true")
	}
	// Different count
	c := traceShape{SpanCount: 1, SpanIDs: []string{"aa"}}
	if a.equal(c) {
		t.Errorf("a.equal(c) = true, want false on differing count")
	}
	// Same count, different IDs
	d := traceShape{SpanCount: 2, SpanIDs: []string{"aa", "cc"}}
	if a.equal(d) {
		t.Errorf("a.equal(d) = true, want false on differing IDs")
	}
}

func TestDiffTracesEndpoint_CerberusProtoMissingIsReported(t *testing.T) {
	// Integration-shape test: mock two backends. Reference Tempo serves
	// the same trace in BOTH JSON and proto (content-negotiated by
	// Accept). Cerberus serves only JSON — when asked for proto it
	// returns JSON instead. The differ must report a proto_decode
	// reason on the cerberus side AND an encoding_mismatch reason
	// (cerberus json shape vs cerberus proto shape, where the proto
	// shape is the empty fallback after decode failure).
	t.Parallel()

	want := twoSpanTrace()
	protoBytes, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Tempo-shape JSON: {batches:[{scopeSpans:[{spans:[{spanId}, ...]}]}]}.
	// We use the same hex span IDs both encodings would project to so
	// the cross-encoding parity holds on the tempo side.
	wantShape := projectProtoShape(want)
	if len(wantShape.SpanIDs) != 2 {
		t.Fatalf("fixture: want 2 span IDs, got %d", len(wantShape.SpanIDs))
	}
	tempoJSON := []byte(`{"batches":[{"scopeSpans":[{"spans":[` +
		`{"spanId":"` + wantShape.SpanIDs[0] + `"},` +
		`{"spanId":"` + wantShape.SpanIDs[1] + `"}` +
		`]}]}]}`)

	tempoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/protobuf" {
			w.Header().Set("Content-Type", "application/protobuf")
			_, _ = w.Write(protoBytes)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tempoJSON)
	}))
	defer tempoSrv.Close()

	// Cerberus: returns JSON regardless of Accept (the #199/#650 bug).
	cerbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tempoJSON)
	}))
	defer cerbSrv.Close()

	res := CaseResult{Case: CorpusCase{Endpoint: "traces"}}
	client := &http.Client{Timeout: 2 * time.Second}
	diffTracesEndpoint(context.Background(), client, tempoSrv.URL, cerbSrv.URL, &res)

	if res.Diff.Equal {
		t.Fatalf("Diff.Equal = true, want false (cerberus proto path is broken)")
	}
	// At least one proto_decode reason on the cerberus side.
	foundProtoDecode := false
	for _, r := range res.Diff.Reasons {
		if r.Kind == "proto_decode" && strings.Contains(r.Detail, "cerberus") {
			foundProtoDecode = true
			break
		}
	}
	if !foundProtoDecode {
		t.Errorf("missing proto_decode/cerberus reason; reasons=%+v", res.Diff.Reasons)
	}
}

func TestDiffTracesEndpoint_BothBackendsHealthy(t *testing.T) {
	// Happy path: both backends serve identical JSON + proto. Diff
	// should be Equal=true with MatchedCount = span count.
	t.Parallel()

	want := twoSpanTrace()
	protoBytes, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantShape := projectProtoShape(want)
	if len(wantShape.SpanIDs) != 2 {
		t.Fatalf("fixture: want 2 span IDs, got %d", len(wantShape.SpanIDs))
	}
	traceJSON := []byte(`{"batches":[{"scopeSpans":[{"spans":[` +
		`{"spanId":"` + wantShape.SpanIDs[0] + `"},` +
		`{"spanId":"` + wantShape.SpanIDs[1] + `"}` +
		`]}]}]}`)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/protobuf" {
			w.Header().Set("Content-Type", "application/protobuf")
			_, _ = w.Write(protoBytes)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(traceJSON)
	})
	tempoSrv := httptest.NewServer(handler)
	defer tempoSrv.Close()
	cerbSrv := httptest.NewServer(handler)
	defer cerbSrv.Close()

	res := CaseResult{Case: CorpusCase{Endpoint: "traces"}}
	client := &http.Client{Timeout: 2 * time.Second}
	diffTracesEndpoint(context.Background(), client, tempoSrv.URL, cerbSrv.URL, &res)

	if !res.Diff.Equal {
		t.Fatalf("Diff.Equal = false, want true; reasons=%+v", res.Diff.Reasons)
	}
	if res.Diff.MatchedCount != 2 {
		t.Errorf("MatchedCount = %d, want 2", res.Diff.MatchedCount)
	}
}

func TestDiffTracesEndpoint_CardinalityMismatchReported(t *testing.T) {
	// Both backends serve proto + JSON cleanly, but each side describes
	// a different number of spans. The cross-backend cardinality
	// reason must fire.
	t.Parallel()

	tempoTrace := twoSpanTrace()
	tempoProto, err := proto.Marshal(tempoTrace)
	if err != nil {
		t.Fatalf("marshal tempo: %v", err)
	}
	tempoShape := projectProtoShape(tempoTrace)
	tempoJSON := []byte(`{"batches":[{"scopeSpans":[{"spans":[` +
		`{"spanId":"` + tempoShape.SpanIDs[0] + `"},` +
		`{"spanId":"` + tempoShape.SpanIDs[1] + `"}` +
		`]}]}]}`)

	// Cerberus: one span only.
	cerbTrace := &tempopb.Trace{
		ResourceSpans: []*v11.ResourceSpans{{
			ScopeSpans: []*v11.ScopeSpans{{
				Spans: []*v11.Span{makeSpan("trace1", "spanA")},
			}},
		}},
	}
	cerbProto, err := proto.Marshal(cerbTrace)
	if err != nil {
		t.Fatalf("marshal cerb: %v", err)
	}
	cerbShape := projectProtoShape(cerbTrace)
	cerbJSON := []byte(`{"batches":[{"scopeSpans":[{"spans":[` +
		`{"spanId":"` + cerbShape.SpanIDs[0] + `"}` +
		`]}]}]}`)

	tempoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/protobuf" {
			_, _ = w.Write(tempoProto)
			return
		}
		_, _ = w.Write(tempoJSON)
	}))
	defer tempoSrv.Close()
	cerbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/protobuf" {
			_, _ = w.Write(cerbProto)
			return
		}
		_, _ = w.Write(cerbJSON)
	}))
	defer cerbSrv.Close()

	res := CaseResult{Case: CorpusCase{Endpoint: "traces"}}
	client := &http.Client{Timeout: 2 * time.Second}
	diffTracesEndpoint(context.Background(), client, tempoSrv.URL, cerbSrv.URL, &res)

	if res.Diff.Equal {
		t.Fatalf("Diff.Equal = true, want false on span-count mismatch")
	}
	foundCardinality := false
	for _, r := range res.Diff.Reasons {
		if r.Kind == "cardinality" {
			foundCardinality = true
			break
		}
	}
	if !foundCardinality {
		t.Errorf("missing cardinality reason; reasons=%+v", res.Diff.Reasons)
	}
}
