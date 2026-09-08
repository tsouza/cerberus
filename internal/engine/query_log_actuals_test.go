package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/actuals"
)

type fakeQueryLogQuerier struct {
	rows []QueryLogActualRow
	err  error

	calls      int
	lastSince  time.Time
	lastPrefix string
	lastLimit  int
}

func (f *fakeQueryLogQuerier) QueryLogActuals(_ context.Context, since time.Time, shapeIDPrefix string, limit int) ([]QueryLogActualRow, error) {
	f.calls++
	f.lastSince = since
	f.lastPrefix = shapeIDPrefix
	f.lastLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func testActualsConfig() actuals.Config {
	cfg := actuals.DefaultConfig()
	cfg.Enabled = true
	return cfg
}

func TestQueryLogActualsReconciler_PollFeedsTrackerAndAdvancesWatermark(t *testing.T) {
	t1 := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	fake := &fakeQueryLogQuerier{rows: []QueryLogActualRow{
		{LogComment: "cerb:agg;rw", ReadRows: 1000, ReadBytes: 8000, MemoryUsage: 500, EventTime: t1},
		{LogComment: "cerb:agg;rw;rbf", ReadRows: 2000, ReadBytes: 16000, MemoryUsage: 900, EventTime: t2},
	}}
	tracker := actuals.NewTracker(testActualsConfig())
	r := NewQueryLogActualsReconciler(fake, tracker, testActualsConfig(), nil)

	since := time.Time{}
	next := r.poll(context.Background(), since)

	if next != t2 {
		t.Fatalf("expected the watermark to advance to the latest EventTime %v, got %v", t2, next)
	}
	if fake.lastPrefix != shapeIDPrefix {
		t.Fatalf("expected the shape id prefix %q, got %q", shapeIDPrefix, fake.lastPrefix)
	}
	if fake.lastLimit != queryLogActualsBatchLimit {
		t.Fatalf("expected the batch limit %d, got %d", queryLogActualsBatchLimit, fake.lastLimit)
	}

	report, ok := tracker.Snapshot("cerb:agg;rw")
	if !ok || report.ActualEMARows != 1000 || report.LastSource != actuals.SourceQueryLog {
		t.Fatalf("expected the first row recorded as SourceQueryLog, got %+v (ok=%v)", report, ok)
	}
	report, ok = tracker.Snapshot("cerb:agg;rw;rbf")
	if !ok || report.ActualEMARows != 2000 {
		t.Fatalf("expected the second row recorded, got %+v (ok=%v)", report, ok)
	}
}

func TestQueryLogActualsReconciler_PollFailureKeepsWatermark(t *testing.T) {
	fake := &fakeQueryLogQuerier{err: errors.New("query_log disabled")}
	tracker := actuals.NewTracker(testActualsConfig())
	r := NewQueryLogActualsReconciler(fake, tracker, testActualsConfig(), nil)

	since := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	next := r.poll(context.Background(), since)
	if !next.Equal(since) {
		t.Fatalf("expected the watermark to stay unchanged on a query failure, got %v want %v", next, since)
	}
}

func TestQueryLogActualsReconciler_SkipsRowsWithNoLogComment(t *testing.T) {
	fake := &fakeQueryLogQuerier{rows: []QueryLogActualRow{
		{LogComment: "", ReadRows: 999, EventTime: time.Now()},
	}}
	tracker := actuals.NewTracker(testActualsConfig())
	r := NewQueryLogActualsReconciler(fake, tracker, testActualsConfig(), nil)

	r.poll(context.Background(), time.Time{})
	if stats := tracker.Stats(); stats.Entries != 0 {
		t.Fatalf("expected an empty log_comment row to be skipped, got %+v", stats)
	}
}

func TestQueryLogActualsReconciler_RunStopsOnContextCancel(t *testing.T) {
	fake := &fakeQueryLogQuerier{}
	tracker := actuals.NewTracker(testActualsConfig())
	cfg := testActualsConfig()
	cfg.QueryLogPollInterval = time.Millisecond
	r := NewQueryLogActualsReconciler(fake, tracker, cfg, nil)

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
	if fake.calls == 0 {
		t.Fatal("expected at least one poll before cancellation")
	}
}

func TestQueryLogActualsReconciler_RunNoOpWithoutClientOrTracker(t *testing.T) {
	r := NewQueryLogActualsReconciler(nil, nil, testActualsConfig(), nil)
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
	t1 := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tracker := actuals.NewTracker(testActualsConfig())

	// The packet path recorded this dispatch: one observation, whole-query
	// row count, and it owns the id.
	tracker.MarkPacketObserved("trace-span-1")
	if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 1000}, actuals.SourcePacket); !ok {
		t.Fatal("fixture: the packet observation was not recorded")
	}

	fake := &fakeQueryLogQuerier{rows: []QueryLogActualRow{
		{LogComment: shape, QueryID: "trace-span-1", ReadRows: 1000, EventTime: t1},
	}}
	r := NewQueryLogActualsReconciler(fake, tracker, testActualsConfig(), nil)

	next := r.poll(context.Background(), time.Time{})

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
	// The watermark still advances past a skipped row, or the poller re-reads
	// it forever and never makes progress.
	if next != t1 {
		t.Errorf("watermark = %v, want %v — a skipped row was still READ and must not stall the watermark", next, t1)
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
	t1 := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tracker := actuals.NewTracker(testActualsConfig())

	fake := &fakeQueryLogQuerier{rows: []QueryLogActualRow{
		{LogComment: shape, QueryID: "trace-span-unmarked", ReadRows: 4242, EventTime: t1},
	}}
	r := NewQueryLogActualsReconciler(fake, tracker, testActualsConfig(), nil)
	r.poll(context.Background(), time.Time{})

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
	t1 := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	tracker := actuals.NewTracker(testActualsConfig())

	// The fan-out: K shard dispatches, then the fold's single summed record.
	rows := make([]QueryLogActualRow, 0, k)
	for i := range k {
		id := "trace-span-shard-" + string(rune('a'+i))
		tracker.MarkPacketObserved(id)
		rows = append(rows, QueryLogActualRow{
			LogComment: shape, QueryID: id, ReadRows: shardRows,
			EventTime: t1.Add(time.Duration(i) * time.Second),
		})
	}
	if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: wholeQuery}, actuals.SourcePacket); !ok {
		t.Fatal("fixture: the fold's summed observation was not recorded")
	}

	r := NewQueryLogActualsReconciler(&fakeQueryLogQuerier{rows: rows}, tracker, testActualsConfig(), nil)
	r.poll(context.Background(), time.Time{})

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
