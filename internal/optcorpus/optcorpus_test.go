package optcorpus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeSource is an in-memory QueryLogSource: it returns a SourceRow for each
// requested id that has a seeded row, recording every batch of ids it was
// asked for.
type fakeSource struct {
	mu      sync.Mutex
	byID    map[string]SourceRow
	batches [][]string
	err     error
}

func newFakeSource() *fakeSource {
	return &fakeSource{byID: map[string]SourceRow{}}
}

// observeNoRoute calls ObserveQuery with route features absent (the
// Solver-off / unclassified-head shape), keeping the pre-route-feature call
// sites readable. Routing-feature behavior is covered by its own tests.
func observeNoRoute(r *Reconciler, queryID, shapeID string, opts []string, language string) {
	r.ObserveQuery(queryID, shapeID, opts, language, false, "", 0, 0, 0, 0, 0, 0, "")
}

func (f *fakeSource) seed(row SourceRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[row.QueryID] = row
}

func (f *fakeSource) FinishedByQueryID(_ context.Context, ids []string) ([]SourceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(ids))
	copy(cp, ids)
	f.batches = append(f.batches, cp)
	if f.err != nil {
		return nil, f.err
	}
	var out []SourceRow
	for _, id := range ids {
		if r, ok := f.byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// memSink captures written rows in memory.
type memSink struct {
	mu     sync.Mutex
	rows   []Row
	closed bool
	err    error
}

func (m *memSink) Write(rows []Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.rows = append(m.rows, rows...)
	return nil
}

func (m *memSink) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memSink) snapshot() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Row, len(m.rows))
	copy(out, m.rows)
	return out
}

func TestObserve_RingBounded_DropsOldest(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 3})
	for i := 0; i < 5; i++ {
		r.Observe(Record{QueryID: "q" + strconv.Itoa(i), ShapeID: "cerb:scan"})
	}
	ids := r.snapshotIDs()
	if len(ids) != 3 {
		t.Fatalf("ring size = %d; want 3 (bounded)", len(ids))
	}
	// Oldest two (q0, q1) evicted; q2,q3,q4 retained.
	want := map[string]bool{"q2": true, "q3": true, "q4": true}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected retained id %q; want one of q2/q3/q4", id)
		}
	}
}

func TestObserve_ReplaceInPlace_NoGrowth(t *testing.T) {
	// A retried dispatch reuses the trace id; re-Observing the same query_id
	// must replace in place, not consume a new slot.
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 3})
	r.Observe(Record{QueryID: "q0", ShapeID: "cerb:a"})
	r.Observe(Record{QueryID: "q0", ShapeID: "cerb:b"})
	ids := r.snapshotIDs()
	if len(ids) != 1 {
		t.Fatalf("ring size = %d; want 1 (replace in place)", len(ids))
	}
	rec, ok := r.recordFor("q0")
	if !ok || rec.ShapeID != "cerb:b" {
		t.Errorf("replace-in-place lost the latest record: %+v ok=%v", rec, ok)
	}
}

func TestObserveQuery_NonBlocking_DropsWhenBufferFull(t *testing.T) {
	// The data-plane seam must NEVER block: with a tiny ingest buffer and no
	// drain running, overflowing ObserveQuery calls are dropped, not blocked.
	// (If they blocked, this test would hang.)
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, ObserveBuffer: 2})
	for i := 0; i < 1000; i++ {
		observeNoRoute(r, "q"+strconv.Itoa(i), "cerb:scan", nil, "promql")
	}
	// Nothing reached the ring yet (no drain ran); the seam only buffers.
	if n := len(r.snapshotIDs()); n != 0 {
		t.Errorf("ring populated without a drain; got %d", n)
	}
	if r.dropped.Load() == 0 {
		t.Error("expected dropped count > 0 when ingest buffer overflowed")
	}
}

func TestDrainIngest_MovesSeamRecordsIntoRing(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, ObserveBuffer: 16})
	observeNoRoute(r, "qa", "cerb:a", []string{"condition_cache"}, "logql")
	observeNoRoute(r, "qb", "cerb:b", nil, "promql")
	r.drainIngest()
	ids := r.snapshotIDs()
	if len(ids) != 2 {
		t.Fatalf("after drain ring size = %d; want 2", len(ids))
	}
	rec, ok := r.recordFor("qa")
	if !ok || rec.ShapeID != "cerb:a" || rec.Language != "logql" {
		t.Errorf("drain dropped seam metadata: %+v ok=%v", rec, ok)
	}
}

func TestRun_DrainsSeamThenReconciles(t *testing.T) {
	// End-to-end through the non-blocking seam: ObserveQuery -> Run drains ->
	// reconcile joins the seeded source row and writes it to the sink.
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, ObserveBuffer: 16, Interval: time.Millisecond})
	src.seed(SourceRow{QueryID: "qz", NormalizedQueryHash: 7, ReadRows: 10})
	observeNoRoute(r, "qz", "cerb:scan", nil, "traceql")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.After(time.Second)
	for {
		if rows := sink.snapshot(); len(rows) == 1 {
			if rows[0].ShapeID != "cerb:scan" || rows[0].ReadRows != 10 {
				t.Errorf("seam->reconcile join wrong: %+v", rows[0])
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("seam record never reconciled to sink")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestEvictExpired_DropsNeverFinishedIDsOlderThanTTL(t *testing.T) {
	// A query observed but never joined to a query_log row must be dropped once
	// it is older than the TTL (the lookback window), so it stops riding every
	// per-interval IN(...).
	src := newFakeSource()
	r := New(src, &memSink{}, Options{RingCapacity: 8, TTL: time.Hour})

	clock := time.Unix(1_000_000, 0).UTC()
	r.now = func() time.Time { return clock }

	r.Observe(Record{QueryID: "stale", ShapeID: "cerb:scan"})
	clock = clock.Add(30 * time.Minute)
	r.Observe(Record{QueryID: "fresh", ShapeID: "cerb:scan"})

	// Advance past the TTL relative to "stale" (observed at t0) but not past it
	// for "fresh" (observed at t0+30m): cutoff = now-1h.
	clock = clock.Add(40 * time.Minute) // now = t0 + 70m

	if n := r.evictExpired(); n != 1 {
		t.Fatalf("evictExpired = %d; want 1 (only the stale id)", n)
	}
	ids := r.snapshotIDs()
	if len(ids) != 1 || ids[0] != "fresh" {
		t.Errorf("after eviction ids = %v; want [fresh]", ids)
	}
}

func TestEvictExpired_RefreshedObservationSurvives(t *testing.T) {
	// Re-observing an id (a retried dispatch reuses the trace id) refreshes its
	// observation time, so it is not TTL-evicted on the old timestamp.
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8, TTL: time.Hour})
	clock := time.Unix(2_000_000, 0).UTC()
	r.now = func() time.Time { return clock }

	r.Observe(Record{QueryID: "retried", ShapeID: "cerb:a"})
	clock = clock.Add(50 * time.Minute)
	r.Observe(Record{QueryID: "retried", ShapeID: "cerb:b"}) // refresh
	clock = clock.Add(20 * time.Minute)                      // 70m since first, 20m since refresh

	if n := r.evictExpired(); n != 0 {
		t.Fatalf("evictExpired = %d; want 0 (refreshed id is within TTL)", n)
	}
	if ids := r.snapshotIDs(); len(ids) != 1 || ids[0] != "retried" {
		t.Errorf("refreshed id evicted: ids = %v; want [retried]", ids)
	}
}

func TestEvictExpired_DisabledWhenTTLNonPositive(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 8}) // TTL unset (0)
	clock := time.Unix(3_000_000, 0).UTC()
	r.now = func() time.Time { return clock }
	r.Observe(Record{QueryID: "x", ShapeID: "cerb:scan"})
	clock = clock.Add(1000 * time.Hour)
	if n := r.evictExpired(); n != 0 {
		t.Fatalf("evictExpired with TTL<=0 = %d; want 0 (disabled)", n)
	}
	if len(r.snapshotIDs()) != 1 {
		t.Error("TTL disabled but id was evicted")
	}
}

func TestObserve_EmptyQueryID_Ignored(t *testing.T) {
	r := New(newFakeSource(), &memSink{}, Options{RingCapacity: 4})
	r.Observe(Record{QueryID: "", ShapeID: "cerb:scan"})
	if len(r.snapshotIDs()) != 0 {
		t.Error("empty query_id was recorded; want ignored")
	}
}

func TestReconcileOnce_JoinsRowToShape(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})

	r.Observe(Record{QueryID: "qid-1", ShapeID: "cerb:agg;agg=2", Opts: []string{"aggregation_in_order"}, Language: "promql"})
	src.seed(SourceRow{
		QueryID:             "qid-1",
		NormalizedQueryHash: 42,
		ReadRows:            1000,
		ReadBytes:           8000,
		QueryDurationMS:     12,
		MemoryUsage:         2048,
		ProfileEvents:       map[string]int64{"QueryConditionCacheHits": 3},
	})

	r.reconcileOnce(context.Background())

	rows := sink.snapshot()
	if len(rows) != 1 {
		t.Fatalf("sink rows = %d; want 1", len(rows))
	}
	got := rows[0]
	if got.ShapeID != "cerb:agg;agg=2" || got.Language != "promql" {
		t.Errorf("shape join wrong: %+v", got)
	}
	if got.NormalizedQueryHash != 42 || got.ReadRows != 1000 || got.MemoryUsage != 2048 {
		t.Errorf("cost columns not joined: %+v", got)
	}
	if got.ProfileEvents["QueryConditionCacheHits"] != 3 {
		t.Errorf("profile events not carried: %+v", got.ProfileEvents)
	}

	// After a successful write the id is forgotten, so a second reconcile does
	// not re-write it.
	r.reconcileOnce(context.Background())
	if n := len(sink.snapshot()); n != 1 {
		t.Errorf("reconciled id re-written; sink rows = %d, want 1", n)
	}
}

func TestReconcileOnce_SourceError_NoWrite_NoForget(t *testing.T) {
	src := newFakeSource()
	src.err = context.DeadlineExceeded
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8})
	r.Observe(Record{QueryID: "qid-err", ShapeID: "cerb:scan"})

	r.reconcileOnce(context.Background()) // must not panic / write

	if len(sink.snapshot()) != 0 {
		t.Error("wrote rows despite source error")
	}
	// id retained for retry.
	if len(r.snapshotIDs()) != 1 {
		t.Error("forgot id despite source error; want retained for retry")
	}
}

func TestReconcileOnce_SinkError_RetainsForRetry(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{err: os.ErrPermission}
	r := New(src, sink, Options{RingCapacity: 8})
	r.Observe(Record{QueryID: "qid-2", ShapeID: "cerb:scan"})
	src.seed(SourceRow{QueryID: "qid-2"})

	r.reconcileOnce(context.Background())

	if len(r.snapshotIDs()) != 1 {
		t.Error("forgot id despite sink write failure; want retained for retry")
	}
}

func TestRun_GracefulShutdown(t *testing.T) {
	src := newFakeSource()
	sink := &memSink{}
	r := New(src, sink, Options{RingCapacity: 8, Interval: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	// Let a couple ticks fire, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestJSONLSink_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")
	sink, err := NewJSONLSink(path)
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}
	rows := []Row{
		{ShapeID: "cerb:scan", Opts: []string{"condition_cache"}, Language: "logql", ReadRows: 5},
		{ShapeID: "cerb:agg;agg=1", Language: "promql", MemoryUsage: 99},
	}
	if err := sink.Write(rows); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	var got []Row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row Row
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		got = append(got, row)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d rows; want 2", len(got))
	}
	if got[0].ShapeID != "cerb:scan" || got[0].ReadRows != 5 || got[0].Opts[0] != "condition_cache" {
		t.Errorf("row 0 round-trip wrong: %+v", got[0])
	}
	if got[1].ShapeID != "cerb:agg;agg=1" || got[1].MemoryUsage != 99 {
		t.Errorf("row 1 round-trip wrong: %+v", got[1])
	}
}

func TestJSONLSink_AppendsAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")
	s1, err := NewJSONLSink(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	_ = s1.Write([]Row{{ShapeID: "a"}})
	_ = s1.Close()

	s2, err := NewJSONLSink(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	_ = s2.Write([]Row{{ShapeID: "b"}})
	_ = s2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		lines++
	}
	if lines != 2 {
		t.Errorf("file has %d lines; want 2 (appended, not truncated)", lines)
	}
}

func TestNewJSONLSink_EmptyPath(t *testing.T) {
	if _, err := NewJSONLSink(""); err == nil {
		t.Error("NewJSONLSink(\"\"): want error, got nil")
	}
}
