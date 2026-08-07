package main

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	"google.golang.org/grpc"
)

// The gRPC transport's whole claim is that it is a transport-only
// difference from the comparator's perspective: drain the stream, flatten
// the frames, hand the comparator the same JSON shape the HTTP path
// decodes. tempopb.StreamingQuerierClient is an interface, so that claim
// is testable without a server — the fake below returns canned frames and
// the assertions below check the three things that can actually go wrong:
// multi-frame accumulation, request-field wiring (the Unix-seconds vs
// nanoseconds split between the search-side and metrics-side RPCs), and
// error wrapping on the open / recv arms.

// fakeStream replays canned frames and then either io.EOF or a canned
// recv error. It embeds the nil grpc.ClientStream because the fetchers
// only ever call Recv.
type fakeStream[T any] struct {
	grpc.ClientStream
	frames []T
	recv   error
	i      int
}

func (f *fakeStream[T]) Recv() (T, error) {
	if f.i < len(f.frames) {
		v := f.frames[f.i]
		f.i++
		return v, nil
	}
	var zero T
	if f.recv != nil {
		return zero, f.recv
	}
	return zero, io.EOF
}

var (
	errOpen = errors.New("dial refused")
	errRecv = errors.New("stream broke mid-drain")
)

// fakeQuerier implements tempopb.StreamingQuerierClient over canned
// frames, records the request each RPC received, and can be told to fail
// either at open time or mid-drain.
type fakeQuerier struct {
	searchFrames       []*tempopb.SearchResponse
	tagsV1Frames       []*tempopb.SearchTagsResponse
	tagsV2Frames       []*tempopb.SearchTagsV2Response
	tagValuesV1Frames  []*tempopb.SearchTagValuesResponse
	tagValuesV2Frames  []*tempopb.SearchTagValuesV2Response
	metricsRangeFrames []*tempopb.QueryRangeResponse
	metricsInstFrames  []*tempopb.QueryInstantResponse

	openErr error
	recvErr error

	searchReq    *tempopb.SearchRequest
	tagsReq      *tempopb.SearchTagsRequest
	tagsV2Req    *tempopb.SearchTagsRequest
	tagValuesReq *tempopb.SearchTagValuesRequest
	tagValsV2Req *tempopb.SearchTagValuesRequest
	rangeReq     *tempopb.QueryRangeRequest
	instantReq   *tempopb.QueryInstantRequest
}

func (f *fakeQuerier) Search(_ context.Context, in *tempopb.SearchRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_SearchClient, error) {
	f.searchReq = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.SearchResponse]{frames: f.searchFrames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) SearchTags(_ context.Context, in *tempopb.SearchTagsRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_SearchTagsClient, error) {
	f.tagsReq = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.SearchTagsResponse]{frames: f.tagsV1Frames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) SearchTagsV2(_ context.Context, in *tempopb.SearchTagsRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_SearchTagsV2Client, error) {
	f.tagsV2Req = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.SearchTagsV2Response]{frames: f.tagsV2Frames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) SearchTagValues(_ context.Context, in *tempopb.SearchTagValuesRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_SearchTagValuesClient, error) {
	f.tagValuesReq = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.SearchTagValuesResponse]{frames: f.tagValuesV1Frames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) SearchTagValuesV2(_ context.Context, in *tempopb.SearchTagValuesRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_SearchTagValuesV2Client, error) {
	f.tagValsV2Req = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.SearchTagValuesV2Response]{frames: f.tagValuesV2Frames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) MetricsQueryRange(_ context.Context, in *tempopb.QueryRangeRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_MetricsQueryRangeClient, error) {
	f.rangeReq = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.QueryRangeResponse]{frames: f.metricsRangeFrames, recv: f.recvErr}, nil
}

func (f *fakeQuerier) MetricsQueryInstant(_ context.Context, in *tempopb.QueryInstantRequest, _ ...grpc.CallOption) (tempopb.StreamingQuerier_MetricsQueryInstantClient, error) {
	f.instantReq = in
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeStream[*tempopb.QueryInstantResponse]{frames: f.metricsInstFrames, recv: f.recvErr}, nil
}

// testWindowStart / testWindowEnd are a fixed compatibility window; the
// exact instants don't matter, only that the two RPC families encode them
// in their own units.
var (
	testWindowStart = time.Unix(1_700_000_000, 0).UTC()
	testWindowEnd   = time.Unix(1_700_003_600, 0).UTC()
)

func testOpts() caseOpts {
	return caseOpts{startTS: testWindowStart, endTS: testWindowEnd, searchLimit: 20}
}

func TestGRPCSupportsEndpoint(t *testing.T) {
	t.Parallel()
	// The 7 RPCs tempopb.StreamingQuerier actually exposes.
	for _, ep := range []string{"search", "tags_v1", "tags_v2", "tag_values_v1", "tag_values_v2", "metrics_range", "metrics_instant"} {
		if !grpcSupportsEndpoint(ep) {
			t.Errorf("grpcSupportsEndpoint(%q) = false, want true — it has a StreamingQuerier RPC", ep)
		}
	}
	// There is no trace-by-id and no search/recent RPC on either backend.
	for _, ep := range []string{"traces", "traces_v2", "search_recent", "", "SEARCH"} {
		if grpcSupportsEndpoint(ep) {
			t.Errorf("grpcSupportsEndpoint(%q) = true, want false — no StreamingQuerier RPC exists", ep)
		}
	}
}

func TestUnixSeconds32_ClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	if got := unixSeconds32(testWindowStart); got != 1_700_000_000 {
		t.Fatalf("unixSeconds32(recent) = %d, want 1700000000", got)
	}
	// A pre-epoch instant must saturate to 0 rather than wrap to a huge
	// uint32, which would silently push the window bound past the data.
	if got := unixSeconds32(time.Unix(-1, 0)); got != 0 {
		t.Fatalf("unixSeconds32(pre-epoch) = %d, want 0", got)
	}
	if got := unixSeconds32(time.Unix(math.MaxUint32+1, 0)); got != math.MaxUint32 {
		t.Fatalf("unixSeconds32(post-2106) = %d, want %d", got, uint32(math.MaxUint32))
	}
	if got := unixSeconds32(time.Unix(math.MaxUint32, 0)); got != math.MaxUint32 {
		t.Fatalf("unixSeconds32(exactly MaxUint32) = %d, want %d", got, uint32(math.MaxUint32))
	}
}

func TestFetchGRPCSearch_AccumulatesEveryFrame(t *testing.T) {
	t.Parallel()
	// cerberus batches searchFrameSize=20 summaries per frame, so the
	// fetcher MUST drain to EOF; a fetcher that returned after the first
	// frame would silently truncate the result set and read as a diff.
	client := &fakeQuerier{searchFrames: []*tempopb.SearchResponse{
		{Traces: []*tempopb.TraceSearchMetadata{
			{TraceID: "t1", RootServiceName: "checkout", DurationMs: 5},
			{TraceID: "t2", RootServiceName: "cart"},
		}},
		{Traces: []*tempopb.TraceSearchMetadata{{TraceID: "t3"}}},
	}}
	tc := CorpusCase{Name: "c", Endpoint: "search", Query: `{}`, Spss: 3}
	body, err := fetchGRPCSearch(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCSearch: %v", err)
	}
	for _, id := range []string{`"t1"`, `"t2"`, `"t3"`} {
		if !strings.Contains(string(body), id) {
			t.Fatalf("frame accumulation dropped %s: %s", id, body)
		}
	}
	if client.searchReq.Query != `{}` {
		t.Fatalf("Query = %q, want %q", client.searchReq.Query, `{}`)
	}
	if client.searchReq.Limit != 20 {
		t.Fatalf("Limit = %d, want the caseOpts searchLimit 20", client.searchReq.Limit)
	}
	if client.searchReq.SpansPerSpanSet != 3 {
		t.Fatalf("SpansPerSpanSet = %d, want 3", client.searchReq.SpansPerSpanSet)
	}
	// SearchRequest's Start/End are Unix SECONDS, not nanoseconds.
	if client.searchReq.Start != 1_700_000_000 || client.searchReq.End != 1_700_003_600 {
		t.Fatalf("Start/End = %d/%d, want unix seconds 1700000000/1700003600", client.searchReq.Start, client.searchReq.End)
	}
}

func TestFetchGRPCSearch_SpssLeftUnsetWhenZero(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{}
	if _, err := fetchGRPCSearch(context.Background(), client, CorpusCase{Endpoint: "search"}, testOpts()); err != nil {
		t.Fatalf("fetchGRPCSearch: %v", err)
	}
	if client.searchReq.SpansPerSpanSet != 0 {
		t.Fatalf("SpansPerSpanSet = %d with no corpus -- spss --, want it left unset", client.searchReq.SpansPerSpanSet)
	}
}

func TestFetchGRPCSearch_EmptyStreamMarshalsEmptyEnvelope(t *testing.T) {
	t.Parallel()
	body, err := fetchGRPCSearch(context.Background(), &fakeQuerier{}, CorpusCase{Endpoint: "search"}, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCSearch: %v", err)
	}
	// The comparator decodes this; it must be valid JSON with a traces key.
	if !strings.Contains(string(body), "traces") {
		t.Fatalf("empty stream produced %s, want a traces envelope", body)
	}
}

func TestFetchGRPCTagsV1(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{tagsV1Frames: []*tempopb.SearchTagsResponse{
		{TagNames: []string{"a"}},
		{TagNames: []string{"b", "c"}},
	}}
	tc := CorpusCase{Endpoint: "tags_v1", Query: `{ span.http.method = "GET" }`}
	body, err := fetchGRPCTagsV1(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCTagsV1: %v", err)
	}
	if string(body) != `{"tagNames":["a","b","c"]}` {
		t.Fatalf("body = %s", body)
	}
	if client.tagsReq.Start != 1_700_000_000 || client.tagsReq.End != 1_700_003_600 {
		t.Fatalf("window not wired: %+v", client.tagsReq)
	}
	// The RPC is driven with the case's query even though V1 ignores it.
	// A backend that started narrowing on V1 has to fail the corpus case
	// over gRPC as well as over HTTP, and it cannot if the parameter never
	// leaves the harness.
	if client.tagsReq.Query != tc.Query {
		t.Fatalf("Query = %q, want the corpus case's query", client.tagsReq.Query)
	}
}

func TestFetchGRPCTagsV2_CarriesScopeAndQuery(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{tagsV2Frames: []*tempopb.SearchTagsV2Response{
		{Scopes: []*tempopb.SearchTagsV2Scope{{Name: "resource", Tags: []string{"service.name"}}}},
		{Scopes: []*tempopb.SearchTagsV2Scope{{Name: "span", Tags: []string{"http.method"}}}},
	}}
	tc := CorpusCase{Endpoint: "tags_v2", Scope: "resource", Query: `{ span.http.method = "GET" }`}
	body, err := fetchGRPCTagsV2(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCTagsV2: %v", err)
	}
	if !strings.Contains(string(body), `"resource"`) || !strings.Contains(string(body), `"span"`) {
		t.Fatalf("scopes not accumulated across frames: %s", body)
	}
	if client.tagsV2Req.Scope != "resource" {
		t.Fatalf("Scope = %q, want the corpus case's scope", client.tagsV2Req.Scope)
	}
	// V2 is the route that narrows, so the query must reach the RPC —
	// without it the case is graded against the wide answer and the
	// corpus's absent-key assertion fires on every backend.
	if client.tagsV2Req.Query != tc.Query {
		t.Fatalf("Query = %q, want the corpus case's query", client.tagsV2Req.Query)
	}
}

func TestFetchGRPCTagValuesV1(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{tagValuesV1Frames: []*tempopb.SearchTagValuesResponse{
		{TagValues: []string{"checkout"}},
		{TagValues: []string{"cart"}},
	}}
	tc := CorpusCase{Endpoint: "tag_values_v1", TagName: "service.name"}
	body, err := fetchGRPCTagValuesV1(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCTagValuesV1: %v", err)
	}
	if string(body) != `{"tagValues":["checkout","cart"]}` {
		t.Fatalf("body = %s", body)
	}
	if client.tagValuesReq.TagName != "service.name" {
		t.Fatalf("TagName = %q, want the corpus case's tag name", client.tagValuesReq.TagName)
	}
}

func TestFetchGRPCTagValuesV2_KeepsTypes(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{tagValuesV2Frames: []*tempopb.SearchTagValuesV2Response{
		{TagValues: []*tempopb.TagValue{{Type: "string", Value: "checkout"}}},
		{TagValues: []*tempopb.TagValue{{Type: "int", Value: "7"}}},
	}}
	tc := CorpusCase{Endpoint: "tag_values_v2", TagName: "span.http.status_code"}
	body, err := fetchGRPCTagValuesV2(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCTagValuesV2: %v", err)
	}
	if !strings.Contains(string(body), `"type":"string"`) || !strings.Contains(string(body), `"type":"int"`) {
		t.Fatalf("typed values not carried: %s", body)
	}
	if client.tagValsV2Req.TagName != "span.http.status_code" {
		t.Fatalf("TagName = %q", client.tagValsV2Req.TagName)
	}
}

func TestFetchGRPCMetricsRange_NanosecondWindowAndStep(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{metricsRangeFrames: []*tempopb.QueryRangeResponse{{
		Series: []*tempopb.TimeSeries{{
			Labels:  []v1.KeyValue{{Key: "svc", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "checkout"}}}},
			Samples: []tempopb.Sample{{TimestampMs: 1000, Value: 2.5}},
		}},
	}}}
	tc := CorpusCase{Endpoint: "metrics_range", Query: `{} | rate()`, Step: "60s"}
	body, err := fetchGRPCMetricsRange(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCMetricsRange: %v", err)
	}
	if !strings.Contains(string(body), `"checkout"`) || !strings.Contains(string(body), "2.5") {
		t.Fatalf("series not converted: %s", body)
	}
	// The metrics RPCs use NANOSECOND bounds, unlike the search-side RPCs.
	if client.rangeReq.Start != uint64(testWindowStart.UnixNano()) {
		t.Fatalf("Start = %d, want nanoseconds %d", client.rangeReq.Start, testWindowStart.UnixNano())
	}
	if client.rangeReq.End != uint64(testWindowEnd.UnixNano()) {
		t.Fatalf("End = %d, want nanoseconds %d", client.rangeReq.End, testWindowEnd.UnixNano())
	}
	if client.rangeReq.Step != uint64(time.Minute.Nanoseconds()) {
		t.Fatalf("Step = %d, want 60s in nanoseconds", client.rangeReq.Step)
	}
}

func TestFetchGRPCMetricsRange_BadStepIsAnError(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Endpoint: "metrics_range", Step: "banana"}
	_, err := fetchGRPCMetricsRange(context.Background(), &fakeQuerier{}, tc, testOpts())
	if err == nil {
		t.Fatal("unparseable -- step -- accepted")
	}
	if !strings.Contains(err.Error(), "banana") {
		t.Fatalf("error should name the offending step: %v", err)
	}
}

func TestFetchGRPCMetricsInstant_ScalarValueAndNilSeriesSkipped(t *testing.T) {
	t.Parallel()
	client := &fakeQuerier{metricsInstFrames: []*tempopb.QueryInstantResponse{{
		Series: []*tempopb.InstantSeries{
			nil,
			{
				Labels: []v1.KeyValue{{Key: "svc", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "cart"}}}},
				Value:  3.25,
			},
		},
	}}}
	tc := CorpusCase{Endpoint: "metrics_instant", Query: `{} | count_over_time()`}
	body, err := fetchGRPCMetricsInstant(context.Background(), client, tc, testOpts())
	if err != nil {
		t.Fatalf("fetchGRPCMetricsInstant: %v", err)
	}
	if strings.Count(string(body), `"value"`) != 1 {
		t.Fatalf("nil series not skipped (want exactly one value entry): %s", body)
	}
	if !strings.Contains(string(body), "3.25") {
		t.Fatalf("scalar value missing: %s", body)
	}
	if strings.Contains(string(body), "samples") {
		t.Fatalf("instant response must not carry a samples array: %s", body)
	}
	if client.instantReq.Start != uint64(testWindowStart.UnixNano()) {
		t.Fatalf("Start = %d, want nanoseconds", client.instantReq.Start)
	}
}

func TestMetricsSeriesFromProtoRange(t *testing.T) {
	t.Parallel()
	if got := metricsSeriesFromProtoRange(nil); got.Labels != nil || got.Samples != nil {
		t.Fatalf("metricsSeriesFromProtoRange(nil) = %+v, want the zero entry", got)
	}
	got := metricsSeriesFromProtoRange(&tempopb.TimeSeries{
		Labels:  []v1.KeyValue{{Key: "svc", Value: &v1.AnyValue{Value: &v1.AnyValue_IntValue{IntValue: 7}}}},
		Samples: []tempopb.Sample{{TimestampMs: 10, Value: 1}, {TimestampMs: 20, Value: 2}},
		Exemplars: []tempopb.Exemplar{{
			Labels:      []v1.KeyValue{{Key: "trace", Value: &v1.AnyValue{Value: &v1.AnyValue_StringValue{StringValue: "abc"}}}},
			Value:       9,
			TimestampMs: 15,
		}},
	})
	if len(got.Labels) != 1 || got.Labels[0].Value != "7" {
		t.Fatalf("labels = %+v, want the int variant flattened to \"7\"", got.Labels)
	}
	if len(got.Samples) != 2 || got.Samples[1].TimestampMs != 20 || got.Samples[1].Value != 2 {
		t.Fatalf("samples = %+v", got.Samples)
	}
	if len(got.Exemplars) != 1 || got.Exemplars[0].Value != 9 || got.Exemplars[0].TimestampMs != 15 {
		t.Fatalf("exemplars = %+v", got.Exemplars)
	}
	if len(got.Exemplars[0].Labels) != 1 || got.Exemplars[0].Labels[0].Key != "trace" {
		t.Fatalf("exemplar labels = %+v", got.Exemplars[0].Labels)
	}
	// Range series carry Samples, never a scalar Value.
	if got.Value != nil {
		t.Fatalf("Value = %v, want nil on a range series", *got.Value)
	}
}

func TestFetchGRPCForEndpoint_DispatchesToTheMatchingRPC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		endpoint string
		hit      func(*fakeQuerier) bool
	}{
		{"search", func(f *fakeQuerier) bool { return f.searchReq != nil }},
		{"tags_v1", func(f *fakeQuerier) bool { return f.tagsReq != nil }},
		{"tags_v2", func(f *fakeQuerier) bool { return f.tagsV2Req != nil }},
		{"tag_values_v1", func(f *fakeQuerier) bool { return f.tagValuesReq != nil }},
		{"tag_values_v2", func(f *fakeQuerier) bool { return f.tagValsV2Req != nil }},
		{"metrics_range", func(f *fakeQuerier) bool { return f.rangeReq != nil }},
		{"metrics_instant", func(f *fakeQuerier) bool { return f.instantReq != nil }},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()
			client := &fakeQuerier{}
			cc := CorpusCase{Endpoint: tc.endpoint, Step: "60s", TagName: "svc"}
			if _, err := fetchGRPCForEndpoint(context.Background(), client, cc, testOpts()); err != nil {
				t.Fatalf("fetchGRPCForEndpoint(%s): %v", tc.endpoint, err)
			}
			if !tc.hit(client) {
				t.Fatalf("endpoint %q dispatched to the wrong RPC", tc.endpoint)
			}
		})
	}
}

func TestFetchGRPCForEndpoint_UnsupportedEndpoint(t *testing.T) {
	t.Parallel()
	for _, ep := range []string{"traces", "traces_v2", "search_recent"} {
		_, err := fetchGRPCForEndpoint(context.Background(), &fakeQuerier{}, CorpusCase{Endpoint: ep}, testOpts())
		if err == nil {
			t.Fatalf("endpoint %q accepted; it has no StreamingQuerier RPC", ep)
		}
		if !strings.Contains(err.Error(), ep) {
			t.Fatalf("error should name the endpoint, got %v", err)
		}
	}
}

func TestFetchGRPC_OpenErrorIsWrappedPerRPC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		endpoint string
		wantWrap string
	}{
		{"search", "open Search stream"},
		{"tags_v1", "open SearchTags stream"},
		{"tags_v2", "open SearchTagsV2 stream"},
		{"tag_values_v1", "open SearchTagValues stream"},
		{"tag_values_v2", "open SearchTagValuesV2 stream"},
		{"metrics_range", "open MetricsQueryRange stream"},
		{"metrics_instant", "open MetricsQueryInstant stream"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()
			client := &fakeQuerier{openErr: errOpen}
			cc := CorpusCase{Endpoint: tc.endpoint, Step: "60s"}
			_, err := fetchGRPCForEndpoint(context.Background(), client, cc, testOpts())
			if err == nil {
				t.Fatalf("open failure swallowed for %s", tc.endpoint)
			}
			if !strings.Contains(err.Error(), tc.wantWrap) {
				t.Fatalf("error = %v, want it wrapped with %q", err, tc.wantWrap)
			}
			if !errors.Is(err, errOpen) {
				t.Fatalf("error = %v, want the cause preserved via %%w", err)
			}
		})
	}
}

func TestFetchGRPC_RecvErrorIsWrappedPerRPC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		endpoint string
		wantWrap string
	}{
		{"search", "recv Search frame"},
		{"tags_v1", "recv SearchTags frame"},
		{"tags_v2", "recv SearchTagsV2 frame"},
		{"tag_values_v1", "recv SearchTagValues frame"},
		{"tag_values_v2", "recv SearchTagValuesV2 frame"},
		{"metrics_range", "recv MetricsQueryRange frame"},
		{"metrics_instant", "recv MetricsQueryInstant frame"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			t.Parallel()
			client := &fakeQuerier{recvErr: errRecv}
			cc := CorpusCase{Endpoint: tc.endpoint, Step: "60s"}
			_, err := fetchGRPCForEndpoint(context.Background(), client, cc, testOpts())
			if err == nil {
				t.Fatalf("mid-drain failure swallowed for %s — a truncated stream would read as a diff", tc.endpoint)
			}
			if !strings.Contains(err.Error(), tc.wantWrap) {
				t.Fatalf("error = %v, want it wrapped with %q", err, tc.wantWrap)
			}
			if !errors.Is(err, errRecv) {
				t.Fatalf("error = %v, want the cause preserved via %%w", err)
			}
		})
	}
}

func TestDiffCaseGRPC_AgreeingBackends(t *testing.T) {
	t.Parallel()
	frames := func() []*tempopb.SearchResponse {
		return []*tempopb.SearchResponse{{Traces: []*tempopb.TraceSearchMetadata{
			{TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootServiceName: "checkout", RootTraceName: "GET /", DurationMs: 5, StartTimeUnixNano: 1000},
		}}}
	}
	tc := CorpusCase{Name: "agree", Endpoint: "search", Query: `{}`, ExpectedMinTraces: 1}
	res := diffCaseGRPC(context.Background(),
		&fakeQuerier{searchFrames: frames()},
		&fakeQuerier{searchFrames: frames()},
		tc, testOpts())
	if res.HardError != "" {
		t.Fatalf("HardError = %q, want none", res.HardError)
	}
	if len(res.Assertions) != 0 {
		t.Fatalf("Assertions = %+v, want none", res.Assertions)
	}
	if !res.Diff.Equal {
		t.Fatalf("Diff.Equal = false, want true for byte-identical streams: %+v", res.Diff)
	}
	if !res.passed() {
		t.Fatal("passed() = false for an agreeing case")
	}
}

func TestDiffCaseGRPC_DivergingBackendsSurfaceADiff(t *testing.T) {
	t.Parallel()
	tempoFrames := []*tempopb.SearchResponse{{Traces: []*tempopb.TraceSearchMetadata{
		{TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RootServiceName: "checkout"},
	}}}
	cerbFrames := []*tempopb.SearchResponse{{Traces: []*tempopb.TraceSearchMetadata{
		{TraceID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RootServiceName: "checkout"},
	}}}
	tc := CorpusCase{Name: "diverge", Endpoint: "search", Query: `{}`}
	res := diffCaseGRPC(context.Background(),
		&fakeQuerier{searchFrames: tempoFrames},
		&fakeQuerier{searchFrames: cerbFrames},
		tc, testOpts())
	if res.HardError != "" {
		t.Fatalf("HardError = %q, want none — the fetch succeeded on both sides", res.HardError)
	}
	if res.Diff.Equal {
		t.Fatal("Diff.Equal = true for backends returning different trace IDs")
	}
	if res.passed() {
		t.Fatal("passed() = true for a diverging case")
	}
}

func TestDiffCaseGRPC_AssertionFailureIsNotAHardError(t *testing.T) {
	t.Parallel()
	// Both backends agree with each other but neither meets the corpus's
	// cardinality floor: that is an assertion, not a transport failure,
	// and it must keep the diff result intact.
	tc := CorpusCase{Name: "too-few", Endpoint: "search", Query: `{}`, ExpectedMinTraces: 2}
	res := diffCaseGRPC(context.Background(), &fakeQuerier{}, &fakeQuerier{}, tc, testOpts())
	if res.HardError != "" {
		t.Fatalf("HardError = %q, want none", res.HardError)
	}
	if len(res.Assertions) != 2 {
		t.Fatalf("Assertions = %+v, want one per backend", res.Assertions)
	}
	if res.passed() {
		t.Fatal("passed() = true despite a failed assertion")
	}
}

func TestDiffCaseGRPC_HardErrorsNameTheFailingSide(t *testing.T) {
	t.Parallel()
	tc := CorpusCase{Name: "boom", Endpoint: "search", Query: `{}`}
	ok := func() *fakeQuerier { return &fakeQuerier{} }
	bad := func() *fakeQuerier { return &fakeQuerier{openErr: errOpen} }

	res := diffCaseGRPC(context.Background(), bad(), ok(), tc, testOpts())
	if !strings.Contains(res.HardError, "tempo grpc:") || strings.Contains(res.HardError, "cerberus grpc:") {
		t.Fatalf("HardError = %q, want tempo-only attribution", res.HardError)
	}

	res = diffCaseGRPC(context.Background(), ok(), bad(), tc, testOpts())
	if !strings.Contains(res.HardError, "cerberus grpc:") || strings.Contains(res.HardError, "tempo grpc:") {
		t.Fatalf("HardError = %q, want cerberus-only attribution", res.HardError)
	}

	res = diffCaseGRPC(context.Background(), bad(), bad(), tc, testOpts())
	if !strings.Contains(res.HardError, "tempo grpc:") || !strings.Contains(res.HardError, "cerberus grpc:") {
		t.Fatalf("HardError = %q, want both sides reported", res.HardError)
	}
	if res.passed() {
		t.Fatal("passed() = true for a case that never reached either backend")
	}
}

func TestWriteReportGRPC_DocumentsSkippedCases(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "report.md")
	results := []CaseResult{{Case: CorpusCase{Name: "ok", Endpoint: "search"}, Diff: Diff{Equal: true}}}
	noRPC := []CorpusCase{
		{Name: "zeta-by-id", Endpoint: "traces"},
		{Name: "alpha-by-id", Endpoint: "traces_v2"},
	}
	statusParity := []CorpusCase{{Name: "bad-syntax", Endpoint: "search"}}

	if err := writeReportGRPC(path, results, noRPC, statusParity); err != nil {
		t.Fatalf("writeReportGRPC: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	report := string(raw)

	if !strings.Contains(report, grpcReportTitle) {
		t.Fatalf("report is missing the gRPC title:\n%s", report)
	}
	if !strings.Contains(report, "## Skipped — no StreamingQuerier RPC for this endpoint") {
		t.Fatalf("no-RPC skip section missing:\n%s", report)
	}
	if !strings.Contains(report, "## Skipped — expect_status axis not yet implemented for gRPC") {
		t.Fatalf("status-parity skip section missing:\n%s", report)
	}
	// The count in the explanation is the section's own case count.
	if !strings.Contains(report, "The 2 case(s) below") || !strings.Contains(report, "The 1 case(s) below") {
		t.Fatalf("per-section counts wrong:\n%s", report)
	}
	// Skipped cases are sorted by name so reruns produce identical bytes.
	alpha := strings.Index(report, "`alpha-by-id`")
	zeta := strings.Index(report, "`zeta-by-id`")
	if alpha < 0 || zeta < 0 || alpha > zeta {
		t.Fatalf("skipped cases not sorted by name (alpha=%d zeta=%d):\n%s", alpha, zeta, report)
	}
	// The skipped cases are documented, not counted as failures.
	if strings.Contains(report, "`zeta-by-id` — ") {
		t.Fatalf("a skipped case leaked into the per-case results:\n%s", report)
	}
}

func TestWriteReportGRPC_NoSkippedSectionsWhenNothingSkipped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.md")
	if err := writeReportGRPC(path, nil, nil, nil); err != nil {
		t.Fatalf("writeReportGRPC: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(raw), "## Skipped") {
		t.Fatalf("a skip section was rendered with no skipped cases:\n%s", raw)
	}
}

func TestWriteReportGRPC_UnwritablePathIsAnError(t *testing.T) {
	t.Parallel()
	// A directory where the report file should be: os.Create fails, and
	// the caller must see it rather than a silently missing report.
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeReportGRPC(path, nil, []CorpusCase{{Name: "x"}}, nil); err == nil {
		t.Fatal("writeReportGRPC returned nil for an unwritable report path")
	}
}

func TestRenderSkippedSection_EmptyListStillRendersTheHeading(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	if err := renderSkippedSection(&sb, nil, "## Heading", "The %d case(s):\n\n"); err != nil {
		t.Fatalf("renderSkippedSection: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "## Heading") || !strings.Contains(out, "The 0 case(s):") {
		t.Fatalf("output = %q", out)
	}
}

// failWriter fails on the Nth write so every error return in
// renderSkippedSection's Fprint ladder is reachable.
type failWriter struct {
	failAt int
	n      int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.n++
	if w.n >= w.failAt {
		return 0, errRecv
	}
	return len(p), nil
}

func TestRenderSkippedSection_PropagatesWriteErrors(t *testing.T) {
	t.Parallel()
	skipped := []CorpusCase{{Name: "a", Endpoint: "traces"}}
	// 5 writes: heading, blank line, explanation, one case line, trailing
	// blank line. Every one of them must surface its error.
	for at := 1; at <= 5; at++ {
		err := renderSkippedSection(&failWriter{failAt: at}, skipped, "## H", "The %d case(s):\n\n")
		if err == nil {
			t.Fatalf("write failure at call %d was swallowed", at)
		}
		if !errors.Is(err, errRecv) {
			t.Fatalf("write failure at call %d returned %v, want the writer's error", at, err)
		}
	}
}

func TestCaseSet_IdentityAndVerdict(t *testing.T) {
	t.Parallel()
	results := []CaseResult{
		{Case: CorpusCase{Name: "agree", Endpoint: "search"}, Diff: Diff{Equal: true}},
		{Case: CorpusCase{Name: "diverge", Endpoint: "tags_v2"}, Diff: Diff{Equal: false}},
		{Case: CorpusCase{Name: "boom", Endpoint: "metrics_range"}, HardError: "dial", Diff: Diff{Equal: true}},
	}
	got := caseSet(results)
	if len(got) != len(results) {
		t.Fatalf("caseSet dropped cases: got %d, want %d — the ratchet gates on WHICH cases failed", len(got), len(results))
	}
	want := []struct {
		id     string
		passed bool
	}{
		{"search | agree", true},
		{"tags_v2 | diverge", false},
		{"metrics_range | boom", false},
	}
	for i, w := range want {
		if got[i].ID != w.id {
			t.Fatalf("case[%d].ID = %q, want %q", i, got[i].ID, w.id)
		}
		if got[i].Passed != w.passed {
			t.Fatalf("case[%d] (%s) Passed = %v, want %v", i, w.id, got[i].Passed, w.passed)
		}
	}
}
