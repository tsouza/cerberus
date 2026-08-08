package chclient

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeRows implements driver.Rows over an in-memory slice so the cursor
// can be exercised without a live ClickHouse. Only the methods the
// cursor actually calls (Next / Scan / Close / Err / Columns) carry real
// behaviour; the rest satisfy the interface as no-ops.
type fakeRows struct {
	samples []Sample
	idx     int
	scanErr error
	rowsErr error
	closed  bool
	// columns, when non-nil, is returned by Columns() — the probe
	// rowsCursor.Next uses to decide the scan shape (hasMetadata /
	// hasHistogram). Tests exercising the base four-column path leave
	// it nil, matching fakeRows' pre-existing zero-value behaviour.
	columns []string
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.samples) {
		return false
	}
	r.idx++
	return true
}

// wantHistogramDest is the destination count rowsCursor.Next binds when
// the histogram probe latches true: the base four columns plus the nine
// Histogram*Column fields. histogramColumnCount is the production
// constant cursor.go derives its scan arity from, so a drift between
// the two fails this test file to compile rather than silently
// mismatching.
const wantHistogramDest = 4 + histogramColumnCount

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	s := r.samples[r.idx-1]
	switch len(dest) {
	case 4:
		if p, ok := dest[0].(*string); ok {
			*p = s.MetricName
		}
		if p, ok := dest[1].(*map[string]string); ok {
			*p = s.Labels
		}
		if p, ok := dest[2].(*time.Time); ok {
			*p = s.Timestamp
		}
		if p, ok := dest[3].(*float64); ok {
			*p = s.Value
		}
	case wantHistogramDest:
		if p, ok := dest[0].(*string); ok {
			*p = s.MetricName
		}
		if p, ok := dest[1].(*map[string]string); ok {
			*p = s.Labels
		}
		if p, ok := dest[2].(*time.Time); ok {
			*p = s.Timestamp
		}
		if p, ok := dest[3].(*float64); ok {
			*p = s.Value
		}
		hv := s.Histogram
		if hv == nil {
			return errors.New("fakeRows.Scan: histogram-shaped row requires a non-nil Sample.Histogram fixture")
		}
		if p, ok := dest[4].(*uint64); ok {
			*p = hv.Count
		}
		if p, ok := dest[5].(*float64); ok {
			*p = hv.Sum
		}
		if p, ok := dest[6].(*int32); ok {
			*p = hv.Scale
		}
		if p, ok := dest[7].(*float64); ok {
			*p = hv.ZeroThreshold
		}
		if p, ok := dest[8].(*uint64); ok {
			*p = hv.ZeroCount
		}
		if p, ok := dest[9].(*int32); ok {
			*p = hv.PositiveOffset
		}
		if p, ok := dest[10].(*[]uint64); ok {
			*p = hv.PositiveBucketCounts
		}
		if p, ok := dest[11].(*int32); ok {
			*p = hv.NegativeOffset
		}
		if p, ok := dest[12].(*[]uint64); ok {
			*p = hv.NegativeBucketCounts
		}
	default:
		return fmt.Errorf("fakeRows.Scan: want 4 or %d destinations, got %d", wantHistogramDest, len(dest))
	}
	return nil
}

func (r *fakeRows) ScanStruct(any) error {
	return errors.New("test mock: ScanStruct unused by cursor tests")
}
func (r *fakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeRows) Totals(...any) error              { return nil }
func (r *fakeRows) Columns() []string                { return r.columns }
func (r *fakeRows) Err() error                       { return r.rowsErr }
func (r *fakeRows) HasData() bool                    { return len(r.samples) > 0 }

func (r *fakeRows) Close() error {
	r.closed = true
	return nil
}

func TestRowsCursor_StreamsSamples(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	// SeriesID is the stable 1-based ordinal the cursor's interning assigns
	// each distinct decoded label set in first-seen order; fakeRows.Scan
	// ignores it on input, so it appears only on the cursor's output.
	want := []Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0, SeriesID: 1},
		{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts.Add(time.Minute), Value: 0.0, SeriesID: 2},
	}
	rows := &fakeRows{samples: want}
	cursor := &rowsCursor{rows: rows}

	var got []Sample
	for cursor.Next() {
		got = append(got, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("samples: got %+v, want %+v", got, want)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rows.closed {
		t.Fatal("underlying rows.Close was not invoked")
	}
	// Idempotent Close.
	if err := cursor.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRowsCursor_EmptyResultSet(t *testing.T) {
	t.Parallel()

	rows := &fakeRows{}
	cursor := &rowsCursor{rows: rows}
	if cursor.Next() {
		t.Fatal("Next on empty cursor should return false")
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err on empty: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("Close on empty: %v", err)
	}
}

func TestRowsCursor_ScanErrorTerminatesIteration(t *testing.T) {
	t.Parallel()

	rows := &fakeRows{
		samples: []Sample{{MetricName: "up", Value: 1}},
		scanErr: errors.New("decode boom"),
	}
	cursor := &rowsCursor{rows: rows}

	if cursor.Next() {
		t.Fatal("Next should return false when Scan fails")
	}
	if err := cursor.Err(); err == nil {
		t.Fatal("Err: want non-nil on scan failure")
	}
	// Subsequent Next must keep returning false (no infinite loop).
	if cursor.Next() {
		t.Fatal("Next must stay false after error")
	}
}

func TestRowsCursor_RowsErrSurfacesAfterEnd(t *testing.T) {
	t.Parallel()

	rows := &fakeRows{rowsErr: errors.New("transport boom")}
	cursor := &rowsCursor{rows: rows}

	if cursor.Next() {
		t.Fatal("Next should return false on empty + rowsErr")
	}
	if err := cursor.Err(); err == nil {
		t.Fatal("Err: want non-nil when rows.Err() is set")
	}
}

// histogramProjectionColumns is the exact nine-alias tail
// chplan.HistogramProjection's emitter produces (see
// internal/chplan/histogram_projection.go), prefixed with the base
// four column names. Duplicated here by value rather than imported —
// chclient may not import chplan (see .go-arch-lint.yml) — mirroring
// histogramLastColumn's own duplication in cursor.go.
var histogramProjectionColumns = []string{
	"MetricName", "Attributes", "TimeUnix", "Value",
	"HistogramCount", "HistogramSum", "HistogramScale",
	"HistogramZeroThreshold", "HistogramZeroCount",
	"HistogramPositiveOffset", "HistogramPositiveBucketCounts",
	"HistogramNegativeOffset", "HistogramNegativeBucketCounts",
}

// TestRowsCursor_DecodesHistogramColumns pins the decode-side half of
// issue #1926's shared prerequisite: a result set whose trailing column
// matches histogramLastColumn is scanned as a 13-destination histogram
// row, and every one of the nine HistogramValue fields round-trips
// through Sample.Histogram with a DISTINCT value per field, so no field
// is silently dropped, swapped, or aliased to a neighbour.
func TestRowsCursor_DecodesHistogramColumns(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	want := HistogramValue{
		Count:                42,
		Sum:                  1234.5,
		Scale:                3,
		ZeroThreshold:        0.001,
		ZeroCount:            7,
		PositiveOffset:       -2,
		PositiveBucketCounts: []uint64{1, 2, 3},
		NegativeOffset:       -5,
		NegativeBucketCounts: []uint64{4, 5},
	}
	rows := &fakeRows{
		columns: histogramProjectionColumns,
		samples: []Sample{{
			MetricName: "http_request_duration_seconds",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  ts,
			Value:      0, // meaningless placeholder on a histogram row
			Histogram:  &want,
		}},
	}
	cursor := &rowsCursor{rows: rows}

	if !cursor.Next() {
		t.Fatalf("Next: want true, cursor.Err() = %v", cursor.Err())
	}
	got := cursor.Sample()
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got.Histogram == nil {
		t.Fatalf("Sample.Histogram is nil; want a decoded HistogramValue")
	}
	if !reflect.DeepEqual(*got.Histogram, want) {
		t.Errorf("Sample.Histogram:\n got  %+v\n want %+v", *got.Histogram, want)
	}
	if got.MetricName != "http_request_duration_seconds" {
		t.Errorf("MetricName: got %q, want the base column to still decode", got.MetricName)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: got %v, want %v", got.Timestamp, ts)
	}
	if cursor.Next() {
		t.Fatal("Next should return false after the single row")
	}
}

// TestRowsCursor_HistogramProbeIsStableAcrossStream pins that the probe
// latches ONCE (on the first row) and stays latched for every later row,
// mirroring the existing metadataProbed contract: driver.Rows.Columns()
// is a result-set-wide property, not a per-row one, so re-probing every
// row would be wasted work at best and a torn decode at worst if a
// driver ever returned a transiently different slice.
func TestRowsCursor_HistogramProbeIsStableAcrossStream(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	h1 := HistogramValue{Count: 1, PositiveBucketCounts: []uint64{1}, NegativeBucketCounts: []uint64{}}
	h2 := HistogramValue{Count: 2, PositiveBucketCounts: []uint64{2}, NegativeBucketCounts: []uint64{}}
	rows := &fakeRows{
		columns: histogramProjectionColumns,
		samples: []Sample{
			{MetricName: "m", Timestamp: ts, Histogram: &h1},
			{MetricName: "m", Timestamp: ts.Add(time.Minute), Histogram: &h2},
		},
	}
	cursor := &rowsCursor{rows: rows}

	var got []Sample
	for cursor.Next() {
		got = append(got, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[0].Histogram == nil || got[0].Histogram.Count != 1 {
		t.Errorf("row 0 Histogram: got %+v, want Count=1", got[0].Histogram)
	}
	if got[1].Histogram == nil || got[1].Histogram.Count != 2 {
		t.Errorf("row 1 Histogram: got %+v, want Count=2", got[1].Histogram)
	}
}

// TestRowsCursor_BaseFourColumnPathUnaffected pins that a plain
// four-column result set (every query today) never latches
// hasHistogram, even though fakeRows now supports the wider scan shape
// — the probe checks the column NAME, not merely a wider Scan call
// being possible.
func TestRowsCursor_BaseFourColumnPathUnaffected(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rows := &fakeRows{
		samples: []Sample{{MetricName: "up", Timestamp: ts, Value: 1}},
	}
	cursor := &rowsCursor{rows: rows}

	if !cursor.Next() {
		t.Fatalf("Next: want true, cursor.Err() = %v", cursor.Err())
	}
	got := cursor.Sample()
	if got.Histogram != nil {
		t.Errorf("Sample.Histogram: got %+v, want nil on the base four-column path", got.Histogram)
	}
	if got.Value != 1 {
		t.Errorf("Value: got %v, want 1", got.Value)
	}
}
