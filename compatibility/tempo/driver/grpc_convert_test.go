package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
)

// The grpc_convert.go converters exist to make the gRPC transport produce
// byte-shape-compatible input to the UNCHANGED comparator. Every test here
// therefore asserts on the *encoding conventions* the file's doc comment
// promises — zero-uint64 collapses to an omitted JSON field, AnyValue
// variants collapse to the flat string form MetricsLabel already tolerates
// — rather than on field-copy plumbing alone. A converter that formatted a
// zero as "0" would still round-trip through a field-by-field equality
// check; it would not survive these.

func TestFormatUint64OrEmpty(t *testing.T) {
	t.Parallel()
	if got := formatUint64OrEmpty(0); got != "" {
		t.Fatalf("formatUint64OrEmpty(0) = %q, want %q (proto3-JSON omits zero-valued fields)", got, "")
	}
	if got := formatUint64OrEmpty(1); got != "1" {
		t.Fatalf("formatUint64OrEmpty(1) = %q, want %q", got, "1")
	}
	// uint64 range must not narrow through an int conversion.
	const maxUint64Decimal = "18446744073709551615"
	if got := formatUint64OrEmpty(^uint64(0)); got != maxUint64Decimal {
		t.Fatalf("formatUint64OrEmpty(max) = %q, want %q", got, maxUint64Decimal)
	}
}

func TestTraceSummaryFromProto_ZeroTimestampOmittedInJSON(t *testing.T) {
	t.Parallel()
	// The whole point of formatUint64OrEmpty: a zero startTimeUnixNano
	// must vanish from the marshalled body exactly as the HTTP/protojson
	// side omits it, so the differ sees the same absent field on both
	// transports.
	summary := traceSummaryFromProto(&tempopb.TraceSearchMetadata{
		TraceID:           "abc",
		StartTimeUnixNano: 0,
	})
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "startTimeUnixNano") {
		t.Fatalf("zero startTimeUnixNano leaked into JSON: %s", body)
	}

	summary = traceSummaryFromProto(&tempopb.TraceSearchMetadata{
		TraceID:           "abc",
		StartTimeUnixNano: 1_700_000_000_000_000_000,
	})
	body, err = json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"startTimeUnixNano":"1700000000000000000"`) {
		t.Fatalf("non-zero startTimeUnixNano not rendered as a decimal string: %s", body)
	}
}

func TestTraceSummaryFromProto_Fields(t *testing.T) {
	t.Parallel()
	got := traceSummaryFromProto(&tempopb.TraceSearchMetadata{
		TraceID:           "aabb",
		RootServiceName:   "checkout",
		RootTraceName:     "GET /api/checkout",
		StartTimeUnixNano: 1000,
		DurationMs:        150,
		SpanSet:           &tempopb.SpanSet{Matched: 2},
		SpanSets: []*tempopb.SpanSet{
			{Matched: 1, Spans: []*tempopb.Span{{SpanID: "s1", Name: "db", DurationNanos: 42}}},
			{Matched: 3},
		},
	})
	if got.TraceID != "aabb" || got.RootServiceName != "checkout" || got.RootTraceName != "GET /api/checkout" {
		t.Fatalf("identity fields not carried: %+v", got)
	}
	if got.StartTimeUnixNano != "1000" {
		t.Fatalf("StartTimeUnixNano = %q, want %q", got.StartTimeUnixNano, "1000")
	}
	if got.DurationMs != 150 {
		t.Fatalf("DurationMs = %d, want 150", got.DurationMs)
	}
	if got.SpanSet == nil || got.SpanSet.Matched != 2 {
		t.Fatalf("SpanSet = %+v, want a non-nil pointer with Matched=2", got.SpanSet)
	}
	if len(got.SpanSets) != 2 {
		t.Fatalf("SpanSets len = %d, want 2", len(got.SpanSets))
	}
	if got.SpanSets[0].Matched != 1 || got.SpanSets[1].Matched != 3 {
		t.Fatalf("SpanSets order/content wrong: %+v", got.SpanSets)
	}
	if len(got.SpanSets[0].Spans) != 1 || got.SpanSets[0].Spans[0].DurationNanos != "42" {
		t.Fatalf("nested span not converted: %+v", got.SpanSets[0])
	}
}

func TestTraceSummaryFromProto_NilAndAbsentSpanSet(t *testing.T) {
	t.Parallel()
	if got := traceSummaryFromProto(nil); got.TraceID != "" || got.SpanSet != nil || got.SpanSets != nil {
		t.Fatalf("traceSummaryFromProto(nil) = %+v, want the zero TraceSummary", got)
	}
	// An absent SpanSet must stay a nil pointer (JSON-omitted), not a
	// pointer to a zero SpanSetJSON, which would render `"spanSet":{}`
	// and diff against the HTTP side's omission.
	got := traceSummaryFromProto(&tempopb.TraceSearchMetadata{TraceID: "x"})
	if got.SpanSet != nil {
		t.Fatalf("absent SpanSet materialised as %+v, want nil", got.SpanSet)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "spanSet") {
		t.Fatalf("absent spanSet leaked into JSON: %s", body)
	}
}

func TestSpanSetFromProto_NilSetAndNilSpanSkipped(t *testing.T) {
	t.Parallel()
	if got := spanSetFromProto(nil); got.Matched != 0 || got.Spans != nil {
		t.Fatalf("spanSetFromProto(nil) = %+v, want the zero SpanSetJSON", got)
	}
	got := spanSetFromProto(&tempopb.SpanSet{
		Matched: 7,
		Spans: []*tempopb.Span{
			nil,
			{SpanID: "s1", Name: "db.query", StartTimeUnixNano: 5, DurationNanos: 0},
		},
	})
	if got.Matched != 7 {
		t.Fatalf("Matched = %d, want 7", got.Matched)
	}
	if len(got.Spans) != 1 {
		t.Fatalf("Spans len = %d, want 1 (the nil span must be skipped, not converted)", len(got.Spans))
	}
	sp := got.Spans[0]
	if sp.SpanID != "s1" || sp.Name != "db.query" || sp.StartTimeUnixNano != "5" {
		t.Fatalf("span fields wrong: %+v", sp)
	}
	if sp.DurationNanos != "" {
		t.Fatalf("zero DurationNanos = %q, want %q", sp.DurationNanos, "")
	}
}

func TestTagsV2ScopeFromProto_NilAndDefensiveCopy(t *testing.T) {
	t.Parallel()
	if got := tagsV2ScopeFromProto(nil); got.Name != "" || got.Tags != nil {
		t.Fatalf("tagsV2ScopeFromProto(nil) = %+v, want the zero TagNamesScope", got)
	}
	src := &tempopb.SearchTagsV2Scope{Name: "resource", Tags: []string{"service.name", "cluster"}}
	got := tagsV2ScopeFromProto(src)
	if got.Name != "resource" || len(got.Tags) != 2 {
		t.Fatalf("tagsV2ScopeFromProto = %+v", got)
	}
	// The converter copies rather than aliasing, so a later reuse of the
	// streamed frame's backing array can't rewrite an already-accumulated
	// scope.
	src.Tags[0] = "mutated"
	if got.Tags[0] != "service.name" {
		t.Fatalf("Tags aliases the proto frame's slice: got %q after mutating the source", got.Tags[0])
	}
}

func TestTagValueV2FromProto(t *testing.T) {
	t.Parallel()
	if got := tagValueV2FromProto(nil); got.Type != "" || got.Value != "" {
		t.Fatalf("tagValueV2FromProto(nil) = %+v, want the zero TagValueV2", got)
	}
	got := tagValueV2FromProto(&tempopb.TagValue{Type: "string", Value: "checkout"})
	if got.Type != "string" || got.Value != "checkout" {
		t.Fatalf("tagValueV2FromProto = %+v", got)
	}
}

func TestAnyValueToString_AllVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *v1.AnyValue
		want string
	}{
		{"nil pointer", nil, ""},
		{"unset variant (nil group-by bucket)", &v1.AnyValue{}, ""},
		{"string", &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "checkout"}}, "checkout"},
		{"empty string stays empty", &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: ""}}, ""},
		{"int64 as decimal string", &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: -7}}, "-7"},
		{"double", &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: 1.5}}, "1.5"},
		{"bool true", &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: true}}, "true"},
		{"bool false", &v1.AnyValue{Value: &v1.AnyValue_BoolValue{BoolValue: false}}, "false"},
		{"unsupported variant falls back to empty", &v1.AnyValue{Value: &v1.AnyValue_BytesValue{BytesValue: []byte("x")}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := anyValueToString(tc.in); got != tc.want {
				t.Fatalf("anyValueToString = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnyValueToString_MatchesHTTPSideUnmarshal(t *testing.T) {
	t.Parallel()
	// The converter's contract is "produce what MetricsLabel.UnmarshalJSON
	// produces from the equivalent HTTP body". Pin the two representations
	// against each other so a change to either side has to change both.
	var viaHTTP MetricsLabel
	if err := json.Unmarshal([]byte(`{"key":"k","value":{"doubleValue":1.5}}`), &viaHTTP); err != nil {
		t.Fatalf("unmarshal HTTP-shape label: %v", err)
	}
	viaGRPC := keyValuesToLabels([]v1.KeyValue{{
		Key:   "k",
		Value: &v1.AnyValue{Value: &v1.AnyValue_DoubleValue{DoubleValue: 1.5}},
	}})
	if len(viaGRPC) != 1 || viaGRPC[0] != viaHTTP {
		t.Fatalf("gRPC label %+v != HTTP label %+v", viaGRPC, viaHTTP)
	}
}

func TestKeyValuesToLabels_OrderAndEmpty(t *testing.T) {
	t.Parallel()
	// An empty label set must convert to a non-nil empty slice: nil would
	// marshal to `null` where the HTTP side emits `[]`.
	got := keyValuesToLabels(nil)
	if got == nil {
		t.Fatal("keyValuesToLabels(nil) returned a nil slice; want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("keyValuesToLabels(nil) = %+v, want empty", got)
	}

	got = keyValuesToLabels([]v1.KeyValue{
		{Key: "b", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "2"}}},
		{Key: "a", Value: nil},
	})
	want := []MetricsLabel{{Key: "b", Value: "2"}, {Key: "a", Value: ""}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("label[%d] = %+v, want %+v (wire order must be preserved)", i, got[i], want[i])
		}
	}
}

func TestMarshalDiffBody(t *testing.T) {
	t.Parallel()
	body, err := marshalDiffBody(TagNamesResponseV1{TagNames: []string{"a"}})
	if err != nil {
		t.Fatalf("marshalDiffBody: %v", err)
	}
	if string(body) != `{"tagNames":["a"]}` {
		t.Fatalf("body = %s", body)
	}

	// The error arm exists so every fetcher wraps a marshal failure
	// identically; a channel is the canonical unmarshalable value.
	_, err = marshalDiffBody(make(chan int))
	if err == nil {
		t.Fatal("marshalDiffBody(chan) returned no error")
	}
	if !strings.Contains(err.Error(), "marshal accumulated grpc response") {
		t.Fatalf("error not wrapped with the shared prefix: %v", err)
	}
}
