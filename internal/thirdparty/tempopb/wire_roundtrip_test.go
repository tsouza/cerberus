package tempopb_test

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"

	"github.com/tsouza/cerberus/internal/thirdparty/tempopb"
	commonv1 "github.com/tsouza/cerberus/internal/thirdparty/tempopb/common/v1"
	resourcev1 "github.com/tsouza/cerberus/internal/thirdparty/tempopb/resource/v1"
	tracev1 "github.com/tsouza/cerberus/internal/thirdparty/tempopb/trace/v1"
)

// wireMessage is the gogo-generated surface every tempopb message carries.
type wireMessage interface {
	proto.Message
	Marshal() ([]byte, error)
	Unmarshal([]byte) error
	Size() int
}

func strKV(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

// everyAnyValue carries one attribute per AnyValue variant, nested ones included.
func everyAnyValue() []*commonv1.KeyValue {
	return []*commonv1.KeyValue{
		strKV("service.name", "checkout"),
		{Key: "bool", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: true}}},
		{Key: "int", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: -42}}},
		{Key: "double", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: 2.5}}},
		{Key: "bytes", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: []byte{0xde, 0xad}}}},
		{Key: "array", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{
			Values: []*commonv1.AnyValue{{Value: &commonv1.AnyValue_StringValue{StringValue: "a"}}, {Value: &commonv1.AnyValue_IntValue{IntValue: 7}}},
		}}}},
		{Key: "kvlist", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{
			Values: []*commonv1.KeyValue{strKV("inner", "x")},
		}}}},
	}
}

func fullTrace() *tempopb.Trace {
	traceID := bytes.Repeat([]byte{0x01}, 16)
	return &tempopb.Trace{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource:  &resourcev1.Resource{Attributes: everyAnyValue(), DroppedAttributesCount: 1},
		SchemaUrl: "https://opentelemetry.io/schemas/1.21.0",
		ScopeSpans: []*tracev1.ScopeSpans{{
			Scope: &commonv1.InstrumentationScope{Name: "otel", Version: "1.0", Attributes: []*commonv1.KeyValue{strKV("s", "t")}, DroppedAttributesCount: 2},
			Spans: []*tracev1.Span{{
				TraceId:                traceID,
				SpanId:                 []byte{1, 2, 3, 4, 5, 6, 7, 8},
				ParentSpanId:           []byte{8, 7, 6, 5, 4, 3, 2, 1},
				TraceState:             "k=v",
				Flags:                  1,
				Name:                   "GET /cart",
				Kind:                   tracev1.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano:      1_700_000_000_000_000_000,
				EndTimeUnixNano:        1_700_000_000_500_000_000,
				Attributes:             everyAnyValue(),
				DroppedAttributesCount: 3,
				Events: []*tracev1.Span_Event{{
					TimeUnixNano: 1_700_000_000_100_000_000, Name: "exception",
					Attributes: []*commonv1.KeyValue{strKV("exception.type", "io")}, DroppedAttributesCount: 4,
				}},
				DroppedEventsCount: 5,
				Links: []*tracev1.Span_Link{{
					TraceId: traceID, SpanId: []byte{9, 9, 9, 9, 9, 9, 9, 9}, TraceState: "a=b",
					Attributes: []*commonv1.KeyValue{strKV("link", "l")}, DroppedAttributesCount: 6, Flags: 1,
				}},
				DroppedLinksCount: 7,
				Status:            &tracev1.Status{Message: "boom", Code: tracev1.Status_STATUS_CODE_ERROR},
			}},
		}},
	}}}
}

func searchMetrics() *tempopb.SearchMetrics {
	return &tempopb.SearchMetrics{InspectedTraces: 1, InspectedBytes: 2, TotalBlocks: 3, CompletedJobs: 4, TotalJobs: 5, TotalBlockBytes: 6, InspectedSpans: 7}
}

func labels() []commonv1.KeyValue {
	return []commonv1.KeyValue{*strKV("resource.service.name", "checkout")}
}

// TestWireRoundTrip encodes each message the Tempo head serves or accepts,
// populated across every field, decodes it into a fresh value, and requires the
// result to equal the input and the encoded length to equal Size().
func TestWireRoundTrip(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in    wireMessage
		fresh func() wireMessage
	}{
		"Trace": {fullTrace(), func() wireMessage { return &tempopb.Trace{} }},
		"TraceByIDResponse": {&tempopb.TraceByIDResponse{
			Trace: fullTrace(), Metrics: &tempopb.TraceByIDMetrics{InspectedBytes: 99},
			Status: tempopb.PartialStatus_PARTIAL, Message: "partial",
		}, func() wireMessage { return &tempopb.TraceByIDResponse{} }},
		"SearchRequest": {&tempopb.SearchRequest{
			Tags: map[string]string{"service.name": "checkout"}, MinDurationMs: 1, MaxDurationMs: 2, Limit: 3,
			Start: 4, End: 5, Query: `{ span.http.status_code = 500 }`, SpansPerSpanSet: 6,
			RF1After: time.Unix(1_700_000_000, 0).UTC(), SkipASTTransformations: []string{"x"},
		}, func() wireMessage { return &tempopb.SearchRequest{} }},
		"SearchResponse": {&tempopb.SearchResponse{
			Traces: []*tempopb.TraceSearchMetadata{{
				TraceID: "0102", RootServiceName: "checkout", RootTraceName: "GET /cart",
				StartTimeUnixNano: 1_700_000_000_000_000_000, DurationMs: 500,
				SpanSet: &tempopb.SpanSet{
					Spans: []*tempopb.Span{{
						SpanID: "0102030405060708", Name: "GET /cart", StartTimeUnixNano: 1, DurationNanos: 2,
						Attributes: everyAnyValue(),
					}},
					Matched: 1, Attributes: []*commonv1.KeyValue{strKV("by", "x")},
				},
				SpanSets:     []*tempopb.SpanSet{{Matched: 2}},
				ServiceStats: map[string]*tempopb.ServiceStats{"checkout": {SpanCount: 3, ErrorCount: 1}},
			}},
			Metrics: searchMetrics(),
		}, func() wireMessage { return &tempopb.SearchResponse{} }},
		"SearchTagsV2Response": {&tempopb.SearchTagsV2Response{
			Scopes:  []*tempopb.SearchTagsV2Scope{{Name: "span", Tags: []string{"http.method", "http.status_code"}}},
			Metrics: &tempopb.MetadataMetrics{InspectedBytes: 1},
		}, func() wireMessage { return &tempopb.SearchTagsV2Response{} }},
		"SearchTagValuesV2Response": {&tempopb.SearchTagValuesV2Response{
			TagValues: []*tempopb.TagValue{{Type: "string", Value: "GET"}, {Type: "int", Value: "200"}},
			Metrics:   &tempopb.MetadataMetrics{InspectedBytes: 1},
		}, func() wireMessage { return &tempopb.SearchTagValuesV2Response{} }},
		"QueryRangeRequest": {&tempopb.QueryRangeRequest{
			Query: `{} | rate()`, Start: 1, End: 2, Step: 3, QueryMode: "recent", Exemplars: 4, MaxSeries: 5,
		}, func() wireMessage { return &tempopb.QueryRangeRequest{} }},
		"QueryRangeResponse": {&tempopb.QueryRangeResponse{
			Series: []*tempopb.TimeSeries{{
				Labels:    labels(),
				Samples:   []tempopb.Sample{{TimestampMs: 1000, Value: 1.5}, {TimestampMs: 2000, Value: 2.5}},
				Exemplars: []tempopb.Exemplar{{Labels: labels(), Value: 3, TimestampMs: 1500}},
			}},
			Metrics: searchMetrics(), Status: tempopb.PartialStatus_PARTIAL, Message: "m",
		}, func() wireMessage { return &tempopb.QueryRangeResponse{} }},
		"QueryInstantResponse": {&tempopb.QueryInstantResponse{
			Series:  []*tempopb.InstantSeries{{Labels: labels(), Value: 4.5}},
			Metrics: searchMetrics(), Status: tempopb.PartialStatus_PARTIAL, Message: "m",
		}, func() wireMessage { return &tempopb.QueryInstantResponse{} }},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wire, err := tc.in.Marshal()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got, want := len(wire), tc.in.Size(); got != want {
				t.Fatalf("len(Marshal()) = %d, Size() = %d", got, want)
			}
			out := tc.fresh()
			if err := out.Unmarshal(wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.in, out) {
				t.Fatalf("round trip changed the message:\n in: %v\nout: %v", tc.in, out)
			}
		})
	}
}

// TestWireBytesArePinned pins the exact encoding of two small messages against
// bytes written from the protobuf wire format by hand, so a symmetric
// marshal/unmarshal defect cannot hide behind the round trip above.
func TestWireBytesArePinned(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in   wireMessage
		want []byte
	}{
		// field 1 (key, LEN) "k"; field 2 (value, LEN) { field 1 (string_value, LEN) "v" }.
		"KeyValue": {strKV("k", "v"), []byte{0x0a, 0x01, 'k', 0x12, 0x03, 0x0a, 0x01, 'v'}},
		// field 1 (value, I64) 1.0; field 2 (timestamp_ms, VARINT) 1.
		"Sample": {&tempopb.Sample{TimestampMs: 1, Value: 1}, []byte{0x09, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f, 0x10, 0x01}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.in.Marshal()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("Marshal() = % x, want % x", got, tc.want)
			}
		})
	}
}

// TestRegistryNamesAreCerberusScoped pins the registry rename that lets these
// types share a test binary with Tempo's own copy of the same .proto files.
func TestRegistryNamesAreCerberusScoped(t *testing.T) {
	t.Parallel()

	for msg, want := range map[proto.Message]string{
		&tempopb.Trace{}:          "cerberus.tempopb.Trace",
		&tempopb.SearchResponse{}: "cerberus.tempopb.SearchResponse",
		&tracev1.Span{}:           "cerberus.tempopb.trace.v1.Span",
		&commonv1.KeyValue{}:      "cerberus.tempopb.common.v1.KeyValue",
		&resourcev1.Resource{}:    "cerberus.tempopb.resource.v1.Resource",
	} {
		if got := proto.MessageName(msg); got != want {
			t.Errorf("proto.MessageName(%T) = %q, want %q", msg, got, want)
		}
	}
}
