package chclient

import (
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
)

// histogram_scan_contract_test.go — the ratchet joining the two halves of
// the native-histogram decode contract.
//
// internal/chsql/histogram_projection.go PINS the ClickHouse type of each
// of the nine Histogram*Column outputs; [HistogramValue] declares the Go
// destination [rowsCursor.Next] binds for each. Nothing enforced that the
// two agreed, and they did not: the `rate()` / `increase()` lowerings
// emitted Float64 counts and Array(Float64) bucket ladders while
// HistogramValue declared uint64 / []uint64, so clickhouse-go/v2's
// Float64.ScanRow rejected the destination outright and the query failed
// at its first row (issue #1967).
//
// Nothing caught that, because the only test of the thirteen-destination
// scan drives a hand-written fake whose Scan type-ASSERTS its
// destinations instead of CONVERTING to them (cursor_test.go's fakeRows):
// its fixture is a HistogramValue, so the two sides agree by
// construction, and the test cannot express a conversion failure. The
// spec lane cannot see it either — it scans into `*any`.
//
// These tests close that class by driving the REAL driver conversion:
// clickhouse-go's own column implementations, the same ScanRow the
// production cursor reaches. A drift on either side — an emitter cast
// changed, a HistogramValue field re-typed — fails here, locally, without
// a ClickHouse server.

// histogramScanSlot is one output column's end-to-end contract: the
// ClickHouse type chsql's emitter pins it to, and the [HistogramValue]
// field the row cursor binds for it, in the positional order
// [rowsCursor.Next] scans them.
type histogramScanSlot struct {
	// column is the chplan.Histogram*Column output alias.
	column string
	// chType is the ClickHouse type internal/chsql's emitter pins this
	// column to.
	//
	// This file does NOT read the emitter, and cannot: chclient declares
	// no internal dependencies in .go-arch-lint.yml, the same constraint
	// that makes histogramLastColumn a duplicated literal. So this table
	// is maintained in lockstep with the emitter rather than derived from
	// it, and the emitter's own half is pinned separately by
	// chsql.TestEmit_HistogramProjection_ShapeSanity plus the `-- sql --`
	// cells of test/spec/promql/exp_histogram_*.txtar. What this file
	// uniquely pins is the half neither of those can see: that the type
	// named here is one clickhouse-go will actually scan into the
	// HistogramValue field bound at the same position.
	chType string
	// sample is a representative value to round-trip.
	sample any
	// dest returns the destination pointer the row cursor binds for this
	// column, into hv.
	dest func(hv *HistogramValue) any
	// got reads the decoded value back for comparison against sample.
	got func(hv *HistogramValue) any
}

func histogramScanSlots() []histogramScanSlot {
	return []histogramScanSlot{
		{
			column: "HistogramCount", chType: "Float64", sample: 0.1,
			dest: func(hv *HistogramValue) any { return &hv.Count },
			got:  func(hv *HistogramValue) any { return hv.Count },
		},
		{
			column: "HistogramSum", chType: "Float64", sample: 42.5,
			dest: func(hv *HistogramValue) any { return &hv.Sum },
			got:  func(hv *HistogramValue) any { return hv.Sum },
		},
		{
			column: "HistogramScale", chType: "Int32", sample: int32(-1),
			dest: func(hv *HistogramValue) any { return &hv.Scale },
			got:  func(hv *HistogramValue) any { return hv.Scale },
		},
		{
			column: "HistogramZeroThreshold", chType: "Float64", sample: 1e-128,
			dest: func(hv *HistogramValue) any { return &hv.ZeroThreshold },
			got:  func(hv *HistogramValue) any { return hv.ZeroThreshold },
		},
		{
			column: "HistogramZeroCount", chType: "Float64", sample: 0.25,
			dest: func(hv *HistogramValue) any { return &hv.ZeroCount },
			got:  func(hv *HistogramValue) any { return hv.ZeroCount },
		},
		{
			column: "HistogramPositiveOffset", chType: "Int32", sample: int32(3),
			dest: func(hv *HistogramValue) any { return &hv.PositiveOffset },
			got:  func(hv *HistogramValue) any { return hv.PositiveOffset },
		},
		{
			column: "HistogramPositiveBucketCounts", chType: "Array(Float64)", sample: []float64{0.05, 0.05},
			dest: func(hv *HistogramValue) any { return &hv.PositiveBucketCounts },
			got:  func(hv *HistogramValue) any { return hv.PositiveBucketCounts },
		},
		{
			column: "HistogramNegativeOffset", chType: "Int32", sample: int32(-2),
			dest: func(hv *HistogramValue) any { return &hv.NegativeOffset },
			got:  func(hv *HistogramValue) any { return hv.NegativeOffset },
		},
		{
			column: "HistogramNegativeBucketCounts", chType: "Array(Float64)", sample: []float64{2.5, 7.5},
			dest: func(hv *HistogramValue) any { return &hv.NegativeBucketCounts },
			got:  func(hv *HistogramValue) any { return hv.NegativeBucketCounts },
		},
	}
}

// TestHistogramScanContract_PinnedTypesScanIntoHistogramValue drives every
// pinned column type through clickhouse-go's real ScanRow into the real
// HistogramValue destination. This is the assertion the shipped
// rate()/increase() lowerings would have failed before the emitter pinned
// its output types.
func TestHistogramScanContract_PinnedTypesScanIntoHistogramValue(t *testing.T) {
	t.Parallel()

	slots := histogramScanSlots()
	if len(slots) != histogramColumnCount {
		t.Fatalf("contract covers %d columns, but the cursor binds %d destinations", len(slots), histogramColumnCount)
	}
	// The last slot must be the column the row cursor probes the result
	// shape by, or the probe and this contract are describing different
	// projections.
	if last := slots[len(slots)-1].column; last != histogramLastColumn {
		t.Errorf("contract ends at %q, but the shape probe latches on %q", last, histogramLastColumn)
	}

	for _, slot := range slots {
		t.Run(slot.column, func(t *testing.T) {
			t.Parallel()

			col, err := column.Type(slot.chType).Column(slot.column, nil)
			if err != nil {
				t.Fatalf("construct %s column: %v", slot.chType, err)
			}
			if err := col.AppendRow(slot.sample); err != nil {
				t.Fatalf("append %v to %s: %v", slot.sample, slot.chType, err)
			}

			var hv HistogramValue
			if err := col.ScanRow(slot.dest(&hv), 0); err != nil {
				t.Fatalf("ScanRow %s into the HistogramValue destination: %v\n"+
					"the emitter's pinned type and the decode destination have drifted apart "+
					"(internal/chsql/histogram_projection.go vs HistogramValue)", slot.chType, err)
			}
			assertScanEqual(t, slot.got(&hv), slot.sample)
		})
	}
}

// TestHistogramScanContract_UnpinnedCountTypeIsRejected is the negative
// control: it pins WHY the emitter casts rather than forwarding the
// physical column. A rate() result is Float64; the pre-fix decode
// destination was *uint64; the driver rejects that pairing rather than
// silently truncating, which is what made the shipped lowering fail on
// its first row.
//
// Without this case the positive test above would still pass if someone
// re-typed both sides back to an integer, quietly reintroducing the bug
// for every fractional-count shape.
func TestHistogramScanContract_UnpinnedCountTypeIsRejected(t *testing.T) {
	t.Parallel()

	col, err := column.Type("Float64").Column("HistogramCount", nil)
	if err != nil {
		t.Fatalf("construct Float64 column: %v", err)
	}
	if err := col.AppendRow(0.1); err != nil {
		t.Fatalf("append 0.1: %v", err)
	}
	var into uint64
	if err := col.ScanRow(&into, 0); err == nil {
		t.Fatal("clickhouse-go accepted a *uint64 destination for a Float64 column; " +
			"the integer destination this repo used to bind would no longer be a bug, " +
			"so this control no longer explains why HistogramValue.Count is float64")
	}
}

// assertScanEqual compares a decoded value against the appended sample,
// handling the two slice-valued slots.
func assertScanEqual(t *testing.T, got, want any) {
	t.Helper()
	gotSlice, isSlice := got.([]float64)
	if !isSlice {
		if got != want {
			t.Errorf("decoded %v (%T), want %v (%T)", got, got, want, want)
		}
		return
	}
	wantSlice, ok := want.([]float64)
	if !ok {
		t.Fatalf("sample for a slice-valued slot is %T, want []float64", want)
	}
	if len(gotSlice) != len(wantSlice) {
		t.Fatalf("decoded %v, want %v", gotSlice, wantSlice)
	}
	for i := range gotSlice {
		if gotSlice[i] != wantSlice[i] {
			t.Errorf("decoded[%d] = %v, want %v", i, gotSlice[i], wantSlice[i])
		}
	}
}
