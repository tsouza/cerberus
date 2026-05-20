package grpc_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// metricsFixtureStartNs / metricsFixtureEndNs mirror the canonical
// fixtureStart / fixtureEnd anchors from the HTTP metrics tests
// (2026-05-12T10:00:00Z + 3 minutes) but in the nanosecond units the
// gRPC tempopb.QueryRangeRequest / QueryInstantRequest wire fields
// expect.
const (
	metricsFixtureStartNs uint64 = 1778580000 * 1_000_000_000
	metricsFixtureEndNs   uint64 = 1778580180 * 1_000_000_000
	metricsFixtureStepNs  uint64 = 60 * 1_000_000_000
)

// metricsFakeQuerier is a tempo.Querier stub the metrics gRPC tests
// drive. It returns a fixed []chclient.Sample (`samples`) from Query,
// plus an optional `err` that maps to the engine's "execute" stage —
// the gRPC handler then maps to codes.Internal or codes.Unavailable
// depending on the error value. QueryStrings is unused by the metrics
// RPCs; QueryCursor is unused too (the metrics path doesn't use the
// streaming cursor — it's an eager-drain pivot).
type metricsFakeQuerier struct {
	mu       sync.Mutex
	samples  []chclient.Sample
	err      error
	delay    time.Duration
	lastSQLs []string
}

func (m *metricsFakeQuerier) Query(ctx context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	m.mu.Lock()
	m.lastSQLs = append(m.lastSQLs, sql)
	delay := m.delay
	err := m.err
	rows := append([]chclient.Sample(nil), m.samples...)
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (m *metricsFakeQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, errors.New("metricsFakeQuerier.QueryStrings unused in metrics RPC tests")
}

// newMetricsTestServer wires up the bufconn-based gRPC plumbing the
// metrics RPCs need: a fake querier → cerberus tempo.Handler → gRPC
// Service → grpc.Server on bufconn → client. Returns the dialled
// client + a cleanup func.
func newMetricsTestServer(t *testing.T, q tempo.Querier) (tempopb.StreamingQuerierClient, func()) {
	t.Helper()
	handler := tempo.New(q, schema.DefaultOTelTraces(), "test-grpc", nil)
	service := tempogrpc.NewService(handler, nil, nil)
	srv := tempogrpc.NewServer(service)
	lis := bufconn.Listen(1 << 20)
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc Serve returned: %v", err)
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	return tempopb.NewStreamingQuerierClient(conn), func() {
		_ = conn.Close()
		srv.GracefulStop()
	}
}

// keyValueValue pulls the AnyValue.StringValue from a v1.KeyValue,
// folding the AnyValue oneof switch into a single test-side accessor.
// The metrics RPCs only emit string-variant AnyValues (TraceQL group-by
// + intrinsic labels stringify on the SQL side), so we don't need a
// full type switch.
func keyValueValue(kv v1.KeyValue) string {
	if kv.Value == nil {
		return ""
	}
	if s, ok := kv.Value.Value.(*v1.AnyValue_StringValue); ok {
		return s.StringValue
	}
	return ""
}

// TestMetricsQueryRange_FrameShape feeds a single observed-bucket
// fixture through MetricsQueryRange and asserts the wire envelope: one
// frame, one TimeSeries, label set populated with the synthetic
// `__name__=rate` for an ungrouped query, samples sorted ascending,
// zero-fill of the trailing 4th anchor at value 0.
//
// 3-minute fixture window @ 60s step → 4 anchors after zero-fill
// (10:00 / 10:01 / 10:02 / 10:03); the stub returns observed values at
// the first three; the fill injects a zero at the trailing anchor —
// same contract the HTTP TestMetricsQueryRange_SingleSeriesNoGroupBy
// pins on the JSON envelope, replayed across the gRPC wire shape.
func TestMetricsQueryRange_FrameShape(t *testing.T) {
	t.Parallel()
	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &metricsFakeQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(2), Value: 2.0},
		{Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(0), Value: 0.5},
		{Labels: map[string]string{"__name__": "rate"}, Timestamp: ts(1), Value: 1.5},
	}}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
		Query: "{} | rate()",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
		Step:  metricsFixtureStepNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	frames := drainRangeFrames(t, stream)
	// Range mode emits exactly one frame after eager drain
	// (design-doc §6 — frame batching does not apply).
	if got, want := len(frames), 1; got != want {
		t.Fatalf("frame count: got %d, want %d (range mode is single-frame)", got, want)
	}
	if got, want := len(frames[0].Series), 1; got != want {
		t.Fatalf("series count: got %d, want %d", got, want)
	}
	got := frames[0].Series[0]
	if len(got.Labels) != 1 || got.Labels[0].Key != "__name__" || keyValueValue(got.Labels[0]) != "rate" {
		t.Errorf("labels: want single {__name__=rate}, got %+v", got.Labels)
	}
	if len(got.Samples) != 4 {
		t.Fatalf("samples: want 4 (3 observed + 1 zero-fill), got %d: %+v", len(got.Samples), got.Samples)
	}
	for i, want := range []float64{0.5, 1.5, 2.0, 0.0} {
		if got.Samples[i].Value != want {
			t.Errorf("sample[%d].Value: got %v, want %v", i, got.Samples[i].Value, want)
		}
	}
	for i := 1; i < len(got.Samples); i++ {
		if got.Samples[i-1].TimestampMs >= got.Samples[i].TimestampMs {
			t.Errorf("samples not sorted ascending by timestamp: %+v", got.Samples)
		}
	}
}

// TestMetricsQueryInstant_FrameShape asserts the instant wire shape
// diverges from range as documented: InstantSeries has a scalar Value
// (no Samples slice). With step = end - start the matrix RangeWindow
// emits exactly one anchor per series; the instant projection picks
// that single sample's value as the series scalar.
func TestMetricsQueryInstant_FrameShape(t *testing.T) {
	t.Parallel()
	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	q := &metricsFakeQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"resource.service.name": "frontend"}, Timestamp: ts(0), Value: 12},
		{Labels: map[string]string{"resource.service.name": "backend"}, Timestamp: ts(0), Value: 3},
	}}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryInstant(ctx, &tempopb.QueryInstantRequest{
		Query: "{} | count_over_time() by (resource.service.name)",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	frames := drainInstantFrames(t, stream)
	if got, want := len(frames), 1; got != want {
		t.Fatalf("frame count: got %d, want %d (instant is single-frame)", got, want)
	}
	if got, want := len(frames[0].Series), 2; got != want {
		t.Fatalf("series count: got %d, want %d", got, want)
	}
	// Series are sorted by canonical label key (deterministic
	// emission); backend < frontend lexicographically.
	values := map[string]float64{}
	for _, s := range frames[0].Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "resource.service.name" {
			t.Errorf("series labels: want single resource.service.name, got %+v", s.Labels)
			continue
		}
		values[keyValueValue(s.Labels[0])] = s.Value
	}
	if got, want := values["frontend"], 12.0; got != want {
		t.Errorf("frontend value: got %v, want %v", got, want)
	}
	if got, want := values["backend"], 3.0; got != want {
		t.Errorf("backend value: got %v, want %v", got, want)
	}
}

// TestMetricsQueryRange_ParseError confirms the gRPC error-mapping
// table from the design doc §3: a TraceQL parser failure surfaces as
// codes.InvalidArgument (user-facing query error), not codes.Internal.
// The HTTP equivalent returns 400 (see classifyMetricsQueryRangeErr) —
// both surfaces converge on "user typed a broken metrics query."
func TestMetricsQueryRange_ParseError(t *testing.T) {
	t.Parallel()
	q := &metricsFakeQuerier{}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
		Query: "{ unclosed",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
		Step:  metricsFixtureStepNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatalf("want error, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("want grpc status, got %v", recvErr)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code: got %s, want InvalidArgument (err=%v)", st.Code(), recvErr)
	}
}

// TestMetricsQueryInstant_Cancellation wires a slow CH stub, cancels
// the client context mid-flight, and asserts the stream terminates
// with a cancellation status. This is the proof that gRPC stream
// context cancellation flows through Handler.ExecMetricsInstant →
// engine.QueryPlan → Client.Query (the slow ctx-aware path).
//
// We can't observe a synthetic-cursor counter the way PR 2's search
// path does (the metrics RPCs use Query, not QueryCursor); instead
// we assert on the propagated status — either codes.Canceled (the
// gRPC transport observed the client disconnect first) or
// codes.Internal wrapping ctx.Err() (the server-side Query returned
// before the transport observed cancel). Both are valid outcomes —
// the test pins "either".
func TestMetricsQueryInstant_Cancellation(t *testing.T) {
	t.Parallel()
	q := &metricsFakeQuerier{
		samples: []chclient.Sample{
			{Labels: map[string]string{"__name__": "count_over_time"}, Timestamp: time.Now(), Value: 1},
		},
		// Long enough that the cancel arrives before Query returns
		// in steady state on any reasonable test host.
		delay: 500 * time.Millisecond,
	}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.MetricsQueryInstant(ctx, &tempopb.QueryInstantRequest{
		Query: "{} | count_over_time()",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// Cancel mid-flight.
	time.AfterFunc(50*time.Millisecond, cancel)

	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatalf("want cancellation error, got nil response")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("want grpc status, got %v", recvErr)
	}
	// Either codes.Canceled (transport saw cancel first) or
	// codes.Internal wrapping context.Canceled (engine surfaced
	// ctx.Err() as an execute-stage error first). Both prove the
	// cancellation flowed through.
	if st.Code() != codes.Canceled && st.Code() != codes.Internal {
		t.Errorf("code: got %s, want Canceled or Internal", st.Code())
	}
	if st.Code() == codes.Internal && !strings.Contains(st.Message(), "context canceled") {
		t.Errorf("Internal status message: want to mention 'context canceled', got %q", st.Message())
	}
}

// TestMetricsQueryRange_CircuitOpen confirms chclient.ErrCircuitOpen
// surfaces as codes.Unavailable rather than the default codes.Internal.
// This is the gRPC equivalent of the HTTP 503 + Retry-After path —
// the design-doc §3 calls out Unavailable as the wire-canonical "ask
// the client to retry later" response.
func TestMetricsQueryRange_CircuitOpen(t *testing.T) {
	t.Parallel()
	q := &metricsFakeQuerier{err: chclient.ErrCircuitOpen}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
		Query: "{} | rate()",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
		Step:  metricsFixtureStepNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatalf("want error, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("want grpc status, got %v", recvErr)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("code: got %s, want Unavailable (err=%v)", st.Code(), recvErr)
	}
}

// TestMetricsQueryRange_NonMetricsQuery asserts the parse-then-lower
// path's "not a metrics-pipeline expression" rejection: a bare TraceQL
// query (no `| rate()` / `| *_over_time()` tail) returns InvalidArgument
// rather than the default Internal. The HTTP handler returns 400 on
// the same shape (see handleMetricsQueryRange's "not a TraceQL
// metrics-pipeline expression" error).
func TestMetricsQueryRange_NonMetricsQuery(t *testing.T) {
	t.Parallel()
	q := &metricsFakeQuerier{}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
		Query: "{ name=\"foo\" }",
		Start: metricsFixtureStartNs,
		End:   metricsFixtureEndNs,
		Step:  metricsFixtureStepNs,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatalf("want error, got nil")
	}
	st, ok := status.FromError(recvErr)
	if !ok {
		t.Fatalf("want grpc status, got %v", recvErr)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code: got %s, want InvalidArgument (err=%v)", st.Code(), recvErr)
	}
}

// TestMetricsQueryRange_BadInputs pins the request-validation surface:
// missing query / start / end / step / inverted-bounds all map to
// codes.InvalidArgument rather than letting the engine see a malformed
// request.
func TestMetricsQueryRange_BadInputs(t *testing.T) {
	t.Parallel()
	q := &metricsFakeQuerier{}
	client, cleanup := newMetricsTestServer(t, q)
	t.Cleanup(cleanup)

	cases := []struct {
		name string
		req  *tempopb.QueryRangeRequest
	}{
		{"missing query", &tempopb.QueryRangeRequest{Start: metricsFixtureStartNs, End: metricsFixtureEndNs, Step: metricsFixtureStepNs}},
		{"zero start", &tempopb.QueryRangeRequest{Query: "{} | rate()", End: metricsFixtureEndNs, Step: metricsFixtureStepNs}},
		{"zero end", &tempopb.QueryRangeRequest{Query: "{} | rate()", Start: metricsFixtureStartNs, Step: metricsFixtureStepNs}},
		{"zero step", &tempopb.QueryRangeRequest{Query: "{} | rate()", Start: metricsFixtureStartNs, End: metricsFixtureEndNs}},
		{"end before start", &tempopb.QueryRangeRequest{Query: "{} | rate()", Start: metricsFixtureEndNs, End: metricsFixtureStartNs, Step: metricsFixtureStepNs}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.MetricsQueryRange(ctx, tc.req)
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			_, recvErr := stream.Recv()
			if recvErr == nil {
				t.Fatalf("want error, got nil")
			}
			st, ok := status.FromError(recvErr)
			if !ok {
				t.Fatalf("want grpc status, got %v", recvErr)
			}
			if st.Code() != codes.InvalidArgument {
				t.Errorf("code: got %s, want InvalidArgument (err=%v)", st.Code(), recvErr)
			}
		})
	}
}

// drainRangeFrames pulls every QueryRangeResponse frame off the
// streaming RPC until EOF. Mirrors PR 2's drainSearch helper but for
// the metrics-range envelope.
func drainRangeFrames(t *testing.T, stream tempopb.StreamingQuerier_MetricsQueryRangeClient) []*tempopb.QueryRangeResponse {
	t.Helper()
	var frames []*tempopb.QueryRangeResponse
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			return frames
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		frames = append(frames, f)
	}
}

// drainInstantFrames is the instant-side sibling of drainRangeFrames.
// Returns the collected QueryInstantResponse frames in arrival order.
func drainInstantFrames(t *testing.T, stream tempopb.StreamingQuerier_MetricsQueryInstantClient) []*tempopb.QueryInstantResponse {
	t.Helper()
	var frames []*tempopb.QueryInstantResponse
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			return frames
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		frames = append(frames, f)
	}
}
