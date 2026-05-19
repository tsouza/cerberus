package chclient

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Layer 11 — OOM-class / streaming-cursor verification.
//
// We can't drive a real ClickHouse server here, but the cursor surface
// is the canonical streaming primitive; verifying it doesn't materialise
// the full row set on the Go side pins the OOM-safety contract handlers
// rely on.

// TestStreamingCursor_Bounded_1M_Points — drain a 1M-row cursor through
// the rowsCursor and assert HeapAlloc delta stays bounded. The
// generator-backed fake rows produces one row at a time, so any
// regression that buffers the full result set on the Go side would
// dominate the heap delta.
//
// Bound: 32 MiB. A regression that buffers every row internally would
// blow this; the streaming path keeps it well below.
//
// Runtime: the generator-backed rows yield one sample per Next, so the
// 1M-iteration sweep finishes in <100ms (≈400ms under -race). Cheap
// enough to always run; no -short skip.
func TestStreamingCursor_Bounded_1M_Points(t *testing.T) {
	const N = 1_000_000

	rows := newGenRows(N)
	cursor := &rowsCursor{rows: rows}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	count := 0
	for cursor.Next() {
		s := cursor.Sample()
		if s.MetricName == "" {
			t.Fatalf("row %d: empty MetricName", count)
		}
		count++
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if count != N {
		t.Fatalf("drained %d rows, want %d", count, N)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	var delta uint64
	if after.HeapAlloc > before.HeapAlloc {
		delta = after.HeapAlloc - before.HeapAlloc
	}
	const ceiling = 32 << 20 // 32 MiB
	if delta > ceiling {
		t.Errorf("heap delta %.1f MiB exceeded 32 MiB ceiling (drained %d rows)",
			float64(delta)/(1<<20), count)
	}
	t.Logf("1M-row drain: heap delta = %.2f MiB (ceiling %d MiB)",
		float64(delta)/(1<<20), ceiling>>20)
}

// TestStreamingCursor_StopMid_FreesBuffers — drain half then close.
// Pins the contract that Close releases the underlying rows so the
// driver can return its slot to the pool.
func TestStreamingCursor_StopMid_FreesBuffers(t *testing.T) {
	t.Parallel()
	rows := newGenRows(100_000)
	cursor := &rowsCursor{rows: rows}

	for i := 0; i < 1000; i++ {
		if !cursor.Next() {
			t.Fatalf("Next: ran out at %d", i)
		}
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rows.closed {
		t.Fatal("rows.Close was not invoked")
	}
	// Close is idempotent.
	if err := cursor.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestStreamingCursor_BackPressure — slow consumer model. The producer
// (fake rows) pumps rows as fast as Next is called; if Next is paced
// by the consumer, the cursor stays bounded.
func TestStreamingCursor_BackPressure(t *testing.T) {
	t.Parallel()
	rows := newGenRows(10_000)
	cursor := &rowsCursor{rows: rows}
	defer func() { _ = cursor.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		for cursor.Next() {
			count++
			if count%1000 == 0 {
				time.Sleep(10 * time.Microsecond)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("back-pressure drain hung")
	}
}

// TestStreamingCursor_DoubleNext_Idempotent — calling Next after EOF
// must stay false (no flip-flop).
func TestStreamingCursor_DoubleNext_Idempotent(t *testing.T) {
	t.Parallel()
	rows := newGenRows(3)
	cursor := &rowsCursor{rows: rows}
	defer func() { _ = cursor.Close() }()

	count := 0
	for cursor.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("count: got %d, want 3", count)
	}
	for range 10 {
		if cursor.Next() {
			t.Fatal("Next: flipped to true after EOF")
		}
	}
}

// TestStreamingCursor_CloseBeforeNext — closing without iterating must
// release rows and stay safe. The cursor's contract does not document
// behaviour for "Next after Close" — handlers always Next-then-Close —
// so we exercise the early-Close path on its own.
func TestStreamingCursor_CloseBeforeNext(t *testing.T) {
	t.Parallel()
	rows := newGenRows(10)
	cursor := &rowsCursor{rows: rows}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rows.closed {
		t.Fatal("rows.Close not invoked on early-Close")
	}
}

// genRows is a generator-style driver.Rows that materialises one row at
// a time instead of holding a slice. Lets us drive 1M iterations
// without 1M sample allocations dominating the measurement.
type genRows struct {
	remaining int
	closed    bool
}

func newGenRows(n int) *genRows { return &genRows{remaining: n} }

func (r *genRows) Next() bool {
	if r.remaining <= 0 || r.closed {
		return false
	}
	r.remaining--
	return true
}

func (r *genRows) Scan(dest ...any) error {
	if len(dest) != 4 {
		return errors.New("genRows.Scan: want 4 destinations")
	}
	if p, ok := dest[0].(*string); ok {
		*p = "up"
	}
	if p, ok := dest[1].(*map[string]string); ok {
		*p = nil
	}
	if p, ok := dest[2].(*time.Time); ok {
		*p = time.Unix(0, 0)
	}
	if p, ok := dest[3].(*float64); ok {
		*p = 1.0
	}
	return nil
}

func (r *genRows) ScanStruct(any) error {
	return errors.New("test mock: ScanStruct unused by cursor-chaos tests")
}
func (r *genRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *genRows) Totals(...any) error              { return nil }
func (r *genRows) Columns() []string                { return nil }
func (r *genRows) Err() error                       { return nil }
func (r *genRows) HasData() bool                    { return r.remaining > 0 }
func (r *genRows) Close() error {
	r.closed = true
	return nil
}
