package chclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
)

// mkMatrixCols builds a fake matrixCols block directly off ch-go's proto
// column types (Append is all real ch-go code — no fake/mock column
// implementation of our own), bypassing the network entirely.
// decodeBlock's charge path only ever touches the typed
// Keys/Values/Offsets/Row accessors these expose, so a hand-built block
// exercises the exact same code the wire decode does, byte-identically,
// without a live ClickHouse connection.
func mkMatrixCols(labelSets []map[string]string) matrixCols {
	name := new(chproto.ColStr)
	attrs := chproto.NewMap[string, string](new(chproto.ColStr), new(chproto.ColStr))
	ts := new(chproto.ColDateTime)
	val := new(chproto.ColFloat64)
	for i, labels := range labelSets {
		name.Append("s")
		attrs.Append(labels)
		ts.Append(time.Unix(int64(i), 0))
		val.Append(1)
	}
	return matrixCols{name: name, attrs: attrs, ts: ts, val: val}
}

// fatLabels builds a label map whose single value carries a distinct
// ~valueBytes payload (distinct so each interns to a unique series and is
// charged once), mirroring drain_byte_budget_cursor_test.go's fatSample.
func fatLabels(i, valueBytes int) map[string]string {
	return map[string]string{"payload": fmt.Sprintf("%d-%s", i, strings.Repeat("x", valueBytes))}
}

// TestColumnarCursor_DrainByteBudget_TripsMidStream is the columnarCursor
// sibling of TestRowsCursor_DrainByteBudget_TripsMidStream
// (drain_byte_budget_cursor_test.go) — same OOM-class guarantee, but for the
// OTHER production entrypoint that charges the wide-projection byte budget:
// internal/chclient/columnar.go's decodeBlock, engaged by the columnar
// query_range matrix decode path. Before this test, only the rowsCursor
// charge site had unit coverage (#1540(c)) — a regression that dropped the
// charge from decodeBlock was caught by nothing but the Docker-only
// columnar integration lane, which doesn't probe the byte axis at all.
func TestColumnarCursor_DrainByteBudget_TripsMidStream(t *testing.T) {
	t.Parallel()
	const valueBytes = 1000
	mkCols := func() matrixCols {
		return mkMatrixCols([]map[string]string{
			fatLabels(1, valueBytes), fatLabels(2, valueBytes),
			fatLabels(3, valueBytes), fatLabels(4, valueBytes),
		})
	}

	// Control: a ceiling far above the cumulative bytes → full decode, no error.
	full := &columnarCursor{byteBudget: NewDrainByteBudget(1 << 20)}
	if err := full.decodeBlock(mkCols(), 4); err != nil {
		t.Fatalf("control decode errored: %v", err)
	}
	if len(full.samples) != 4 {
		t.Fatalf("control decoded %d samples, want 4 — fixture did not fully decode under a huge ceiling", len(full.samples))
	}

	// Trip: a ceiling below the cumulative (~4×1007) but above a single map
	// (~1007) → the decode crosses it mid-block.
	trip := &columnarCursor{byteBudget: NewDrainByteBudget(2500)}
	err := trip.decodeBlock(mkCols(), 4)
	if !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("decodeBlock err = %v, want errBudgetExceeded", err)
	}
	if !errors.Is(trip.err, ErrDrainBytesExceeded) {
		t.Fatalf("trip.err = %v, want ErrDrainBytesExceeded", trip.err)
	}
	var be *DrainByteBudgetError
	if !errors.As(trip.err, &be) || be.Limit != 2500 {
		t.Fatalf("want *DrainByteBudgetError{Limit:2500}, got %+v", trip.err)
	}
	if len(trip.samples) >= 4 {
		t.Errorf("decoded %d samples before aborting, want < 4 (must abort before full materialisation)", len(trip.samples))
	}
}

// TestColumnarCursor_DrainByteBudget_PerUniqueSeries mirrors
// TestRowsCursor_DrainByteBudget_PerUniqueSeries: many contiguous rows
// aliasing ONE fat map must charge once, not once per row — decodeBlock's
// sameSpan pre-check is exactly what makes that true, and this pins it
// against the byte budget the same way the row path is pinned.
func TestColumnarCursor_DrainByteBudget_PerUniqueSeries(t *testing.T) {
	t.Parallel()
	shared := map[string]string{"payload": strings.Repeat("x", 1000)}
	labelSets := make([]map[string]string, 100)
	for i := range labelSets {
		labelSets[i] = shared
	}
	// Ceiling just above one map (~1007) but far below 100×1007: per-row
	// charging would trip; per-unique-contiguous-run must not.
	cur := &columnarCursor{byteBudget: NewDrainByteBudget(5000)}
	if err := cur.decodeBlock(mkMatrixCols(labelSets), len(labelSets)); err != nil {
		t.Fatalf("per-unique decode errored (%v) — the charge over-counted a contiguous identical run", err)
	}
	if len(cur.samples) != 100 {
		t.Fatalf("decoded %d samples, want 100 — a contiguous identical run should share one charge", len(cur.samples))
	}
}

// TestColumnarCursor_DrainByteBudget_Inert proves an inert (nil) byte
// budget — the shape every non-Tempo columnar decode runs under, since
// WithDrainByteBudget is only ever attached on the Tempo span-search path —
// never charges and never trips, regardless of how fat the decoded maps
// are.
func TestColumnarCursor_DrainByteBudget_Inert(t *testing.T) {
	t.Parallel()
	cols := mkMatrixCols([]map[string]string{fatLabels(1, 1<<20)})
	cur := &columnarCursor{}
	if err := cur.decodeBlock(cols, 1); err != nil {
		t.Fatalf("decode with no byte budget errored: %v", err)
	}
	if len(cur.samples) != 1 {
		t.Fatalf("decoded %d samples, want 1", len(cur.samples))
	}
}
