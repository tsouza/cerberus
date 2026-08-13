package chclient

import (
	"errors"
	"math"
	"testing"
	"time"
)

// histogram_drain_byte_budget_test.go — the byte axis of a histogram-valued
// matrix drain (issue #2038).
//
// A float Sample is a metric name, an interned label map shared across the
// series' run, a timestamp and a float64, so "rows × a small constant" is a
// fair proxy for the Go heap a buffered matrix costs, and the row budget
// (MaxQuerySamples) bounds it adequately. A histogram-valued row is not: it
// additionally owns a heap HistogramValue and two []float64 bucket ladders
// sized by the histogram's bucket count, so the SAME row cap admits one to two
// orders of magnitude more bytes. These tests pin that the drain byte budget
// closes that gap on both decode paths — and that a float matrix under the
// identical budget is untouched.

// histogramBucketCount is the per-ladder bucket count the fixtures below use.
// A native histogram at scale 3 covering four decades carries a ladder of this
// order, so it is a realistic row rather than a pathological one — the point of
// the fixture is that ORDINARY histogram rows blow a byte ceiling the row cap
// would wave through.
const histogramBucketCount = 128

// fatHistogram builds a HistogramValue with histogramBucketCount buckets in
// each ladder — the shape whose per-row heap the row budget cannot see.
func fatHistogram(seed float64) *HistogramValue {
	pos := make([]float64, histogramBucketCount)
	neg := make([]float64, histogramBucketCount)
	for i := range pos {
		pos[i] = seed + float64(i)
		neg[i] = seed - float64(i)
	}
	return &HistogramValue{
		Count: seed, Sum: seed * 2, Scale: 3, ZeroThreshold: 1e-128, ZeroCount: 1,
		PositiveOffset: -2, PositiveBucketCounts: pos,
		NegativeOffset: -5, NegativeBucketCounts: neg,
	}
}

// histogramFixtureRows is the fixture height. It is far below
// histogramFixtureMaxSamples so the ROW budget admits every row — the whole
// point being that the byte budget must stop the drain anyway.
const histogramFixtureRows = 40

// histogramFixtureMaxSamples is the configured row cap the byte ceiling is
// derived from (NewMatrixDrainBudget). 100 rows × 128 B/row = 12,800 bytes,
// which is ~6 of these histogram rows and ~1,800 of the float rows the const
// was sized against.
const histogramFixtureMaxSamples = 100

// mkHistogramBlock builds a `rows`-high histogram-shaped result block, every
// row on ONE series (contiguous identical labels) so the per-series label-map
// charge is paid once and what the ceiling measures is the histogram payload.
func mkHistogramBlock(t *testing.T, rows int) (*columnarCursor, matrixCols) {
	t.Helper()
	ts := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	labels := map[string]string{"job": "api"}
	res := histogramShapedResults()
	for i := range rows {
		appendHistogramRow(res, "latency", labels, ts.Add(time.Duration(i)*time.Second), *fatHistogram(float64(i)))
	}
	cur := &columnarCursor{results: res, responseShape: ResponseShapeMatrix}
	cols, ok := cur.bindColumns()
	if !ok {
		t.Fatal("bindColumns declined the histogram-valued matrix shape")
	}
	return cur, cols
}

// TestColumnarCursor_HistogramByteBudget_TripsUnderTheRowBudget is the core
// assertion: a histogram matrix well inside the configured ROW cap is stopped
// by the byte budget derived from that same cap, instead of buffering in full.
// The non-vacuity control is the same fixture under the row budget alone —
// which admits every row, demonstrating that rows are the poor proxy for bytes
// this budget exists to correct.
func TestColumnarCursor_HistogramByteBudget_TripsUnderTheRowBudget(t *testing.T) {
	t.Parallel()

	// Control: the row budget alone, at the SAME configured cap, waves the
	// whole fixture through — 40 rows against a 100-row cap.
	control, controlCols := mkHistogramBlock(t, histogramFixtureRows)
	control.maxSamples = histogramFixtureMaxSamples
	if err := control.decodeBlock(controlCols, histogramFixtureRows); err != nil {
		t.Fatalf("row budget alone rejected the fixture (%v) — it must admit every row for this test to mean anything", err)
	}
	if len(control.samples) != histogramFixtureRows {
		t.Fatalf("row budget alone buffered %d rows, want all %d", len(control.samples), histogramFixtureRows)
	}

	// The byte budget derived from that same cap stops the identical drain.
	budget := NewMatrixDrainBudget(histogramFixtureMaxSamples)
	trip, tripCols := mkHistogramBlock(t, histogramFixtureRows)
	trip.maxSamples = histogramFixtureMaxSamples
	trip.byteBudget = budget

	err := trip.decodeBlock(tripCols, histogramFixtureRows)
	if !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("decodeBlock err = %v, want errBudgetExceeded", err)
	}
	if !errors.Is(trip.err, ErrDrainBytesExceeded) {
		t.Fatalf("trip.err = %v, want ErrDrainBytesExceeded", trip.err)
	}
	var be *DrainByteBudgetError
	if !errors.As(trip.err, &be) || be.Limit != budget.Limit() {
		t.Fatalf("want *DrainByteBudgetError{Limit:%d}, got %+v", budget.Limit(), trip.err)
	}
	if len(trip.samples) >= histogramFixtureRows {
		t.Errorf("buffered %d rows before aborting, want < %d (must abort before full materialisation)",
			len(trip.samples), histogramFixtureRows)
	}
	// The abort must land near where the heap actually crosses the ceiling,
	// not on the first row: a charge that over-counted (e.g. per bucket rather
	// than per bucket ELEMENT) would reject a servable histogram query.
	if len(trip.samples) == 0 {
		t.Error("aborted before buffering a single row — the per-row charge is over-counting")
	}
}

// TestColumnarCursor_FloatMatrixUnchangedUnderMatrixBudget is the
// no-regression half: the identical budget, the identical row count, but float
// rows — the shape the row cap was already a fair proxy for. It must decode in
// full, so attaching the byte budget to every prom drain changes nothing for
// the overwhelmingly common shape.
func TestColumnarCursor_FloatMatrixUnchangedUnderMatrixBudget(t *testing.T) {
	t.Parallel()

	labelSets := make([]map[string]string, histogramFixtureRows)
	for i := range labelSets {
		// A distinct label set per row — the WORST case for the per-series
		// charge (no interning dedup at all), and still far under the ceiling.
		labelSets[i] = map[string]string{"job": "api", "instance": fatLabels(i, 32)["payload"]}
	}
	cur := &columnarCursor{
		maxSamples: histogramFixtureMaxSamples,
		byteBudget: NewMatrixDrainBudget(histogramFixtureMaxSamples),
	}
	if err := cur.decodeBlock(mkMatrixCols(labelSets), histogramFixtureRows); err != nil {
		t.Fatalf("float matrix tripped the matrix drain budget (%v) — the budget must be inert for the float shape", err)
	}
	if len(cur.samples) != histogramFixtureRows {
		t.Fatalf("buffered %d float rows, want all %d", len(cur.samples), histogramFixtureRows)
	}
}

// TestRowsCursor_HistogramByteBudget_TripsMidStream pins the SAME guarantee on
// the OTHER decode path. It is not redundant coverage: the columnar binder
// declines a stock OTel-CH deployment's `Map(LowCardinality(String), String)`
// label column outright, so in production a histogram matrix is decoded by the
// row cursor. A charge landed only in decodeBlock would leave the deployed
// shape unbounded.
func TestRowsCursor_HistogramByteBudget_TripsMidStream(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	mkRows := func() *fakeRows {
		samples := make([]Sample, histogramFixtureRows)
		for i := range samples {
			samples[i] = Sample{
				MetricName: "latency",
				Labels:     map[string]string{"job": "api"},
				Timestamp:  ts.Add(time.Duration(i) * time.Second),
				Histogram:  fatHistogram(float64(i)),
			}
		}
		return &fakeRows{columns: histogramProjectionColumns, samples: samples}
	}

	// Control: the row cap alone admits the whole fixture.
	control := &rowsCursor{rows: mkRows(), maxSamples: histogramFixtureMaxSamples}
	defer func() { _ = control.Close() }()
	var admitted int
	for control.Next() {
		admitted++
	}
	if err := control.Err(); err != nil {
		t.Fatalf("row budget alone errored: %v", err)
	}
	if admitted != histogramFixtureRows {
		t.Fatalf("row budget alone drained %d rows, want all %d", admitted, histogramFixtureRows)
	}

	budget := NewMatrixDrainBudget(histogramFixtureMaxSamples)
	trip := &rowsCursor{rows: mkRows(), maxSamples: histogramFixtureMaxSamples, byteBudget: budget}
	defer func() { _ = trip.Close() }()
	var drained int
	for trip.Next() {
		drained++
	}
	err := trip.Err()
	if !errors.Is(err, ErrDrainBytesExceeded) {
		t.Fatalf("Err = %v, want ErrDrainBytesExceeded", err)
	}
	var be *DrainByteBudgetError
	if !errors.As(err, &be) || be.Limit != budget.Limit() {
		t.Fatalf("want *DrainByteBudgetError{Limit:%d}, got %+v", budget.Limit(), err)
	}
	if drained >= histogramFixtureRows || drained == 0 {
		t.Errorf("drained %d rows before aborting, want 0 < n < %d", drained, histogramFixtureRows)
	}
}

// TestRowsCursor_FloatDrainUnchangedUnderMatrixBudget is the row-path half of
// the no-regression assertion.
func TestRowsCursor_FloatDrainUnchangedUnderMatrixBudget(t *testing.T) {
	t.Parallel()

	rows := make([]Sample, histogramFixtureRows)
	for i := range rows {
		rows[i] = fatSample(i, 32)
	}
	cur := &rowsCursor{
		rows:       &freshLabelRows{rows: rows},
		maxSamples: histogramFixtureMaxSamples,
		byteBudget: NewMatrixDrainBudget(histogramFixtureMaxSamples),
	}
	defer func() { _ = cur.Close() }()
	var drained int
	for cur.Next() {
		drained++
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("float drain tripped the matrix drain budget: %v", err)
	}
	if drained != histogramFixtureRows {
		t.Fatalf("drained %d float rows, want all %d", drained, histogramFixtureRows)
	}
}

func TestHistogramValueBytes(t *testing.T) {
	t.Parallel()

	if got := histogramValueBytes(nil); got != 0 {
		t.Fatalf("histogramValueBytes(nil) = %d, want 0", got)
	}
	// Both ladders count: charging only the positive one halves the estimate
	// for a histogram straddling zero.
	h := &HistogramValue{PositiveBucketCounts: make([]float64, 3), NegativeBucketCounts: make([]float64, 2)}
	if want := int64(histogramValueFixedBytes + 5*bucketCountBytes); histogramValueBytes(h) != want {
		t.Fatalf("histogramValueBytes = %d, want %d", histogramValueBytes(h), want)
	}
	// A bucket-less histogram still costs its struct: the fixed part is not
	// optional, or a million empty histograms charge nothing.
	if got := histogramValueBytes(&HistogramValue{}); got != histogramValueFixedBytes {
		t.Fatalf("histogramValueBytes(empty) = %d, want %d", got, histogramValueFixedBytes)
	}
}

func TestNewMatrixDrainBudget(t *testing.T) {
	t.Parallel()

	b := NewMatrixDrainBudget(1000)
	if b.Limit() != 1000*matrixDrainBytesPerSample {
		t.Fatalf("Limit = %d, want %d", b.Limit(), int64(1000*matrixDrainBytesPerSample))
	}
	// The -1 "sample budget deliberately disabled" sentinel (and 0) must leave
	// the byte budget inert rather than substituting an unasked-for bound.
	for _, maxSamples := range []int64{0, -1} {
		if got := NewMatrixDrainBudget(maxSamples); got.active() {
			t.Errorf("NewMatrixDrainBudget(%d) is active with limit %d, want inert", maxSamples, got.Limit())
		}
	}
	// A row cap large enough to overflow the multiplication clamps rather than
	// wrapping negative — a wrapped ceiling reads as inert, silently removing
	// the bound at exactly the setting that needs it most.
	if got := NewMatrixDrainBudget(math.MaxInt64).Limit(); got != math.MaxInt64 {
		t.Fatalf("overflowing row cap yielded limit %d, want %d", got, int64(math.MaxInt64))
	}
}
