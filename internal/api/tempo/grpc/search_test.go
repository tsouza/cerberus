package grpc_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
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

// stubCursorQuerier is the in-test ClickHouse client cerberus's gRPC
// Search RPC drives against. It satisfies both engine.Querier
// (Query) and engine.CursorQuerier (QueryCursor) — the gRPC Search
// path opens a streaming cursor, then issues an eager Query for the
// follow-up root-resolution lookup, so the stub has to answer both.
//
// rowsByQuery routes the cursor open call by SQL-substring match so
// the same stub serves both the canonical search-path SQL (the
// first QueryCursor) and any follow-up Query the resolveTraceRoots
// path issues (matched via the "argMinIf" needle that appears in the
// root-lookup SQL).
type stubCursorQuerier struct {
	rows []chclient.Sample
	// rootSamples are returned by the follow-up Query that
	// resolveTraceRoots fires for traces whose result set lacked a
	// real root span. Empty / nil here means "no roots to patch",
	// matching the steady-state happy path.
	rootSamples []chclient.Sample
	// closed flips to 1 once the cursor's Close has been invoked.
	// Tests assert against it to prove that cancellation reached the
	// streaming cursor.
	closed atomic.Int32
}

func (s *stubCursorQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.rootSamples, nil
}

func (s *stubCursorQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (s *stubCursorQuerier) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return &stubCursor{rows: s.rows, ctx: ctx, closed: &s.closed}, nil
}

// stubCursor walks rowsByQuery one Sample at a time. Honours ctx
// cancellation so the gRPC Search RPC can prove that a client
// disconnect mid-stream surfaces as a cursor termination.
type stubCursor struct {
	rows   []chclient.Sample
	i      int
	cur    chclient.Sample
	err    error
	ctx    context.Context
	closed *atomic.Int32
}

func (c *stubCursor) Next() bool {
	// Mid-iteration context cancellation: shortcut Next to surface the
	// ctx error via Err(). Matches the rowsCursor production shape
	// where a cancelled driver.Rows reports its cause through Err.
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return false
	}
	if c.i >= len(c.rows) {
		return false
	}
	c.cur = c.rows[c.i]
	c.i++
	return true
}

func (c *stubCursor) Sample() chclient.Sample { return c.cur }
func (c *stubCursor) Err() error              { return c.err }
func (c *stubCursor) Close() error {
	c.closed.Store(1)
	return nil
}

// makeSearchRow returns one synthetic chclient.Sample shaped like the
// canonical wrap-projection output: MetricName carries SpanName, the
// reserved __cerberus_traceID label carries the 32-char hex TraceID,
// Timestamp carries the span start, Value carries the per-row span
// duration in ns. Spans are root-anchored (ParentSpanID == "0" — the
// post-strip form of 0000000000000000) so toTraceSummaries doesn't
// flag the trace as missing-root.
func makeSearchRow(traceID, spanName, service string, ts time.Time, durationMs int64) chclient.Sample {
	return chclient.Sample{
		MetricName: spanName,
		Labels: map[string]string{
			"service.name":               service,
			"__cerberus_traceID":         traceID,
			"__cerberus_parentSpanID":    "0",
			"__cerberus_traceDurationNs": fmt.Sprintf("%d", durationMs*1_000_000),
		},
		Timestamp: ts,
		Value:     float64(durationMs * 1_000_000),
	}
}

// padHexTraceID returns a 32-char hex string of the form NNN... so each
// synthetic trace gets a unique stable TraceID without re-implementing
// hex encoding semantics in the test.
func padHexTraceID(i int) string {
	return fmt.Sprintf("%032x", i+1)
}

// dialServer wires a Service to a bufconn listener and returns the
// dialled tempopb.StreamingQuerierClient + a cleanup func. Used by
// the Search RPC tests to keep the boilerplate per-case overhead low.
func dialServer(t *testing.T, q *stubCursorQuerier) (tempopb.StreamingQuerierClient, func()) {
	t.Helper()
	handler := tempo.New(q, schema.DefaultOTelTraces(), "test", nil)
	svc := tempogrpc.NewService(handler, nil, nil)
	srv := tempogrpc.NewServer(svc)
	lis := bufconn.Listen(1 << 20)
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc Serve returned: %v", err)
		}
	}()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
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

// drainSearch consumes the streaming response and returns the
// collected frames in arrival order. Stops at io.EOF (clean end of
// stream); any other RecvMsg error is returned as-is so callers can
// assert on it.
func drainSearch(t *testing.T, stream tempopb.StreamingQuerier_SearchClient) ([]*tempopb.SearchResponse, error) {
	t.Helper()
	var frames []*tempopb.SearchResponse
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
	}
}

// TestSearch_FrameBatching feeds 45 synthetic traces through the gRPC
// Search RPC and asserts the wire-level frame layout: 2 full shards
// of 20 traces each + 1 tail shard with the remaining 5, where the
// tail carries the SearchMetrics with InspectedTraces = 45.
//
// 45 = 2*searchFrameSize + 5 is picked so the test exercises both the
// "shard fills exactly" path (Push returns ready=true) and the "tail
// rolls over" path (Tail returns the partial buffer). Frame count =
// ceil(45/20) = 3 — the design-doc §4 contract.
func TestSearch_FrameBatching(t *testing.T) {
	t.Parallel()

	const total = 45
	rows := make([]chclient.Sample, 0, total)
	ts := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		rows = append(rows, makeSearchRow(
			padHexTraceID(i), "GET /api", "svc",
			ts.Add(time.Duration(i)*time.Millisecond), 10,
		))
	}
	q := &stubCursorQuerier{rows: rows}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Limit must cover all 45 traces — Search honours
	// SearchRequest.Limit (default 20, mirroring HTTP `limit`), which
	// would otherwise truncate the stream to a single frame.
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}", Limit: total})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Expected: 3 frames total — two full 20-trace shards + a 5-trace
	// tail that also carries SearchMetrics.
	if got, want := len(frames), 3; got != want {
		t.Fatalf("frame count: got %d, want %d", got, want)
	}
	if got := len(frames[0].Traces); got != 20 {
		t.Errorf("frame[0] traces: got %d, want 20", got)
	}
	if got := len(frames[1].Traces); got != 20 {
		t.Errorf("frame[1] traces: got %d, want 20", got)
	}
	if got := len(frames[2].Traces); got != 5 {
		t.Errorf("frame[2] (tail) traces: got %d, want 5", got)
	}
	// Metrics live on the tail frame exclusively.
	if frames[0].Metrics != nil {
		t.Errorf("frame[0] metrics: want nil, got %+v", frames[0].Metrics)
	}
	if frames[1].Metrics != nil {
		t.Errorf("frame[1] metrics: want nil, got %+v", frames[1].Metrics)
	}
	if frames[2].Metrics == nil {
		t.Fatalf("frame[2] (tail) metrics: want non-nil")
	}
	if got, want := int(frames[2].Metrics.InspectedTraces), total; got != want {
		t.Errorf("frame[2] InspectedTraces: got %d, want %d", got, want)
	}
	// Sum-check: total traces across all frames matches the input.
	sum := 0
	for _, f := range frames {
		sum += len(f.Traces)
	}
	if sum != total {
		t.Errorf("traces across frames: got %d, want %d", sum, total)
	}

	// Cursor.Close MUST be called by the Search handler so CH
	// resources don't leak. Cancel the context first so any
	// in-flight goroutine drains, then check.
	cancel()
	if !waitForClose(&q.closed, time.Second) {
		t.Errorf("cursor.Close was not invoked")
	}
}

// TestSearch_EmptyResultSet asserts the zero-row happy path: a search
// against a CH cursor that returns 0 rows still produces exactly one
// frame (the tail with InspectedTraces=0 + empty Traces slice).
// Grafana's Tempo datasource parses the metrics from the last frame
// regardless of trace count, so this is the wire-format contract for
// "no results found".
func TestSearch_EmptyResultSet(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{rows: nil}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, want := len(frames), 1; got != want {
		t.Fatalf("frame count: got %d, want %d (tail-only)", got, want)
	}
	if got := len(frames[0].Traces); got != 0 {
		t.Errorf("frame[0] traces: got %d, want 0", got)
	}
	if frames[0].Metrics == nil {
		t.Fatalf("frame[0] metrics: want non-nil with InspectedTraces=0")
	}
	if got := int(frames[0].Metrics.InspectedTraces); got != 0 {
		t.Errorf("InspectedTraces: got %d, want 0", got)
	}
}

// TestSearch_EmptyQueryHealthCheck asserts the empty-query Grafana
// health-check path — the gRPC analogue of the HTTP /api/search
// behaviour where an empty `q` returns a 200 + empty traces. Over
// gRPC we emit one frame with empty Traces + zero-metrics so the
// streaming health-check stays semantically identical to the HTTP one.
func TestSearch_EmptyQueryHealthCheck(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{rows: nil}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: ""})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, want := len(frames), 1; got != want {
		t.Fatalf("frame count: got %d, want %d", got, want)
	}
	if len(frames[0].Traces) != 0 {
		t.Errorf("expected empty traces, got %d", len(frames[0].Traces))
	}
}

// TestSearch_TraceMetadataPivot pins the conversion from cerberus's
// tempo.TraceSummary into the wire-format tempopb.TraceSearchMetadata.
// The mapping is field-for-field (TraceID / RootServiceName /
// RootTraceName / startTimeUnixNano / durationMs) so a search query
// over the streaming path produces the same identity + timing the
// HTTP path returns; this test pins the conversion so a future field
// reorder is caught at the gRPC boundary, not by the e2e smoke.
func TestSearch_TraceMetadataPivot(t *testing.T) {
	t.Parallel()
	const traceID = "0123456789abcdef0123456789abcdef"
	ts := time.Date(2026, 5, 19, 12, 0, 0, 123_456_789, time.UTC)
	rows := []chclient.Sample{
		makeSearchRow(traceID, "GET /api/users", "frontend", ts, 250),
	}
	q := &stubCursorQuerier{rows: rows}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(frames) != 1 || len(frames[0].Traces) != 1 {
		t.Fatalf("expected 1 frame with 1 trace, got %d frames, frame[0] traces=%d",
			len(frames), len(frames[0].Traces))
	}
	tr := frames[0].Traces[0]
	if tr.TraceID != traceID {
		t.Errorf("TraceID: got %q, want %q", tr.TraceID, traceID)
	}
	if tr.RootServiceName != "frontend" {
		t.Errorf("RootServiceName: got %q, want %q", tr.RootServiceName, "frontend")
	}
	if tr.RootTraceName != "GET /api/users" {
		t.Errorf("RootTraceName: got %q, want %q", tr.RootTraceName, "GET /api/users")
	}
	if got, want := tr.StartTimeUnixNano, uint64(ts.UnixNano()); got != want {
		t.Errorf("StartTimeUnixNano: got %d, want %d", got, want)
	}
	if tr.DurationMs != 250 {
		t.Errorf("DurationMs: got %d, want 250", tr.DurationMs)
	}
}

// TestSearch_CancellationPropagatesToCursor wires a slow synthetic
// cursor (1 row every 50ms), cancels the client context mid-stream,
// and asserts that cursor.Close was invoked. This is the proof that
// gRPC stream context cancellation flows through to the CH cursor's
// resource-release path — the property the design-doc §4 calls out
// as "Cancellation: stream.Context() cancels on client disconnect;
// pass into Engine.QueryPlanCursor."
func TestSearch_CancellationPropagatesToCursor(t *testing.T) {
	t.Parallel()
	// Enough rows that no realistic in-flight drain ever exhausts the
	// cursor before the test cancels — we want the cancel to be
	// observable, not a race with the natural end-of-stream.
	rows := make([]chclient.Sample, 0, 1000)
	ts := time.Now()
	for i := 0; i < 1000; i++ {
		rows = append(rows, makeSearchRow(padHexTraceID(i), "GET /api", "svc", ts, 1))
	}
	q := &slowCursorQuerier{
		stubCursorQuerier: stubCursorQuerier{rows: rows},
		rowDelay:          20 * time.Millisecond,
	}
	client, cleanup := dialServer(t, &q.stubCursorQuerier)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// Pump the stream in a goroutine so we can cancel mid-drain and
	// observe Close from the test thread.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	// Give the server a moment to start draining, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream did not terminate after cancel")
	}

	if !waitForClose(&q.closed, time.Second) {
		t.Errorf("cursor.Close was not invoked after cancellation")
	}
}

// slowCursorQuerier is stubCursorQuerier with a delay between
// Next() calls so the CancellationPropagatesToCursor test has
// enough wall-clock to interrupt mid-stream. The embedded
// stubCursorQuerier carries the rows + closed-counter; the delay
// is layered on by wrapping QueryCursor's returned cursor.
type slowCursorQuerier struct {
	stubCursorQuerier
	rowDelay time.Duration
}

func (s *slowCursorQuerier) QueryCursor(ctx context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return &slowCursor{
		stubCursor: stubCursor{rows: s.rows, ctx: ctx, closed: &s.closed},
		delay:      s.rowDelay,
	}, nil
}

type slowCursor struct {
	stubCursor
	delay time.Duration
}

func (c *slowCursor) Next() bool {
	if c.i > 0 {
		// Honour ctx cancellation while sleeping so the cancel
		// propagates promptly (a naked time.Sleep wouldn't return).
		select {
		case <-time.After(c.delay):
		case <-c.ctx.Done():
			c.err = c.ctx.Err()
			return false
		}
	}
	return c.stubCursor.Next()
}

func (c *slowCursor) Sample() chclient.Sample { return c.stubCursor.Sample() }
func (c *slowCursor) Err() error              { return c.stubCursor.Err() }
func (c *slowCursor) Close() error            { return c.stubCursor.Close() }

// waitForClose polls the closed counter for up to deadline so the
// test doesn't race the goroutine that owns the cursor. Returns true
// once Close has been observed; false when the deadline expires.
func waitForClose(c *atomic.Int32, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if c.Load() == 1 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.Load() == 1
}

// TestSearch_ParseErrorMapsToInvalidArgument confirms the gRPC error-
// mapping table from the design doc §3: a TraceQL parser failure
// surfaces as codes.InvalidArgument (user-facing query error), not
// the default codes.Internal. The HTTP equivalent returns 400 (see
// classifySearchErr) — both heads converge on "user typed a broken
// query."
func TestSearch_ParseErrorMapsToInvalidArgument(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// A non-empty but syntactically invalid TraceQL query — the
	// upstream parser rejects it before lowering, so the engine
	// surfaces ErrParseStage and the gRPC handler maps to
	// codes.InvalidArgument.
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{ unclosed"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_, recvErr := drainSearch(t, stream)
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

// TestSearch_SpanSetsLimitAndSpss pins the gRPC mirror of the HTTP
// /api/search spanSets contract: SearchRequest.SpansPerSpanSet caps
// the spans per spanset (Matched keeps the uncapped total),
// SearchRequest.Limit truncates the newest-first trace list, and the
// proto envelope carries both SpanSets and the legacy SpanSet pair —
// the same matched-span lists Grafana's tableType='spans' transform
// reads on the JSON path.
func TestSearch_SpanSetsLimitAndSpss(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
	spanRow := func(traceID, spanID, name string, at time.Time, durNs int64) chclient.Sample {
		return chclient.Sample{
			MetricName: name,
			Labels: map[string]string{
				"service.name":            "svc",
				"__cerberus_traceID":      traceID,
				"__cerberus_parentSpanID": "0000000000000000",
				"__cerberus_spanID":       spanID,
			},
			Timestamp: at,
			Value:     float64(durNs),
		}
	}
	traceOld := padHexTraceID(1)
	traceNew := padHexTraceID(2)
	rows := []chclient.Sample{
		// Older trace — must fall off under Limit=1.
		spanRow(traceOld, "0000000000000001", "old.root", ts, 5_000_000),
		// Newer trace with two matched spans — SpansPerSpanSet=1 keeps
		// the earlier one, Matched stays 2.
		spanRow(traceNew, "000000000000000a", "new.root", ts.Add(time.Second), 7_000_000),
		spanRow(traceNew, "000000000000000b", "new.child", ts.Add(time.Second+time.Millisecond), 3_000_000),
	}
	q := &stubCursorQuerier{rows: rows}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{
		Query:           "{}",
		Limit:           1,
		SpansPerSpanSet: 1,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	var traces []*tempopb.TraceSearchMetadata
	for _, f := range frames {
		traces = append(traces, f.Traces...)
	}
	if len(traces) != 1 {
		t.Fatalf("Limit=1: got %d traces, want 1", len(traces))
	}
	tr := traces[0]
	if tr.TraceID != traceNew {
		t.Errorf("TraceID: got %q, want %q (newest-first ordering before Limit truncation)", tr.TraceID, traceNew)
	}
	if len(tr.SpanSets) != 1 {
		t.Fatalf("SpanSets: got %d sets, want 1", len(tr.SpanSets))
	}
	set := tr.SpanSets[0]
	if got, want := int(set.Matched), 2; got != want {
		t.Errorf("Matched: got %d, want %d (uncapped total)", got, want)
	}
	if len(set.Spans) != 1 {
		t.Fatalf("SpansPerSpanSet=1: got %d spans, want 1", len(set.Spans))
	}
	sp := set.Spans[0]
	if sp.SpanID != "000000000000000a" {
		t.Errorf("span identity: got %q, want 000000000000000a", sp.SpanID)
	}
	// Reference Tempo emits no span name inside search spanSets; the
	// proto mirror must stay empty too (compat-differ-pinned).
	if sp.Name != "" {
		t.Errorf("span name: got %q, want empty", sp.Name)
	}
	if got, want := sp.StartTimeUnixNano, uint64(ts.Add(time.Second).UnixNano()); got != want {
		t.Errorf("StartTimeUnixNano: got %d, want %d", got, want)
	}
	if got, want := sp.DurationNanos, uint64(7_000_000); got != want {
		t.Errorf("DurationNanos: got %d, want %d", got, want)
	}
	// Legacy single-set field mirrors SpanSets[0].
	if tr.SpanSet == nil || len(tr.SpanSet.Spans) != 1 || tr.SpanSet.Spans[0].SpanID != sp.SpanID {
		t.Errorf("legacy SpanSet must mirror SpanSets[0]; got %+v", tr.SpanSet)
	}
}
