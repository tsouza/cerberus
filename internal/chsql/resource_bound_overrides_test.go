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
		Input:        closedTimestampTestScan("otel_metrics_gauge", "TimeUnix", "Attributes", "Value"),
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

// resourceBoundFanoutGroupPlan is resourceBoundFanoutPlan's growing-
// accumulator sibling: the SAME shape, but with a groupArray AggFunc
// (the collapse shape classicBucketWindowAggs / expHistogramWindowAggs
// both build — see maxRangeBucketFanoutGroupRows' own doc, issue #3468) in
// place of the fixed-size argMax the pre-collapse-only sibling plan uses.
// This is what actually reaches emitRangeBucketFanout's new
// rangeBucketFanoutHasGrowingAccumulator branch; resourceBoundFanoutPlan
// deliberately does NOT (its argMax-only AggFuncs is the discriminating
// control TestWithRangeBucketFanoutMaxRows_OverridesEmittedLimit already
// pins: exactly two LIMIT literals, never four).
func resourceBoundFanoutGroupPlan() *chplan.RangeBucketFanout {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeBucketFanout{
		Input:        closedTimestampTestScan("otel_metrics_gauge", "TimeUnix", "Attributes", "Value"),
		Start:        start,
		End:          start.Add(5 * time.Minute),
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		AggFuncs: []chplan.AggFunc{
			{
				Fn:    chplan.FnGroupArray,
				Alias: "Values",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
			},
		},
	}
}

// resourceBoundLWRPlan builds a minimal RangeLWR over a bare Scan, matching
// range_lwr_test.go's own plan shape.
func resourceBoundLWRPlan() *chplan.RangeLWR {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeLWR{
		Input:         rangeLWRTestInput("otel_metrics_gauge"),
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
		Input:           closedRangeWindowTestScan("otel_metrics_sum", "TimeUnix", "Attributes", "Value", "AggregationTemporality"),
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

// TestWithRangeBucketFanoutMaxRows_GrowingAccumulatorCarriesBothGuards
// confirms a groupArray-accumulating RangeBucketFanout collapse (issue
// #3468) carries BOTH the pre-collapse sample-fanout guard
// (rangeBucketFanoutRowBound, unchanged) AND the new post-collapse
// group-count guard (rangeBucketFanoutGroupRowBound) — four LIMIT
// literals total, not two — and that
// RangeBucketFanoutGroupBudgetMessage, not RangeBucketFanoutBudgetMessage,
// is the one the new guard's throwIf carries.
func TestWithRangeBucketFanoutMaxRows_GrowingAccumulatorCarriesBothGuards(t *testing.T) {
	t.Parallel()

	plan := resourceBoundFanoutGroupPlan()

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// FOUR, not two: lwrFanoutBoundedSourceFrag reads its source TWICE
	// (bounded + the independent truncation probe), and the group guard's
	// source IS the collapse — which itself already embeds the
	// pre-collapse guard's own two reads once. Doubling collapse doubles
	// everything inside it, exactly as it doubles the raw scan beneath the
	// sample fanout for every RangeBucketFanout/RangeLWR this file's own
	// design already accepts that cost for.
	if got := strings.Count(sql, "LIMIT 4000001"); got != 4 {
		t.Errorf("pre-collapse fanout guard: expected \"LIMIT 4000001\" exactly four times (twice per group-guard read), got %d\nSQL:\n%s", got, sql)
	}
	if got := strings.Count(sql, "LIMIT 801"); got != 2 {
		t.Errorf("post-collapse group guard: expected \"LIMIT 801\" exactly twice, got %d\nSQL:\n%s", got, sql)
	}
	if !strings.Contains(sql, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("emitted SQL missing RangeBucketFanoutGroupBudgetMessage\nSQL:\n%s", sql)
	}
	if !strings.Contains(sql, chsql.RangeBucketFanoutBudgetMessage) {
		t.Errorf("emitted SQL missing RangeBucketFanoutBudgetMessage (the pre-collapse guard should still be present)\nSQL:\n%s", sql)
	}
}

// TestWithRangeBucketFanoutGroupMaxRows_OverridesEmittedLimit is
// TestWithRangeBucketFanoutMaxRows_OverridesEmittedLimit's post-collapse
// sibling — default maxRangeBucketFanoutGroupRows = 800, overridden to
// 999.
func TestWithRangeBucketFanoutGroupMaxRows_OverridesEmittedLimit(t *testing.T) {
	t.Parallel()

	plan := resourceBoundFanoutGroupPlan()

	sqlDefault, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit (default): %v", err)
	}
	if got := strings.Count(sqlDefault, "LIMIT 801"); got != 2 {
		t.Errorf("default emit: expected \"LIMIT 801\" exactly twice, got %d\nSQL:\n%s", got, sqlDefault)
	}

	ctx := chsql.WithRangeBucketFanoutGroupMaxRows(context.Background(), 999)
	sqlOverridden, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit (overridden): %v", err)
	}
	if got := strings.Count(sqlOverridden, "LIMIT 1000"); got != 2 {
		t.Errorf("overridden emit: expected \"LIMIT 1000\" exactly twice, got %d\nSQL:\n%s", got, sqlOverridden)
	}
	if strings.Contains(sqlOverridden, "LIMIT 801") {
		t.Errorf("overridden emit must NOT still carry the default's LIMIT literal\nSQL:\n%s", sqlOverridden)
	}
	// The pre-collapse fanout guard is untouched by this override — only
	// the post-collapse group guard's own literal should move. Four, not
	// two — see TestWithRangeBucketFanoutMaxRows_GrowingAccumulatorCarriesBothGuards's
	// own comment for why.
	if got := strings.Count(sqlOverridden, "LIMIT 4000001"); got != 4 {
		t.Errorf("overridden emit: pre-collapse fanout guard changed unexpectedly, expected \"LIMIT 4000001\" four times, got %d\nSQL:\n%s", got, sqlOverridden)
	}
}

// TestRangeBucketFanoutGroupGuard_FixedSizeAccumulatorUnaffected is the
// discriminating control: resourceBoundFanoutPlan's argMax-only AggFuncs
// must never carry the new group guard, at default OR override — an
// unconditional application would false-positive-reject a cheap, safe,
// wide-range dashboard query the new guard was never calibrated to police
// (see rangeBucketFanoutHasGrowingAccumulator's own doc).
func TestRangeBucketFanoutGroupGuard_FixedSizeAccumulatorUnaffected(t *testing.T) {
	t.Parallel()

	plan := resourceBoundFanoutPlan()
	ctx := chsql.WithRangeBucketFanoutGroupMaxRows(context.Background(), 1)
	sql, _, err := chsql.Emit(ctx, plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("a fixed-size-accumulator (argMax) collapse must never carry the group guard, even overridden to 1\nSQL:\n%s", sql)
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
