//go:build chdb

// resource_bound_overrides_chdb_test.go is the behavioural half of issue
// #2667's test contract for the three chsql sample-fanout resource bounds
// (resource_bound_overrides_test.go pins the SQL-emission half): a real
// ClickHouse query that would NOT trip the compiled-in default now gets
// REJECTED once an operator override lowers the bound via ctx — proving the
// override changes actual query behaviour, not just an emitted literal.
//
// Every seed below places every raw sample at the SAME timestamp
// (resourceBoundOverrideStart), deliberately as close to the grid's own
// Start as the plan allows: the LWR/bucket-fanout staleness window looks
// FORWARD from a sample to the anchors it covers (ts <= anchor <=
// ts+Lookback), so a sample sitting at the earliest possible timestamp
// covers the WIDEST run of anchors the grid offers — the shape that
// maximises fanned-row count for the smallest possible seed, keeping these
// tests fast.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

var resourceBoundOverrideStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// resourceBoundOverrideSeedRows is the number of identical-timestamp raw
// samples every test below seeds. Each fans out to close to
// resourceBoundOverrideAnchors anchors (see the file doc comment), so the
// true fanned-row count is comfortably north of
// resourceBoundOverrideSmallMaxRows (the override every "rejects" test
// below uses) while staying comfortably under every constant's own
// compiled-in default (the smallest of the three, maxRateWindowFanoutRows,
// is 2,800,000) — so the SAME seed passes cleanly under the default and
// trips cleanly under the override.
const resourceBoundOverrideSeedRows = 20

// resourceBoundOverrideAnchors is both the anchor-grid width (Step=1m over
// a (resourceBoundOverrideAnchors-1)-minute window) and, not coincidentally,
// close to the Lookback/Step ratio (30m/1m): every seeded sample sits at
// the grid's own Start, so its covered-anchor count is bounded only by the
// grid width itself, not by Lookback.
const resourceBoundOverrideAnchors = 30

// resourceBoundOverrideSmallMaxRows is deliberately far below
// resourceBoundOverrideSeedRows*resourceBoundOverrideAnchors (≈600) but
// far above zero, so a passing "rejects" test genuinely exercises the
// truncation-probe LIMIT rather than an edge case at 0 or 1.
const resourceBoundOverrideSmallMaxRows = 100

// countPassingRows wraps sqlStr in an outer `SELECT count() FROM (...)` and
// reads back a single scalar — rather than iterating db.Query(sqlStr)'s own
// rows.Next()/Scan directly — because every plan below projects Attributes
// (Map(String,String)); the chdb-go driver's row scan cannot decode a Map
// column when iterating with rows.Next()/rows.Err() alone (the same
// limitation range_bucket_grid_native_bound_test.go's own doc documents and
// works around by avoiding a Map column in ITS plan entirely — not an
// option here, since RangeLWR/RangeBucketFanout/RangeWindow's GroupBy
// always projects the real Attributes column). Wrapping in a count()
// avoids decoding the Map at all: only a UInt64 scalar crosses the driver.
func countPassingRows(t *testing.T, db *sql.DB, sqlStr string, args []any) int {
	t.Helper()
	wrapped := "SELECT count() AS n FROM (" + sqlStr + ")"
	var n int
	if err := db.QueryRow(wrapped, args...).Scan(&n); err != nil {
		t.Fatalf("default-bound query unexpectedly failed: %v", err)
	}
	return n
}

// resourceBoundOverrideGrid returns the shared (Start, End, Step, Lookback)
// every plan below uses: a resourceBoundOverrideAnchors-wide, 1-minute-step
// grid with a Lookback equal to the grid's own full span, so a sample at
// Start covers every anchor.
func resourceBoundOverrideGrid() (start, end time.Time, step, lookback time.Duration) {
	step = time.Minute
	start = resourceBoundOverrideStart
	end = start.Add(time.Duration(resourceBoundOverrideAnchors-1) * step)
	lookback = end.Sub(start) + step
	return start, end, step, lookback
}

// insertResourceBoundOverrideSeed inserts resourceBoundOverrideSeedRows
// identical-series rows into table (MetricsSeedDDL's own
// MetricName/Attributes/TimeUnix/Value shape), split across TWO timestamps
// 30 seconds apart — both still close enough to resourceBoundOverrideStart
// to fan out across nearly every anchor (see the file doc comment) — rather
// than one: rate()'s own extrapolation (TestWithRateWindowFanoutMaxRows_ChangesQueryOutcome's
// plan) needs at least two DISTINCT-timestamp samples in a window to
// compute a counter delta at all; a single repeated timestamp resolves to
// no output rows regardless of the resource bound, which would make that
// test's own "passes under the default" half fail for an unrelated reason.
func insertResourceBoundOverrideSeed(t *testing.T, exec func(string) error, table string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO %s (MetricName, Attributes, TimeUnix, Value) VALUES ", table)
	for i := 0; i < resourceBoundOverrideSeedRows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		ts := resourceBoundOverrideStart
		if i%2 == 1 {
			ts = ts.Add(30 * time.Second)
		}
		fmt.Fprintf(&b, "('m', map('series','a'), toDateTime64('%s', 9), %d)",
			ts.Format("2006-01-02 15:04:05.000000000"), i)
	}
	if err := exec(b.String()); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// TestWithRangeBucketFanoutMaxRows_ChangesQueryOutcome proves the override
// is not just plumbed through to the emitted SQL (resource_bound_overrides_test.go)
// but actually changes whether a real ClickHouse query is admitted: the
// SAME seed and plan pass cleanly under the default (4,000,000) and get
// cleanly rejected once chsql.WithRangeBucketFanoutMaxRows lowers the bound
// below the seed's own true fanned-row count.
func TestWithRangeBucketFanoutMaxRows_ChangesQueryOutcome(t *testing.T) {
	const table = "range_bucket_fanout_override_metrics"
	start, end, step, lookback := resourceBoundOverrideGrid()
	plan := &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: table},
		Start:        start,
		End:          end,
		Step:         step,
		Lookback:     lookback,
		GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		AggFuncs: []chplan.AggFunc{
			{
				Fn:    chplan.FnArgMax,
				Alias: "Value",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "Value"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		},
	}

	t.Run("passes under the default", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		n := countPassingRows(t, db, sqlStr, args)
		if n == 0 {
			t.Fatal("expected at least one row under the default bound")
		}
	})

	t.Run("rejects once overridden below the true fanned-row count", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		ctx := chsql.WithRangeBucketFanoutMaxRows(context.Background(), resourceBoundOverrideSmallMaxRows)
		sqlStr, args, err := chsql.Emit(ctx, plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		_, err = db.Query(sqlStr, args...)
		if err == nil {
			t.Fatal("expected the overridden bound's throwIf to fire, got no error")
		}
		if !strings.Contains(err.Error(), chsql.RangeBucketFanoutBudgetMessage) {
			t.Errorf("query failed, but not with the expected resource-bound message %q: %v",
				chsql.RangeBucketFanoutBudgetMessage, err)
		}
	})
}

// TestWithRangeLWRFanoutMaxRows_ChangesQueryOutcome is
// TestWithRangeBucketFanoutMaxRows_ChangesQueryOutcome's RangeLWR sibling —
// default maxRangeLWRFanoutRows = 40,000,000.
func TestWithRangeLWRFanoutMaxRows_ChangesQueryOutcome(t *testing.T) {
	const table = "range_lwr_override_metrics"
	start, end, step, lookback := resourceBoundOverrideGrid()
	plan := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: table},
		Start:         start,
		End:           end,
		Step:          step,
		Lookback:      lookback,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}

	t.Run("passes under the default", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		n := countPassingRows(t, db, sqlStr, args)
		if n == 0 {
			t.Fatal("expected at least one row under the default bound")
		}
	})

	t.Run("rejects once overridden below the true fanned-row count", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		ctx := chsql.WithRangeLWRFanoutMaxRows(context.Background(), resourceBoundOverrideSmallMaxRows)
		sqlStr, args, err := chsql.Emit(ctx, plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		_, err = db.Query(sqlStr, args...)
		if err == nil {
			t.Fatal("expected the overridden bound's throwIf to fire, got no error")
		}
		if !strings.Contains(err.Error(), chsql.RangeLWRFanoutBudgetMessage) {
			t.Errorf("query failed, but not with the expected resource-bound message %q: %v",
				chsql.RangeLWRFanoutBudgetMessage, err)
		}
	})
}

// TestWithRateWindowFanoutMaxRows_ChangesQueryOutcome is
// TestWithRangeBucketFanoutMaxRows_ChangesQueryOutcome's
// emitWindowedArrayExtrapolatedMatrix sibling — default
// maxRateWindowFanoutRows = 2,800,000.
func TestWithRateWindowFanoutMaxRows_ChangesQueryOutcome(t *testing.T) {
	const table = "rate_window_override_metrics"
	start, end, step, lookback := resourceBoundOverrideGrid()
	_ = lookback // rate()'s window is chplan.RangeWindow.Range, not Lookback.
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: table},
		Func:            "rate",
		Range:           time.Duration(resourceBoundOverrideAnchors) * step,
		Start:           start,
		End:             end,
		Step:            step,
		OuterRange:      time.Duration(resourceBoundOverrideAnchors-1) * step,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	t.Run("passes under the default", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		n := countPassingRows(t, db, sqlStr, args)
		if n == 0 {
			t.Fatal("expected at least one row under the default bound")
		}
	})

	t.Run("rejects once overridden below the true fanned-row count", func(t *testing.T) {
		db := chsqltest.OpenIsolatedChDB(t)
		if _, err := db.Exec(chsqltest.MetricsSeedDDL(table)); err != nil {
			t.Fatalf("ddl: %v", err)
		}
		insertResourceBoundOverrideSeed(t, func(s string) error { _, err := db.Exec(s); return err }, table)

		ctx := chsql.WithRateWindowFanoutMaxRows(context.Background(), resourceBoundOverrideSmallMaxRows)
		sqlStr, args, err := chsql.Emit(ctx, plan)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		_, err = db.Query(sqlStr, args...)
		if err == nil {
			t.Fatal("expected the overridden bound's throwIf to fire, got no error")
		}
		if !strings.Contains(err.Error(), chsql.RateWindowFanoutBudgetMessage) {
			t.Errorf("query failed, but not with the expected resource-bound message %q: %v",
				chsql.RateWindowFanoutBudgetMessage, err)
		}
	})
}
