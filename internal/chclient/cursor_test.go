package chclient

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeRows implements driver.Rows over an in-memory slice so the cursor
// can be exercised without a live ClickHouse. Only the methods the
// cursor actually calls (Next / Scan / Close / Err) carry real
// behaviour; the rest satisfy the interface as no-ops.
type fakeRows struct {
	samples []Sample
	idx     int
	scanErr error
	rowsErr error
	closed  bool
}

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.samples) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != 4 {
		return errors.New("fakeRows.Scan: want 4 destinations")
	}
	s := r.samples[r.idx-1]
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
	return nil
}

func (r *fakeRows) ScanStruct(any) error             { return errors.New("test mock: ScanStruct unused by cursor tests") }
func (r *fakeRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeRows) Totals(...any) error              { return nil }
func (r *fakeRows) Columns() []string                { return nil }
func (r *fakeRows) Err() error                       { return r.rowsErr }
func (r *fakeRows) HasData() bool                    { return len(r.samples) > 0 }

func (r *fakeRows) Close() error {
	r.closed = true
	return nil
}

func TestRowsCursor_StreamsSamples(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	want := []Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
		{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts.Add(time.Minute), Value: 0.0},
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
