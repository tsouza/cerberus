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
