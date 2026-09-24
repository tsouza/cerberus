package engine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/chclient"
)

// fakeQueryLog models a server's query log as the reader sees it: rows in
// the reader's total order, served strictly after the request's cursor and
// capped at its limit — the contract chclient's record-selection query
// implements on a real server (test/querylog pins that half).
type fakeQueryLog struct {
	mu       sync.Mutex
	local    []chclient.QueryLogActualRow
	union    []chclient.QueryLogActualRow
	err      error
	unionErr error
	reqs     []chclient.QueryLogActualsRequest
}

func (f *fakeQueryLog) QueryLogActuals(_ context.Context, req chclient.QueryLogActualsRequest) ([]chclient.QueryLogActualRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	rows := f.local
	if req.Union {
		if f.unionErr != nil {
			return nil, f.unionErr
		}
		rows = f.union
	}
	var out []chclient.QueryLogActualRow
	for _, r := range rows {
		if compareCursor(cursorOf(r), req.After) > 0 {
			out = append(out, r)
			if len(out) == req.Limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeQueryLog) requests() []chclient.QueryLogActualsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func cursorOf(r chclient.QueryLogActualRow) chclient.QueryLogCursor {
	return chclient.QueryLogCursor{EventTime: r.EventTime, Hostname: r.Hostname, QueryID: r.QueryID}
}

func compareCursor(a, b chclient.QueryLogCursor) int {
	if c := a.EventTime.Compare(b.EventTime); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Hostname, b.Hostname); c != 0 {
		return c
	}
	return cmp.Compare(a.QueryID, b.QueryID)
}

// sortedLog returns rows in the reader's total order, as the server serves them.
func sortedLog(rows ...chclient.QueryLogActualRow) []chclient.QueryLogActualRow {
	out := slices.Clone(rows)
	slices.SortFunc(out, func(a, b chclient.QueryLogActualRow) int { return compareCursor(cursorOf(a), cursorOf(b)) })
	return out
}

func testActualsConfig() actuals.Config {
	cfg := actuals.DefaultConfig()
	cfg.Enabled = true
	return cfg
}

// testNow is the reconciler clock every test pins; testRowTime is inside its
// lookback window.
var (
	testNow     = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	testRowTime = testNow.Add(-time.Minute)
)

func newTestReconciler(q QueryLogQuerier, tracker *actuals.Tracker, union func() bool, logger *slog.Logger) *QueryLogActualsReconciler {
	r := NewQueryLogActualsReconciler(q, tracker, testActualsConfig(), union, logger)
	r.now = func() time.Time { return testNow }
	return r
}

func TestQueryLogActualsReconciler_PollFeedsTrackerAndAdvancesCursor(t *testing.T) {
	t2 := testRowTime.Add(time.Second)
	fake := &fakeQueryLog{local: sortedLog(
		chclient.QueryLogActualRow{Hostname: "ch-0", LogComment: "cerb:agg;rw", QueryID: "q1", ReadRows: 1000, ReadBytes: 8000, MemoryUsage: 500, EventTime: testRowTime},
		chclient.QueryLogActualRow{Hostname: "ch-0", LogComment: "cerb:agg;rw;rbf", QueryID: "q2", ReadRows: 2000, ReadBytes: 16000, MemoryUsage: 900, EventTime: t2},
	)}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, nil, nil)

	r.poll(context.Background())

	cfg := testActualsConfig()
	req := fake.requests()[0]
	want := chclient.QueryLogActualsRequest{
		After:         chclient.QueryLogCursor{EventTime: testNow.Add(-cfg.QueryLogLookback)},
		SettleDelay:   cfg.QueryLogSettleDelay,
		Window:        cfg.QueryLogLookback,
		ShapeIDPrefix: shapeIDPrefix,
		Limit:         queryLogActualsBatchLimit,
	}
	if req != want {
		t.Fatalf("first read = %+v, want %+v", req, want)
	}
	if got, want := r.cursor, (chclient.QueryLogCursor{EventTime: t2, Hostname: "ch-0", QueryID: "q2"}); got != want {
		t.Fatalf("cursor = %+v, want the last row read %+v", got, want)
	}
	report, ok := tracker.Snapshot("cerb:agg;rw")
	if !ok || report.ActualEMARows != 1000 || report.LastSource != actuals.SourceQueryLog {
		t.Fatalf("expected the first row recorded as SourceQueryLog, got %+v (ok=%v)", report, ok)
	}
	if report, ok = tracker.Snapshot("cerb:agg;rw;rbf"); !ok || report.ActualEMARows != 2000 {
		t.Fatalf("expected the second row recorded, got %+v (ok=%v)", report, ok)
	}
}

// TestQueryLogActualsReconciler_EqualTimestampsPageWithoutLossOrRepeat pins
// the pagination the old second-granularity watermark got wrong: more rows
// than one page shares ONE timestamp, and every one of them is recorded
// exactly once, across pages and across polls.
func TestQueryLogActualsReconciler_EqualTimestampsPageWithoutLossOrRepeat(t *testing.T) {
	const shape = "cerb:agg;rw;burst"
	n := queryLogActualsBatchLimit*2 + queryLogActualsBatchLimit/2
	rows := make([]chclient.QueryLogActualRow, 0, n)
	for i := range n {
		rows = append(rows, chclient.QueryLogActualRow{
			Hostname: fmt.Sprintf("ch-%d", i%2), LogComment: shape, QueryID: fmt.Sprintf("q-%05d", i),
			ReadRows: 1, EventTime: testRowTime,
		})
	}
	fake := &fakeQueryLog{local: sortedLog(rows...)}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, nil, nil)

	r.poll(context.Background())
	r.poll(context.Background())

	report, ok := tracker.Snapshot(shape)
	if !ok || report.Observations != n {
		t.Fatalf("observations = %d (ok=%v), want %d — every row sharing the timestamp, once each", report.Observations, ok, n)
	}
}

// TestQueryLogActualsReconciler_PagesPerPollAreBounded pins the per-poll cost
// bound: a backlog larger than queryLogActualsMaxPagesPerPoll pages costs
// exactly that many reads, and the cursor carries the rest to the next poll.
func TestQueryLogActualsReconciler_PagesPerPollAreBounded(t *testing.T) {
	const shape = "cerb:agg;rw;backlog"
	extra := queryLogActualsBatchLimit / 4
	n := queryLogActualsBatchLimit*queryLogActualsMaxPagesPerPoll + extra
	rows := make([]chclient.QueryLogActualRow, 0, n)
	for i := range n {
		rows = append(rows, chclient.QueryLogActualRow{
			Hostname: "ch-0", LogComment: shape, QueryID: fmt.Sprintf("q-%06d", i),
			ReadRows: 1, EventTime: testRowTime.Add(time.Duration(i) * time.Microsecond),
		})
	}
	fake := &fakeQueryLog{local: sortedLog(rows...)}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, nil, nil)

	r.poll(context.Background())
	if got := len(fake.requests()); got != queryLogActualsMaxPagesPerPoll {
		t.Fatalf("one poll issued %d reads, want the bound %d", got, queryLogActualsMaxPagesPerPoll)
	}
	if report, _ := tracker.Snapshot(shape); report.Observations != n-extra {
		t.Fatalf("after the bounded poll: %d observations, want %d", report.Observations, n-extra)
	}
	r.poll(context.Background())
	if report, _ := tracker.Snapshot(shape); report.Observations != n {
		t.Fatalf("after the next poll: %d observations, want all %d", report.Observations, n)
	}
}

func TestQueryLogActualsReconciler_PollFailureKeepsCursor(t *testing.T) {
	fake := &fakeQueryLog{err: errors.New("query_log disabled")}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, nil, nil)
	start := chclient.QueryLogCursor{EventTime: testRowTime, Hostname: "ch-0", QueryID: "q-last"}
	r.cursor = start

	r.poll(context.Background())
	if r.cursor != start {
		t.Fatalf("cursor = %+v after a failed read, want it unchanged at %+v", r.cursor, start)
	}
	fake.err = nil
	r.poll(context.Background())
	reqs := fake.requests()
	if reqs[len(reqs)-1].After != start {
		t.Fatalf("retry read after %+v, want the same cursor %+v", reqs[len(reqs)-1].After, start)
	}
}

// TestQueryLogActualsReconciler_CursorClampedToLookback pins the read window:
// however far behind the cursor is, a poll never reads before the lookback,
// the interval the packet path's query-id marks are kept for.
func TestQueryLogActualsReconciler_CursorClampedToLookback(t *testing.T) {
	fake := &fakeQueryLog{}
	r := newTestReconciler(fake, actuals.NewTracker(testActualsConfig()), nil, nil)
	r.cursor = chclient.QueryLogCursor{EventTime: testNow.Add(-24 * time.Hour), Hostname: "ch-0", QueryID: "stale"}

	r.poll(context.Background())
	floor := chclient.QueryLogCursor{EventTime: testNow.Add(-testActualsConfig().QueryLogLookback)}
	if got := fake.requests()[0].After; got != floor {
		t.Fatalf("read after %+v, want the lookback floor %+v", got, floor)
	}
}

func TestQueryLogActualsReconciler_SkipsRowsWithNoLogComment(t *testing.T) {
	fake := &fakeQueryLog{local: sortedLog(chclient.QueryLogActualRow{Hostname: "ch-0", QueryID: "q", ReadRows: 999, EventTime: testRowTime})}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, nil, nil)

	r.poll(context.Background())
	if stats := tracker.Stats(); stats.Entries != 0 {
		t.Fatalf("expected an empty log_comment row to be skipped, got %+v", stats)
	}
}

// TestQueryLogActualsReconciler_ReadsUnionWhenInForce pins the source switch:
// with query_log_union in force the reconciler reads system.all_query_log and
// records a row only a replica or a rotated table holds.
func TestQueryLogActualsReconciler_ReadsUnionWhenInForce(t *testing.T) {
	const shape = "cerb:agg;rw;replica"
	fake := &fakeQueryLog{union: sortedLog(chclient.QueryLogActualRow{
		Hostname: "ch-1", LogComment: shape, QueryID: "on-the-other-replica", ReadRows: 77, EventTime: testRowTime,
	})}
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, func() bool { return true }, nil)

	r.poll(context.Background())
	if !fake.requests()[0].Union {
		t.Fatal("query_log_union in force but the read went to the local log")
	}
	if report, ok := tracker.Snapshot(shape); !ok || report.ActualEMARows != 77 {
		t.Fatalf("the replica's row was not recorded: %+v (ok=%v)", report, ok)
	}
}

// TestQueryLogActualsReconciler_UnionRefusalFallsBackToLocal pins the
// runtime fallback: a union read the server refuses between two capability
// probes is retried on the local log in the same poll, logged once per
// transition, and the union is read again once it answers.
func TestQueryLogActualsReconciler_UnionRefusalFallsBackToLocal(t *testing.T) {
	const shape = "cerb:agg;rw;local"
	fake := &fakeQueryLog{
		local:    sortedLog(chclient.QueryLogActualRow{Hostname: "ch-0", LogComment: shape, QueryID: "q-local", ReadRows: 5, EventTime: testRowTime}),
		unionErr: fmt.Errorf("%w: code 60", chclient.ErrQueryLogUnionRefused),
	}
	var logs bytes.Buffer
	tracker := actuals.NewTracker(testActualsConfig())
	r := newTestReconciler(fake, tracker, func() bool { return true }, slog.New(slog.NewTextHandler(&logs, nil)))

	r.poll(context.Background())
	r.poll(context.Background())

	if report, ok := tracker.Snapshot(shape); !ok || report.Observations != 1 {
		t.Fatalf("the local row after a refused union read: %+v (ok=%v), want one observation", report, ok)
	}
	reqs := fake.requests()
	if len(reqs) != 4 || !reqs[0].Union || reqs[1].Union || !reqs[2].Union || reqs[3].Union {
		t.Fatalf("reads = %+v, want union-then-local on each of the two polls", reqs)
	}
	if got := strings.Count(logs.String(), "reading the local system.query_log"); got != 1 {
		t.Fatalf("fallback logged %d times over two refused polls, want once:\n%s", got, logs.String())
	}

	fake.unionErr = nil
	r.poll(context.Background())
	if !strings.Contains(logs.String(), "system.all_query_log readable again") {
		t.Fatalf("the recovery was not logged:\n%s", logs.String())
	}
}

// TestQueryLogActualsReconciler_TransportFailureIsNotAFallback pins that only
// a server's refusal of the union table falls back: a transport failure is
// retried from the same cursor next poll, never read from a narrower source
// that would move the cursor past rows only the union holds.
func TestQueryLogActualsReconciler_TransportFailureIsNotAFallback(t *testing.T) {
	fake := &fakeQueryLog{unionErr: errors.New("dial tcp: connection refused")}
	r := newTestReconciler(fake, actuals.NewTracker(testActualsConfig()), func() bool { return true }, nil)

	r.poll(context.Background())
	if reqs := fake.requests(); len(reqs) != 1 || !reqs[0].Union {
		t.Fatalf("reads = %+v, want the one failed union read and no local fallback", reqs)
	}
}

func TestQueryLogActualsReconciler_RunStopsOnContextCancel(t *testing.T) {
	fake := &fakeQueryLog{}
	cfg := testActualsConfig()
	cfg.QueryLogPollInterval = time.Millisecond
	r := NewQueryLogActualsReconciler(fake, actuals.NewTracker(cfg), cfg, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Let it poll at least once, then stop it.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop within 1s of ctx cancellation")
	}
	if len(fake.requests()) == 0 {
		t.Fatal("expected at least one poll before cancellation")
	}
}

func TestQueryLogActualsReconciler_RunNoOpWithoutClientOrTracker(t *testing.T) {
	r := NewQueryLogActualsReconciler(nil, nil, testActualsConfig(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.Run(ctx) // must return once ctx is done, not hang or panic
}

// TestQueryLogActualsReconciler_SkipsRowsThePacketPathAlreadyRecorded pins the
// query-id set difference (cerberus issue #3184).
//
// The progress-packet path and this poller feed the SAME Tracker for the SAME
// physical query, and the poller had no set difference against the packet
// path, so every completed query was recorded twice. That is not merely a
// doubled counter: MinObservations is 2, so a single real query satisfied a
// corroboration floor whose whole stated purpose is that one observation is
// never enough evidence — and the Tracker feeds the K clamp and per-rung
// admission, making it a wrong routing input.
func TestQueryLogActualsReconciler_SkipsRowsThePacketPathAlreadyRecorded(t *testing.T) {
	const shape = "cerb:agg;rw"
	tracker := actuals.NewTracker(testActualsConfig())

	// The packet path recorded this dispatch: one observation, whole-query
	// row count, and it owns the id.
	tracker.MarkPacketObserved("trace-span-1")
	if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 1000}, actuals.SourcePacket); !ok {
		t.Fatal("fixture: the packet observation was not recorded")
	}

	row := chclient.QueryLogActualRow{Hostname: "ch-0", LogComment: shape, QueryID: "trace-span-1", ReadRows: 1000, EventTime: testRowTime}
	fake := &fakeQueryLog{local: sortedLog(row)}
	r := newTestReconciler(fake, tracker, nil, nil)

	r.poll(context.Background())
	r.poll(context.Background())

	report, ok := tracker.Snapshot(shape)
	if !ok {
		t.Fatal("shape vanished from the tracker")
	}
	if report.Observations != 1 {
		t.Errorf("observations = %d, want 1 — one physical query must yield ONE observation, "+
			"or a single query on its own satisfies the MinObservations=2 corroboration floor",
			report.Observations)
	}
	if report.LastSource != actuals.SourcePacket {
		t.Errorf("last source = %v, want %v — the query_log row must not overwrite the packet observation",
			report.LastSource, actuals.SourcePacket)
	}
	// The cursor still advances past a refused row, or the poller re-reads it
	// forever and never makes progress.
	if r.cursor != cursorOf(row) {
		t.Errorf("cursor = %+v, want %+v — a refused row was still READ and must not stall the cursor", r.cursor, cursorOf(row))
	}
}

// TestQueryLogActualsReconciler_RecordsRowsThePacketPathNeverSaw is the
// non-vacuity guard for the test above: the poller keeps its genuine residual
// coverage. A dispatch whose log_comment was stamped without actuals capture
// being armed has no packet observation and no marked id, so the poller is the
// only source for it and must still record it.
//
// Without this, "skip everything" would pass the sibling test.
func TestQueryLogActualsReconciler_RecordsRowsThePacketPathNeverSaw(t *testing.T) {
	const shape = "cerb:agg;rw;unseen"
	tracker := actuals.NewTracker(testActualsConfig())

	fake := &fakeQueryLog{local: sortedLog(chclient.QueryLogActualRow{
		Hostname: "ch-0", LogComment: shape, QueryID: "trace-span-unmarked", ReadRows: 4242, EventTime: testRowTime,
	})}
	r := newTestReconciler(fake, tracker, nil, nil)
	r.poll(context.Background())

	report, ok := tracker.Snapshot(shape)
	if !ok {
		t.Fatal("an unmarked query_log row was dropped; the poller's residual coverage is gone")
	}
	if report.ActualEMARows != 4242 || report.LastSource != actuals.SourceQueryLog {
		t.Errorf("report = %+v, want the query_log row recorded verbatim", report)
	}
}

// TestQueryLogActualsReconciler_RouteBShardRowsDoNotDragTheEMA pins the
// route-B half of cerberus issue #3184, which re-opened issue #3033 through
// the other source.
//
// A routed request dispatches K shard statements. Each gets its own query_id
// but they all share ONE log_comment, so the poller saw K rows for one logical
// request — each carrying a shard's FRACTIONAL read_rows — while the packet
// path's ShardActualsFold correctly contributed a single SUMMED observation.
// The K fractional samples then dragged the EMA toward total/K.
func TestQueryLogActualsReconciler_RouteBShardRowsDoNotDragTheEMA(t *testing.T) {
	const (
		shape      = "cerb:agg;rw;routed"
		k          = 4
		shardRows  = 250
		wholeQuery = k * shardRows
	)
	tracker := actuals.NewTracker(testActualsConfig())

	// The fan-out: K shard dispatches, then the fold's single summed record.
	rows := make([]chclient.QueryLogActualRow, 0, k)
	for i := range k {
		id := "trace-span-shard-" + string(rune('a'+i))
		tracker.MarkPacketObserved(id)
		rows = append(rows, chclient.QueryLogActualRow{
			Hostname: "ch-0", LogComment: shape, QueryID: id, ReadRows: shardRows,
			EventTime: testRowTime.Add(time.Duration(i) * time.Second),
		})
	}
	if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: wholeQuery}, actuals.SourcePacket); !ok {
		t.Fatal("fixture: the fold's summed observation was not recorded")
	}

	r := newTestReconciler(&fakeQueryLog{local: sortedLog(rows...)}, tracker, nil, nil)
	r.poll(context.Background())

	report, ok := tracker.Snapshot(shape)
	if !ok {
		t.Fatal("shape vanished from the tracker")
	}
	if report.Observations != 1 {
		t.Errorf("observations = %d, want 1 — one routed request is ONE observation, not %d shard fragments",
			report.Observations, k)
	}
	if report.ActualEMARows != wholeQuery {
		t.Errorf("EMA rows = %v, want %d — %d per-shard samples dragged the whole-query row count toward total/K, "+
			"which is the input the K clamp and per-rung admission read",
			report.ActualEMARows, wholeQuery, k)
	}
}
