//go:build chdb

package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestDeltaPrefixAnchorArrayIsLazyForCumulativeRows(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET max_threads = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		TimeUnix DateTime64(9)
	) ENGINE = MergeTree ORDER BY TimeUnix`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, toDateTime64('2026-01-01 00:00:00',9)),
		(1, toDateTime64('2026-01-01 00:04:00',9)),
		(2, toDateTime64('2026-01-01 00:04:00',9))`); err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC)
	endFrag := func(b *Builder) { b.DateTime64Lit(end) }
	prefixSQL := renderFragToSQL(deltaPrefixAnchorArrayFrag(
		endFrag, Col("TimeUnix"), Col("AggregationTemporality"),
		int64(time.Minute), int64(5*time.Minute), 6,
	))
	rows, err := db.Query("SELECT AggregationTemporality, length(" + prefixSQL + ") FROM otel_metrics_sum ORDER BY AggregationTemporality, TimeUnix")
	if err != nil {
		t.Fatalf("prefix array: %v\n%s", err, prefixSQL)
	}
	defer func() { _ = rows.Close() }()
	var deltaEvents, cumulativeEvents int
	for rows.Next() {
		var temporality, n int
		if err := rows.Scan(&temporality, &n); err != nil {
			t.Fatal(err)
		}
		if temporality == 1 {
			deltaEvents += n
		} else {
			cumulativeEvents += n
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if deltaEvents != 1 || cumulativeEvents != 0 {
		t.Fatalf("prefix events: DELTA=%d cumulative=%d, want 1/0", deltaEvents, cumulativeEvents)
	}
}

func TestDeltaPrefixBucketsExecute(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET max_threads = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum
		(AggregationTemporality, Attributes, TimeUnix, Value) VALUES
		(1, map('job','api'), toDateTime64('2026-01-01 00:00:00',9), 2),
		(1, map('job','api'), toDateTime64('2026-01-01 00:01:00',9), 3),
		(1, map('job','api'), toDateTime64('2026-01-01 00:02:00',9), 5),
		(1, map('job','api'), toDateTime64('2026-01-01 00:03:00',9), 7),
		(1, map('job','api'), toDateTime64('2026-01-01 00:04:00',9), 11),
		(1, map('job','api'), toDateTime64('2026-01-01 00:05:00',9), 13),
		(1, map('job','api'), toDateTime64('2026-01-01 00:06:00',9), 17),
		(2, map('job','cumulative'), toDateTime64('2026-01-01 00:06:00',9), 19)`); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	endFrag := func(b *Builder) { b.DateTime64Lit(end) }
	prefixSQL := renderFragToSQL(deltaPrefixAnchorArrayFrag(
		endFrag, Col("TimeUnix"), Col("AggregationTemporality"),
		int64(time.Minute), int64(5*time.Minute), 6,
	))
	rows, err := db.Query("SELECT AggregationTemporality, length(" + prefixSQL + ") FROM otel_metrics_sum ORDER BY AggregationTemporality, TimeUnix")
	if err != nil {
		t.Fatalf("prefix array: %v\n%s", err, prefixSQL)
	}
	var deltaEvents, cumulativeEvents int
	for rows.Next() {
		var temporality, n int
		if err := rows.Scan(&temporality, &n); err != nil {
			t.Fatal(err)
		}
		if temporality == 1 {
			deltaEvents += n
		} else {
			cumulativeEvents += n
		}
	}
	_ = rows.Close()
	if cumulativeEvents != 0 {
		t.Fatalf("cumulative rows emitted %d DELTA prefix events", cumulativeEvents)
	}
	t.Logf("prefix array isolated: DELTA events=%d cumulative events=%d", deltaEvents, cumulativeEvents)

	r := &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		Range:             5 * time.Minute,
		Start:             start,
		End:               end,
		Step:              time.Minute,
		OuterRange:        5 * time.Minute,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sqlText, args, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	countSQL := "SELECT count() FROM (" + strings.TrimSuffix(strings.TrimSpace(sqlText), ";") + ")"
	var count int
	if err := db.QueryRow(countSQL, args...).Scan(&count); err != nil {
		t.Fatalf("query: %v\n%s", err, sqlText)
	}
	if count == 0 {
		t.Fatal("no rows")
	}
}
