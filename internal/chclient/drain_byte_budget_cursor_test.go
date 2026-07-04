package chclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fatSample builds a Sample whose Labels map carries a distinct ~valueBytes
// payload (distinct so each interns to a unique series and is charged once).
func fatSample(i, valueBytes int) Sample {
	return Sample{
		MetricName: "s",
		Labels:     map[string]string{"payload": fmt.Sprintf("%d-%s", i, strings.Repeat("x", valueBytes))},
		Timestamp:  time.Unix(0, 0),
		Value:      1,
	}
}

// TestRowsCursor_DrainByteBudget_TripsMidStream proves the wide-projection byte
// charge fires on the PRODUCTION rowsCursor drain and aborts fail-closed BEFORE
// buffering the full result — the core of the OOM class fix. Non-vacuity is the
// paired control: the same rows under a huge ceiling drain fully with no error.
func TestRowsCursor_DrainByteBudget_TripsMidStream(t *testing.T) {
	t.Parallel()
	const valueBytes = 1000
	mkRows := func() *freshLabelRows {
		return &freshLabelRows{rows: []Sample{
			fatSample(1, valueBytes), fatSample(2, valueBytes),
			fatSample(3, valueBytes), fatSample(4, valueBytes),
		}}
	}

	// Control: a ceiling far above the cumulative bytes → full drain, no error.
	full := &rowsCursor{rows: mkRows(), byteBudget: NewDrainByteBudget(1 << 20)}
	defer func() { _ = full.Close() }()
	var drained int
	for full.Next() {
		drained++
	}
	if err := full.Err(); err != nil {
		t.Fatalf("control drain errored: %v", err)
	}
	if drained != 4 {
		t.Fatalf("control drained %d rows, want 4 — fixture did not fully drain under a huge ceiling", drained)
	}

	// Trip: a ceiling below the cumulative (~4×1007) but above a single map
	// (~1007) → the drain crosses it mid-stream.
	trip := &rowsCursor{rows: mkRows(), byteBudget: NewDrainByteBudget(2500)}
	defer func() { _ = trip.Close() }()
	var got int
	for trip.Next() {
		got++
	}
	err := trip.Err()
	if !errors.Is(err, ErrDrainBytesExceeded) {
		t.Fatalf("Err = %v, want ErrDrainBytesExceeded", err)
	}
	var be *DrainByteBudgetError
	if !errors.As(err, &be) || be.Limit != 2500 {
		t.Fatalf("want *DrainByteBudgetError{Limit:2500}, got %+v", err)
	}
	if got >= 4 {
		t.Errorf("drained %d rows before aborting, want < 4 (must abort before full materialisation)", got)
	}
}

// TestRowsCursor_DrainByteBudget_PerUniqueSeries proves the charge is per UNIQUE
// interned series, not per row: many rows aliasing ONE fat map cost one map on
// the heap and must NOT trip a ceiling just above that single map. Charging
// per-row would wrongly reject this legitimate many-identical-spans result.
func TestRowsCursor_DrainByteBudget_PerUniqueSeries(t *testing.T) {
	t.Parallel()
	shared := map[string]string{"payload": strings.Repeat("x", 1000)}
	rows := make([]Sample, 100)
	for i := range rows {
		// Same map CONTENTS every row → interns to one series after the first.
		rows[i] = Sample{MetricName: "s", Labels: shared, Timestamp: time.Unix(int64(i), 0), Value: 1}
	}
	// Ceiling just above one map (~1007) but far below 100×1007: per-row charging
	// would trip; per-unique must not.
	cur := &rowsCursor{rows: &freshLabelRows{rows: rows}, byteBudget: NewDrainByteBudget(2000)}
	defer func() { _ = cur.Close() }()
	var drained int
	for cur.Next() {
		drained++
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("per-unique drain errored (%v) — the charge over-counted aliased rows", err)
	}
	if drained != 100 {
		t.Fatalf("drained %d rows, want 100 — aliased rows should share one charge", drained)
	}
}

func TestDrainByteBudget_ConsumeAndContext(t *testing.T) {
	t.Parallel()
	b := NewDrainByteBudget(100)
	if !b.consume(60) || !b.consume(40) {
		t.Fatal("consume within limit returned false")
	}
	if b.consume(1) {
		t.Fatal("consume past limit returned true")
	}
	// Inert (non-positive) budget is never consulted.
	if inert := NewDrainByteBudget(0); !inert.consume(1_000_000) {
		t.Fatal("inert budget consumed")
	}
	// Context round-trip: attached active budget comes back; inert one reads nil.
	ctx := WithDrainByteBudget(context.Background(), NewDrainByteBudget(5))
	if drainByteBudgetFromContext(ctx) == nil {
		t.Fatal("active budget not retrieved from context")
	}
	if drainByteBudgetFromContext(WithDrainByteBudget(context.Background(), NewDrainByteBudget(0))) != nil {
		t.Fatal("inert budget must read back nil")
	}
	if drainByteBudgetFromContext(context.Background()) != nil {
		t.Fatal("no budget must read back nil")
	}
}

func TestLabelMapBytes(t *testing.T) {
	t.Parallel()
	if got := labelMapBytes(map[string]string{"ab": "cde", "f": "g"}); got != 2+3+1+1 {
		t.Fatalf("labelMapBytes = %d, want 7", got)
	}
	if got := labelMapBytes(nil); got != 0 {
		t.Fatalf("labelMapBytes(nil) = %d, want 0", got)
	}
}
