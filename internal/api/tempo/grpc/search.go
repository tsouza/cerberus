package grpc

import (
	"errors"
	"strconv"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/traceql"
)

// searchFrameSize caps the number of TraceSearchMetadata entries
// cerberus packs into one tempopb.SearchResponse before flushing it
// down the gRPC stream. Sized to amortise the per-Send framing
// overhead (~30B/Send on HTTP/2) without overshooting the default
// per-stream HTTP/2 window (64 KiB) — 20 trace summaries at ~1 KB each
// land at ~20 KB per frame, comfortably under the window. The exact
// value isn't load-bearing; the design-doc §4 picks 20 as the search-
// path counterpart to MetricsQueryRange's 50-series-per-frame.
const searchFrameSize = 20

// Search implements the streaming /search gRPC RPC. It mirrors the
// HTTP /api/search handler byte-for-byte on the wire-format side
// (same TraceQL → chplan → CH-row → toTraceSummaries pipeline) and
// only diverges on the response envelope: instead of one JSON body
// with all trace summaries, cerberus emits one or more
// tempopb.SearchResponse frames, each carrying up to searchFrameSize
// summaries. The final frame stamps SearchMetrics.InspectedTraces with
// the total trace count so Grafana's Tempo datasource has the same
// aggregate the HTTP path returns.
//
// This method shadows the embedded
// tempopb.UnimplementedStreamingQuerierServer.Search promoted via
// Service — Go's method-shadowing rules dispatch here while the other
// six RPCs continue to return codes.Unimplemented until PRs 3 + 4
// land. See .claude/plans/tempo-grpc-streaming-design.md §3 (Search row)
// + §4 (frame mapping) for the cross-RPC strategy.
//
// Error mapping (matches the design-doc table):
//   - TraceQL parser error / lowering error → codes.InvalidArgument
//     (errors.Is against tempo.ErrParseStage / tempo.ErrLowerStage).
//   - chclient circuit-open → codes.Unavailable.
//   - any other engine/CH error → codes.Internal.
//
// The admit limiter is enforced upstream by the stream interceptor
// wired in NewServer (see server.go); a saturated head short-circuits
// with codes.ResourceExhausted before Search is invoked, mirroring the
// HTTP 503 + Retry-After: 1 path.
func (s *Service) Search(req *tempopb.SearchRequest, stream tempopb.StreamingQuerier_SearchServer) error {
	if s.Handler == nil {
		return status.Errorf(codes.Internal, "tempo gRPC: handler not wired")
	}
	if req == nil {
		return status.Errorf(codes.InvalidArgument, "tempo gRPC: nil SearchRequest")
	}
	if req.Query == "" {
		// HTTP /api/search returns an empty result set on empty Query
		// (Grafana sometimes pings without a query as a health-check);
		// mirror that by emitting one empty frame with zero metrics
		// rather than InvalidArgument so the streaming health-check
		// stays equivalent to the HTTP one.
		return stream.Send(&tempopb.SearchResponse{
			Traces:  []*tempopb.TraceSearchMetadata{},
			Metrics: &tempopb.SearchMetrics{InspectedTraces: 0},
		})
	}

	ctx := stream.Context()

	// Thread the response trace limit into lowering so the nested-set
	// numbering walk bounds to the traces this stream will keep — same
	// memory bound the HTTP /api/search path applies (#103). req.Limit==0
	// falls back to the documented default so the bound matches the
	// TruncateSummaries default below.
	limit := int(req.Limit)
	if limit <= 0 {
		limit = tempo.DefaultSearchLimit
	}
	ctx = traceql.WithSearchTraceLimit(ctx, limit)

	// Thread the request time window so stampSearchTraceLimit folds it
	// into the bounded plain-search scan — the gRPC mirror of HTTP
	// /api/search's WithSearchWindow. SearchRequest.Start / End arrive as
	// uint32 Unix seconds (proto fields 5/6); a windowless request (both
	// zero — e.g. a hand-rolled `q={}`) is clamped to the same recent
	// lookback the HTTP handler applies, so the trace-limit pushdown's
	// inner GROUP BY TraceId scans a window instead of the whole table.
	// A one-sided window is a deliberate open-ended bound, left as-is.
	start, end := secondsToTime(req.Start), secondsToTime(req.End)
	if start.IsZero() && end.IsZero() {
		end = time.Now().UTC()
		start = end.Add(-tempo.DefaultSearchLookback)
	}
	ctx = traceql.WithSearchWindow(ctx, start, end)

	// Open the streaming cursor against the lowered TraceQL plan.
	// cursor.Close is deferred so a client cancellation (ctx.Done()
	// fires through QueryCursor's progress-context) propagates into
	// the CH driver — same pattern as Loki's /tail and Prom's
	// /query_range chunked path.
	res, err := s.Handler.Engine.QueryCursor(ctx, s.Handler.Lang(), req.Query)
	if err != nil {
		return mapEngineError(err)
	}
	defer func() { _ = res.Cursor.Close() }()

	// Drain the cursor into a samples slice. The toTraceSummaries
	// shaper requires the full result set to group spans by TraceID;
	// the gRPC frame batching kicks in on the post-shaped summary
	// list (so frames carry whole-trace summaries, not partial spans).
	// Cancellation through ctx surfaces as cursor.Next returning false
	// + cursor.Err returning the wrapped context error.
	samples := make([]chclient.Sample, 0, 64)
	for res.Cursor.Next() {
		samples = append(samples, res.Cursor.Sample())
		// Honour context cancellation between rows so a client
		// disconnect mid-drain doesn't block on CH back-pressure.
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
	}
	if err := res.Cursor.Err(); err != nil {
		return mapEngineError(err)
	}

	// Honour the request's SpansPerSpanSet + Limit the same way the
	// HTTP handler honours `spss` + `limit` (zero values fall back to
	// Tempo's documented defaults inside the shared helpers), so the
	// streaming path stays wire-equivalent with /api/search.
	summaries, missingRoots := tempo.ToTraceSummaries(samples, int(req.SpansPerSpanSet))
	summaries, missingRoots = tempo.TruncateSummaries(summaries, missingRoots, int(req.Limit))
	if len(missingRoots) > 0 {
		// Same best-effort policy the HTTP handler uses: log + ignore
		// a follow-up lookup failure (the earliest-span fallback in
		// summaries stays in place). Errors here are NOT surfaced to
		// the client — a downgraded summary is preferable to a hard
		// 5xx when the primary query already succeeded.
		if err := s.Handler.ResolveMissingRoots(ctx, summaries, missingRoots); err != nil {
			s.Logger.Warn("tempo gRPC root-span lookup failed",
				"err", err, "missing", len(missingRoots))
		}
	}

	// Frame-batch the summaries into searchFrameSize-sized chunks.
	// The tail frame carries SearchMetrics so Grafana sees the
	// aggregate exactly once at end-of-stream — mirroring how the
	// HTTP /api/search response embeds Metrics alongside the (single)
	// traces array.
	flusher := newSearchFlusher(searchFrameSize)
	for i := range summaries {
		shard, ready := flusher.Push(toTempopbTraceMetadata(summaries[i]))
		if !ready {
			continue
		}
		if err := stream.Send(&tempopb.SearchResponse{Traces: shard}); err != nil {
			return mapStreamError(err)
		}
	}
	tail := flusher.Tail()
	// Always emit a tail frame, even when summaries is empty or the
	// final shard would be empty — Grafana parses SearchMetrics from
	// the tail and an empty trailer keeps the wire envelope round-
	// tripable through the streaming/HTTP equivalence check.
	if err := stream.Send(&tempopb.SearchResponse{
		Traces: tail,
		Metrics: &tempopb.SearchMetrics{
			//nolint:gosec // search result count is bounded by CH row LIMIT; overflow would require > 4B summaries which the engine can't materialise.
			InspectedTraces: uint32(len(summaries)),
		},
	}); err != nil {
		return mapStreamError(err)
	}
	return nil
}

// mapEngineError converts an engine.Query / Engine.QueryCursor error
// into the gRPC status the Search RPC returns. Mirrors
// classifySearchErr's HTTP-status mapping in handler.go: parse + lower
// stages → codes.InvalidArgument (user-facing query errors);
// chclient circuit-open → codes.Unavailable; anything else → codes.Internal.
func mapEngineError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, chclient.ErrCircuitOpen):
		return status.Errorf(codes.Unavailable, "%v", err)
	case errors.Is(err, tempo.ErrParseStage), errors.Is(err, tempo.ErrLowerStage):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// mapStreamError converts a stream.Send error into the appropriate
// gRPC status. Send fails most often because the client cancelled
// (ctx.Err()) or the transport dropped; we keep the underlying status
// when it's already a gRPC one (the gRPC runtime usually returns
// status-wrapped errors here) and otherwise surface codes.Internal.
func mapStreamError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "stream send: %v", err)
}

// secondsToTime converts a SearchRequest Start / End bound into a
// time.Time. The streaming /search proto carries the window as uint32
// Unix seconds (tempopb.SearchRequest fields 5/6 are named start/end,
// distinct from MetricsQueryRange's nanosecond Start/End — hence a
// dedicated helper rather than reusing nanosToTime). A zero value means
// "bound omitted" and round-trips to the zero time.Time, which
// WithSearchWindow treats as no predicate.
func secondsToTime(sec uint32) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0).UTC()
}

// toTempopbTraceMetadata pivots cerberus's local tempo.TraceSummary
// into the wire-format tempopb.TraceSearchMetadata Grafana's Tempo
// datasource consumes over gRPC. Field-for-field equivalence with the
// HTTP /api/search JSON envelope (see tempo.TraceSummary) so a search
// query through the streaming path produces the same trace identity +
// timestamps + duration the HTTP path returns. The startTimeUnixNano
// field switches from JSON-string (HTTP) to uint64 (proto); we parse
// the string back into the integer the proto field expects, falling
// back to 0 when the conversion fails (defensive — TraceSummary
// populates the field via strconv.FormatInt of a UnixNano result that
// always round-trips).
func toTempopbTraceMetadata(t tempo.TraceSummary) *tempopb.TraceSearchMetadata {
	var startNs uint64
	if v, err := strconv.ParseUint(t.StartTimeUnixNano, 10, 64); err == nil {
		startNs = v
	}
	out := &tempopb.TraceSearchMetadata{
		TraceID:           t.TraceID,
		RootServiceName:   t.RootServiceName,
		RootTraceName:     t.RootTraceName,
		StartTimeUnixNano: startNs,
		//nolint:gosec // DurationMs is sourced from a TraceSummary populated by dateDiff(ms, …) over CH spans; field tops out at int32 in practice — uint32 cannot overflow.
		DurationMs: uint32(t.DurationMs),
	}
	// Mirror the HTTP envelope's spanSets + legacy spanSet pair onto
	// the proto fields so Grafana's streaming search consumers see the
	// same matched-span lists the JSON path carries.
	for i := range t.SpanSets {
		out.SpanSets = append(out.SpanSets, toTempopbSpanSet(t.SpanSets[i]))
	}
	if t.SpanSet != nil {
		out.SpanSet = toTempopbSpanSet(*t.SpanSet)
	}
	return out
}

// toTempopbSpanSet pivots the HTTP-envelope SpanSet onto tempopb's
// proto shape. StartTimeUnixNano / DurationNanos arrive as the proto3
// JSON decimal-string encoding of the uint64 fields; parse failures
// land as 0 (defensive — TraceSummary populates them via FormatInt of
// values that always round-trip).
func toTempopbSpanSet(s tempo.SpanSet) *tempopb.SpanSet {
	out := &tempopb.SpanSet{
		//nolint:gosec // Matched counts CH result rows per trace; bounded far below uint32.
		Matched: uint32(s.Matched),
	}
	for _, sp := range s.Spans {
		var start, dur uint64
		if v, err := strconv.ParseUint(sp.StartTimeUnixNano, 10, 64); err == nil {
			start = v
		}
		if v, err := strconv.ParseUint(sp.DurationNanos, 10, 64); err == nil {
			dur = v
		}
		out.Spans = append(out.Spans, &tempopb.Span{
			SpanID:            sp.SpanID,
			Name:              sp.Name,
			StartTimeUnixNano: start,
			DurationNanos:     dur,
		})
	}
	return out
}

// searchFlusher batches TraceSearchMetadata entries into shards of a
// fixed size + tracks the running total so the tail-frame Metrics
// block can stamp the inspected-trace aggregate exactly once.
//
// Lifecycle:
//   - Push(m) appends m to the current shard. When the shard reaches
//     `size`, Push returns (shard, true) and resets the internal
//     buffer; the caller is expected to flush the returned shard
//     immediately via stream.Send.
//   - Tail() returns whatever rows accumulated after the last full
//     shard (empty when the row count is a clean multiple of size).
//     Callers always emit the tail with SearchMetrics attached even
//     when len(Tail())==0, so Grafana parses the aggregate from a
//     well-defined "last frame" position.
//
// The struct is intentionally unexported — only Search.search-shaped
// code paths in this package consume it. PR 4's MetricsQueryRange
// sibling has its own series-batching helper with different chunking
// semantics (per-series chunks, not per-summary).
type searchFlusher struct {
	size  int
	cur   []*tempopb.TraceSearchMetadata
	total int
}

// newSearchFlusher returns a flusher that emits a shard every `size`
// pushes. size MUST be > 0; size <= 0 panics — there's no sensible
// fallback (a non-batching flusher would defeat the purpose).
func newSearchFlusher(size int) *searchFlusher {
	if size <= 0 {
		panic("tempo/grpc: newSearchFlusher requires size > 0")
	}
	return &searchFlusher{
		size: size,
		cur:  make([]*tempopb.TraceSearchMetadata, 0, size),
	}
}

// Push appends m to the current shard. When the shard fills, returns
// the now-flushable batch along with `ready=true` and resets the
// internal buffer for the next shard. When the shard is not yet full,
// returns (nil, false) — the caller continues feeding.
func (f *searchFlusher) Push(m *tempopb.TraceSearchMetadata) ([]*tempopb.TraceSearchMetadata, bool) {
	f.cur = append(f.cur, m)
	f.total++
	if len(f.cur) < f.size {
		return nil, false
	}
	shard := f.cur
	f.cur = make([]*tempopb.TraceSearchMetadata, 0, f.size)
	return shard, true
}

// Tail returns whatever rows accumulated after the last full shard.
// Empty (nil-or-zero-length) when the total push count is a clean
// multiple of size. Callers stamp SearchMetrics on the tail frame even
// when Tail returns an empty slice so Grafana receives the aggregate
// at a well-defined wire position.
func (f *searchFlusher) Tail() []*tempopb.TraceSearchMetadata {
	if len(f.cur) == 0 {
		return nil
	}
	out := f.cur
	f.cur = nil
	return out
}
