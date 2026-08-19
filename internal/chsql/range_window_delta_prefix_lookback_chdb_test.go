//go:build chdb

package chsql

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// instantDeltaPrefixWindow builds the minimal instant (OuterRange == 0)
// rate() RangeWindow instantDeltaPrefixSource governs, grouped by the whole
// series-identity Attributes map so multiple `job` values in one table
// surface as separate output rows.
func instantDeltaPrefixWindow(end time.Time, rng time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		Range:             rng,
		End:               end,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// runInstantDeltaPrefixQuery lowers r to SQL under the given lookback
// (0 == disabled / unbounded), wraps it to expose the `job` label as a plain
// string column (sidestepping Map-column scan support), and returns
// job -> rate() value for every row.
func runInstantDeltaPrefixQuery(t *testing.T, db *sql.DB, r *chplan.RangeWindow, lookback time.Duration) map[string]float64 {
	t.Helper()
	sqlText, args, err := Emit(WithDeltaPrefixLookback(context.Background(), lookback), r)
	if err != nil {
		t.Fatalf("Emit(lookback=%s): %v", lookback, err)
	}
	inner := strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	wrapped := "SELECT Attributes['job'] AS job, Value FROM (" + inner + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query(lookback=%s): %v\n%s", lookback, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]float64{}
	for rows.Next() {
		var job string
		var value float64
		if err := rows.Scan(&job, &value); err != nil {
			t.Fatalf("scan(lookback=%s): %v", lookback, err)
		}
		out[job] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows(lookback=%s): %v", lookback, err)
	}
	return out
}

// TestDeltaPrefixLookback_CumulativeSeriesUnaffected proves the lookback
// bound changes ZERO output for a CUMULATIVE-temporality series sharing a
// table with genuine DELTA data — deltaFirstValFrag never consults
// delta_prefix_before_window for a Cumulative series (see its doc), so
// whatever the (disabled vs bounded) prefix scan computes for OTHER series
// must not leak into this one's result.
func TestDeltaPrefixLookback_CumulativeSeriesUnaffected(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`); err != nil {
		t.Fatal(err)
	}
	// A CUMULATIVE series (temporality=2) with its own history reaching back
	// well past ANY reasonable lookback, interleaved with an UNRELATED DELTA
	// series (temporality=1, a different `job` label) so a bug that leaked
	// cross-series would be caught.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(2, map('job', 'cumulative'), toDateTime64('2025-01-01 00:00:00', 9), 100),
		(2, map('job', 'cumulative'), toDateTime64('2025-12-31 23:59:30', 9), 500),
		(2, map('job', 'cumulative'), toDateTime64('2026-01-01 00:00:00', 9), 510),
		(1, map('job', 'delta'), toDateTime64('2020-01-01 00:00:00', 9), 9999),
		(1, map('job', 'delta'), toDateTime64('2025-12-31 23:59:30', 9), 5),
		(1, map('job', 'delta'), toDateTime64('2026-01-01 00:00:00', 9), 7)`); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := instantDeltaPrefixWindow(start, time.Minute)

	unbounded := runInstantDeltaPrefixQuery(t, db, r, 0)
	bounded := runInstantDeltaPrefixQuery(t, db, r, time.Minute)

	cumUnbounded, ok := unbounded["cumulative"]
	if !ok {
		t.Fatal("cumulative series (unbounded): no row")
	}
	cumBounded, ok := bounded["cumulative"]
	if !ok {
		t.Fatal("cumulative series (1m lookback): no row")
	}
	if math.Abs(cumUnbounded-cumBounded) > 1e-12 {
		t.Errorf("cumulative series value changed with a 1m lookback: unbounded=%v bounded=%v", cumUnbounded, cumBounded)
	}
}

// TestDeltaPrefixLookback_DeltaSeries_WithinVsBeyondBound proves the DELTA
// reconstruction itself behaves exactly as documented: a lookback wide
// enough to cover the series' true prior contribution reproduces the
// unbounded answer bit-for-bit, and a lookback narrower than that prior
// contribution's age produces a DIFFERENT (documented, non-crashing)
// approximation rather than silently matching the unbounded value by
// accident.
func TestDeltaPrefixLookback_DeltaSeries_WithinVsBeyondBound(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`); err != nil {
		t.Fatal(err)
	}
	// A DELTA series (temporality=1) whose only pre-window contribution
	// (value 1000) sits 10 minutes before the window — inside a 20m lookback,
	// outside a 2m one. Two more DELTA samples land inside the 1m rate()
	// window itself so counter_delta is well-defined regardless of the
	// prefix.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'api'), toDateTime64('2025-12-31 23:50:00', 9), 1000),
		(1, map('job', 'api'), toDateTime64('2025-12-31 23:59:30', 9), 1),
		(1, map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 10)`); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := instantDeltaPrefixWindow(start, time.Minute)

	unbounded := runInstantDeltaPrefixQuery(t, db, r, 0)["api"]
	within := runInstantDeltaPrefixQuery(t, db, r, 20*time.Minute)["api"]
	beyond := runInstantDeltaPrefixQuery(t, db, r, 2*time.Minute)["api"]

	if math.Abs(unbounded-within) > 1e-12 {
		t.Errorf("a lookback covering the series' true prior contribution must match unbounded exactly: unbounded=%v within(20m)=%v", unbounded, within)
	}
	if math.Abs(unbounded-beyond) < 1e-12 {
		t.Errorf("a lookback narrower than the series' true prior contribution must NOT silently match unbounded (the whole point of this test is that they differ): unbounded=%v beyond(2m)=%v", unbounded, beyond)
	}
	// Both forms must still produce a finite, sane rate() value — the
	// approximation must never crash or emit NaN/Inf for a case this benign
	// (2+ in-window samples).
	if math.IsNaN(beyond) || math.IsInf(beyond, 0) {
		t.Errorf("bounded (2m) lookback produced a non-finite value: %v", beyond)
	}
}
