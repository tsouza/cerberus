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
