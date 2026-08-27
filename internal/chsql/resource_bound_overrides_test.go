package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// resource_bound_overrides_test.go pins the operator-override wiring issue
// #2667 adds for the three chsql sample-fanout resource bounds
// (maxRangeBucketFanoutRows / maxRangeLWRFanoutRows,
// internal/chsql/lwr_fanout_bound.go; maxRateWindowFanoutRows,
// internal/chsql/rate_window_fanout_bound.go) at the SQL-emission level: an
// operator override threaded via ctx must change the LIMIT / throwIf
// comparison literal the emitter actually renders, not merely exist as dead
// plumbing. This is the fast, no-chDB-required half of the contract;
// resource_bound_overrides_chdb_test.go is the slower half — the SAME
// override actually changing whether a real ClickHouse query is admitted or
// rejected.

// resourceBoundFanoutPlan builds a minimal RangeBucketFanout over a bare
// Scan (bypassing PromQL lowering, matching this package's own emit-level
// test style — see range_bucket_fanout_mutation_test.go's fanoutMinSamplesPlan).
func resourceBoundFanoutPlan() *chplan.RangeBucketFanout {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeBucketFanout{
		Input:        &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:        start,
		End:          start.Add(5 * time.Minute),
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
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
}

// resourceBoundLWRPlan builds a minimal RangeLWR over a bare Scan, matching
// range_lwr_test.go's own plan shape.
func resourceBoundLWRPlan() *chplan.RangeLWR {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "otel_metrics_gauge"},
		Start:         start,
		End:           start.Add(5 * time.Minute),
		Step:          30 * time.Second,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// resourceBoundRateWindowPlan builds a minimal matrix-shaped (OuterRange >
// 0) RangeWindow rate() query — the shape emitWindowedArrayExtrapolatedMatrix
// governs — over a bare Scan.
func resourceBoundRateWindowPlan() *chplan.RangeWindow {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Start:           end.Add(-10 * time.Minute),
		End:             end,
		Step:            time.Minute,
		OuterRange:      10 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// TestWithRangeBucketFanoutMaxRows_OverridesEmittedLimit confirms
// chsql.WithRangeBucketFanoutMaxRows actually changes the LIMIT literal
// lwrFanoutBoundedSourceFrag renders for RangeBucketFanout's fan-out — the
// default (maxRangeBucketFanoutRows = 4,000,000) without an override, and
// the operator's own value (here, an arbitrary 12,345) once threaded via
// ctx.
func TestWithRangeBucketFanoutMaxRows_OverridesEmittedLimit(t *testing.T) {
	t.Parallel()

	plan := resourceBoundFanoutPlan()

	sqlDefault, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit (default): %v", err)
	}
	if got := strings.Count(sqlDefault, "LIMIT 4000001"); got != 2 {
		t.Errorf("default emit: expected \"LIMIT 4000001\" exactly twice, got %d\nSQL:\n%s", got, sqlDefault)
	}

	ctx := chsql.WithRangeBucketFanoutMaxRows(context.Background(), 12345)
	sqlOverridden, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit (overridden): %v", err)
	}
	if got := strings.Count(sqlOverridden, "LIMIT 12346"); got != 2 {
		t.Errorf("overridden emit: expected \"LIMIT 12346\" exactly twice, got %d\nSQL:\n%s", got, sqlOverridden)
	}
	if strings.Contains(sqlOverridden, "LIMIT 4000001") {
		t.Errorf("overridden emit must NOT still carry the default's LIMIT literal\nSQL:\n%s", sqlOverridden)
	}
}

// TestWithRangeLWRFanoutMaxRows_OverridesEmittedLimit is
// TestWithRangeBucketFanoutMaxRows_OverridesEmittedLimit's RangeLWR sibling
// — default maxRangeLWRFanoutRows = 40,000,000, overridden to 98765.
func TestWithRangeLWRFanoutMaxRows_OverridesEmittedLimit(t *testing.T) {
	t.Parallel()

	plan := resourceBoundLWRPlan()

	sqlDefault, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit (default): %v", err)
	}
	if got := strings.Count(sqlDefault, "LIMIT 40000001"); got != 2 {
		t.Errorf("default emit: expected \"LIMIT 40000001\" exactly twice, got %d\nSQL:\n%s", got, sqlDefault)
	}

	ctx := chsql.WithRangeLWRFanoutMaxRows(context.Background(), 98765)
	sqlOverridden, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit (overridden): %v", err)
	}
	if got := strings.Count(sqlOverridden, "LIMIT 98766"); got != 2 {
		t.Errorf("overridden emit: expected \"LIMIT 98766\" exactly twice, got %d\nSQL:\n%s", got, sqlOverridden)
	}
	if strings.Contains(sqlOverridden, "LIMIT 40000001") {
		t.Errorf("overridden emit must NOT still carry the default's LIMIT literal\nSQL:\n%s", sqlOverridden)
	}
}

// TestWithRateWindowFanoutMaxRows_OverridesEmittedLimit is the
// emitWindowedArrayExtrapolatedMatrix sibling — default
// maxRateWindowFanoutRows = 2,800,000, overridden to 54321.
func TestWithRateWindowFanoutMaxRows_OverridesEmittedLimit(t *testing.T) {
	t.Parallel()

	plan := resourceBoundRateWindowPlan()

	sqlDefault, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit (default): %v", err)
	}
	if got := strings.Count(sqlDefault, "LIMIT 2800001"); got != 2 {
		t.Errorf("default emit: expected \"LIMIT 2800001\" exactly twice, got %d\nSQL:\n%s", got, sqlDefault)
	}

	ctx := chsql.WithRateWindowFanoutMaxRows(context.Background(), 54321)
	sqlOverridden, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit (overridden): %v", err)
	}
	if got := strings.Count(sqlOverridden, "LIMIT 54322"); got != 2 {
		t.Errorf("overridden emit: expected \"LIMIT 54322\" exactly twice, got %d\nSQL:\n%s", got, sqlOverridden)
	}
	if strings.Contains(sqlOverridden, "LIMIT 2800001") {
		t.Errorf("overridden emit must NOT still carry the default's LIMIT literal\nSQL:\n%s", sqlOverridden)
	}
}
