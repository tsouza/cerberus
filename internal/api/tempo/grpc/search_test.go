package grpc_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
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
	// lastSQL / lastArgs capture what the eager search Query received, so
	// window-threading tests can assert the emitted scan is windowed.
	lastSQL  string
	lastArgs []any
	// phaseAStrings records QueryStrings calls; phaseAIDs is what it returns.
	// A non-empty phaseAStrings proves a gRPC structural search ROUTED through
	// the two-phase split; setting phaseAIDs lets phase B run end-to-end.
	phaseAStrings []string
	phaseAIDs     []string
	// block makes Query wait for ctx cancellation then return ctx.Err(); released
	// records it observed the cancellation — so a cancellation test proves the
	// client cancel propagated through SearchResult -> QueryPlan -> Query.
	block    bool
	released atomic.Bool
}

func (s *stubCursorQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	if s.block {
		<-ctx.Done()
		s.released.Store(true)
		return nil, ctx.Err()
	}
	// The eager search routes here (the streaming QueryCursor path is gone). The
	// follow-up root lookup (resolveTraceRoots) emits argMinIf(...); serve
	// rootSamples for it and the main search rows otherwise.
	if strings.Contains(sql, "argMinIf") {
		return s.rootSamples, nil
	}
	s.lastSQL = sql
	s.lastArgs = args
	return s.rows, nil
}

func (s *stubCursorQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	s.phaseAStrings = append(s.phaseAStrings, sql)
	return s.phaseAIDs, nil
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

	// Release the stream ctx. Search now routes through the eager SearchResult
	// (no gRPC-owned cursor to Close — CH resources are released inside the engine
	// when Query returns); the wire contract this test pins is the frame batching
	// above.
	cancel()
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

// TestSearch_CancellationReachesQuery proves a client cancellation propagates
// through the eager SearchResult -> Engine.QueryPlan -> Query drain (the
// coverage the removed gRPC-owned cursor used to give). The stub Query BLOCKS
// until its ctx is cancelled, so the test asserts BOTH that the stream
// terminates promptly AND that the server's Query observed the cancellation
// (released): a broken cancellation path would hang and leave released false.
func TestSearch_CancellationReachesQuery(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{block: true}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream did not terminate after cancel")
	}
	if !waitForBool(&q.released, time.Second) {
		t.Error("server Query never observed ctx cancellation — not propagated to the eager drain")
	}
}

// waitForBool polls an atomic.Bool until true or the deadline elapses.
func waitForBool(b *atomic.Bool, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if b.Load() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return b.Load()
}

// TestSearch_WindowlessDefaultsLookback_SQLShape proves the gRPC
// streaming /search RPC closes the L2 whole-table hazard the HTTP
// /api/search handler closes: a windowless request (Start==End==0) is
// clamped to a recent lookback before lowering, so the trace-limit
// pushdown's inner GROUP BY TraceId scans a window instead of the whole
// table. Without the WithSearchWindow threading + clamp in Search, the
// emitted SQL carries no `Timestamp` bound and the inner aggregation
// runs over every row server-side — the same defect the HTTP path had.
//
// The stub captures the SQL the engine handed the cursor; we assert the
// windowed bound appears on BOTH scans (the inner ranking subquery and
// the outer drain) — count==2 each, the same shape the HTTP SQL test
// pins.
func TestSearch_WindowlessDefaultsLookback_SQLShape(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{rows: nil}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// No Start / End on the request — the windowless degenerate path.
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := drainSearch(t, stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if q.lastSQL == "" {
		t.Fatalf("no SQL captured")
	}
	// The default lookback folds a lower Timestamp bound into BOTH the
	// inner ranking subquery and the outer drain — proving the inner
	// GROUP BY TraceId is windowed, not whole-table. The bound rides as a
	// `fromUnixTimestamp64Nano(?)` placeholder (the value is in lastArgs).
	const wantBound = "`Timestamp` >= fromUnixTimestamp64Nano(?)"
	if got := strings.Count(q.lastSQL, wantBound); got != 2 {
		t.Errorf("windowless gRPC search: want %q on both scans (count 2), got %d:\n%s",
			wantBound, got, q.lastSQL)
	}
	// And the GROUP BY TraceId aggregation must still be present (the
	// trace-limit pushdown), now over the windowed input.
	if !strings.Contains(q.lastSQL, "GROUP BY `TraceId`") {
		t.Errorf("windowless gRPC search SQL missing GROUP BY TraceId:\n%s", q.lastSQL)
	}
}

// TestSearch_ExplicitWindow_Honored proves the clamp fires ONLY on the
// both-absent path: an explicit Start / End on the SearchRequest reaches
// the emitted SQL verbatim (the default does not override a supplied
// window). SearchRequest.Start / End are uint32 Unix seconds; we send
// 1_700_000_000s / 1_700_003_600s and assert the exact nanosecond bounds
// (seconds * 1e9) appear in the SQL.
func TestSearch_ExplicitWindow_Honored(t *testing.T) {
	t.Parallel()
	q := &stubCursorQuerier{rows: nil}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	const (
		startSec = uint32(1_700_000_000)
		endSec   = uint32(1_700_003_600)
	)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{
		Query: "{}",
		Start: startSec,
		End:   endSec,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := drainSearch(t, stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if q.lastSQL == "" {
		t.Fatalf("no SQL captured")
	}
	// The bound folds on both scans as a placeholder; the explicit nanos
	// (seconds * 1e9) must be the bound args, proving the supplied window
	// — not a now()-relative lookback — reached the query.
	const ge = "`Timestamp` >= fromUnixTimestamp64Nano(?)"
	const le = "`Timestamp` <= fromUnixTimestamp64Nano(?)"
	if got := strings.Count(q.lastSQL, ge); got != 2 {
		t.Errorf("explicit window must fold on both scans (2x %q), got %d:\n%s", ge, got, q.lastSQL)
	}
	if got := strings.Count(q.lastSQL, le); got != 2 {
		t.Errorf("explicit window must fold on both scans (2x %q), got %d:\n%s", le, got, q.lastSQL)
	}
	startNanos := int64(startSec) * 1_000_000_000
	endNanos := int64(endSec) * 1_000_000_000
	var sawStart, sawEnd bool
	for _, a := range q.lastArgs {
		if v, ok := a.(int64); ok {
			if v == startNanos {
				sawStart = true
			}
			if v == endNanos {
				sawEnd = true
			}
		}
	}
	if !sawStart || !sawEnd {
		t.Errorf("explicit window args not honored verbatim: sawStart=%v sawEnd=%v args=%v",
			sawStart, sawEnd, q.lastArgs)
	}
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

// TestSearch_StructuralRoutesTwoPhase is the non-vacuity pin: a recursive
// structural query over gRPC must ROUTE through the same two-phase split as HTTP
// (phase A ranks via QueryStrings), not silently fall through to a single wide
// drain. Without the shared SearchResult wiring this passes vacuously; with it,
// phase A fires. (Phase A returns no ids from the stub, so phase B is skipped and
// the result is empty — we assert the ROUTING, not the rows.)
func TestSearch_StructuralRoutesTwoPhase(t *testing.T) {
	t.Parallel()
	ts := time.Now()
	// phaseAIDs = the top-N ranking phase A returns (non-empty so phase B runs);
	// rows = the wide phase-B hydrate for those traces. Proves the split runs
	// END-TO-END over gRPC, not just that phase A fires.
	q := &stubCursorQuerier{
		phaseAIDs: []string{padHexTraceID(1), padHexTraceID(2)},
		rows: []chclient.Sample{
			makeSearchRow(padHexTraceID(1), "op", "a", ts, 1),
			makeSearchRow(padHexTraceID(2), "op", "a", ts, 1),
		},
	}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)
	stream, err := client.Search(context.Background(), &tempopb.SearchRequest{
		Query: `{ resource.service.name = "a" } >> { resource.service.name = "b" }`,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	frames, err := drainSearch(t, stream)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// Phase A fired — routed through the two-phase split, not a single wide query.
	if len(q.phaseAStrings) == 0 {
		t.Fatal("gRPC structural search did not route through the two-phase split (phase A never fired)")
	}
	// Phase B hydrated + shaped end-to-end into trace summaries over the stream.
	traces := 0
	for _, f := range frames {
		traces += len(f.Traces)
	}
	if traces != 2 {
		t.Errorf("gRPC structural two-phase returned %d traces, want 2 (phase B did not hydrate+shape end-to-end)", traces)
	}
}
