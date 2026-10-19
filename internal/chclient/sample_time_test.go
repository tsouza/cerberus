package chclient

import (
	"testing"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
)

// beyondStorage is an instant past the last one a DateTime64(9) holds,
// which a query reports as Int64 Unix milliseconds.
var beyondStorage = time.Date(2286, 11, 20, 17, 46, 40, 0, time.UTC)

// TestSampleTime_Scan pins the row path's TimeUnix scan: a DateTime64
// value as it is, Int64 Unix milliseconds as their instant, and any other
// type refused.
func TestSampleTime_Scan(t *testing.T) {
	var got time.Time
	at := time.Date(2026, 1, 1, 0, 0, 1, 500, time.UTC)
	if err := (*SampleTime)(&got).Scan(at); err != nil || !got.Equal(at) {
		t.Fatalf("Scan(time.Time) = %v, %v; want %v", got, err, at)
	}
	if err := (*SampleTime)(&got).Scan(beyondStorage.UnixMilli()); err != nil || !got.Equal(beyondStorage) {
		t.Fatalf("Scan(int64 ms) = %v, %v; want %v", got, err, beyondStorage)
	}
	if err := (*SampleTime)(&got).Scan("2026-01-01"); err == nil {
		t.Fatal("Scan(string): want an error")
	}
}

// TestSampleTimeColumn_BindsMillis pins the columnar path's TimeUnix
// binding: an Int64 column reads as Unix milliseconds, a DateTime64
// column as its instants, and any other column is declined.
func TestSampleTimeColumn_BindsMillis(t *testing.T) {
	ms := &chproto.ColInt64{beyondStorage.UnixMilli()}
	col, ok := sampleTimeColumn(ms)
	if !ok || !col.Row(0).Equal(beyondStorage) {
		t.Fatalf("Int64 column: ok %v; want %v", ok, beyondStorage)
	}
	dt := (&chproto.ColDateTime64{}).WithPrecision(chproto.PrecisionNano)
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	dt.Append(at)
	col, ok = sampleTimeColumn(dt)
	if !ok || !col.Row(0).Equal(at) {
		t.Fatalf("DateTime64 column: ok %v; want %v", ok, at)
	}
	if _, ok := sampleTimeColumn(&chproto.ColFloat64{}); ok {
		t.Fatal("Float64 column: want declined")
	}
}
