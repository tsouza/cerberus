//go:build chdb

package chsql

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestDeltaMatrixFirstLevelIncludesHistoryBeforeQueryEnvelope(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'api'), toDateTime64('2025-12-31 23:58:30', 9), 50),
		(1, map('job', 'api'), toDateTime64('2025-12-31 23:59:30', 9), 1),
		(1, map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 10),
		(1, map('job', 'api'), toDateTime64('2026-01-01 00:00:30', 9), 40)`); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		Range:             time.Minute,
		Start:             start,
		End:               start.Add(30 * time.Second),
		Step:              30 * time.Second,
		OuterRange:        30 * time.Second,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sqlText, args, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT toUnixTimestamp64Milli(anchor_ts), Value FROM (" +
		strings.TrimSuffix(strings.TrimSpace(sqlText), ";") + ") ORDER BY anchor_ts"
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, sqlText)
	}
	defer func() { _ = rows.Close() }()

	want := []struct {
		ts    int64
		value float64
	}{
		{ts: start.UnixMilli(), value: 1.0 / 3.0},
		{ts: start.Add(30 * time.Second).UnixMilli(), value: 4.0 / 3.0},
	}
	var i int
	for rows.Next() {
		if i >= len(want) {
			t.Fatal("query returned more rows than expected")
		}
		var ts int64
		var value float64
		if err := rows.Scan(&ts, &value); err != nil {
			t.Fatal(err)
		}
		if ts != want[i].ts || math.Abs(value-want[i].value) > 1e-12 {
			t.Errorf("row %d = (%d, %.15g), want (%d, %.15g)",
				i, ts, value, want[i].ts, want[i].value)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(want) {
		t.Fatalf("query returned %d rows, want %d", i, len(want))
	}
}
