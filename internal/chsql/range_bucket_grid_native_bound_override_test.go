package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// overrideTestPlan builds a minimal RangeBucketGridNative — the same bare
// emit-level shape buildGridNativePlan (range_bucket_grid_native_bound_test.go)
// uses, kept separate here since this file's own tests are deliberately
// NOT chDB-gated (they only assert about the emitted SQL TEXT, never
// execute it — issue #2665's own operator-override mechanism is pure
// plan-time ctx threading, needing no real ClickHouse to verify).
func overrideTestPlan() *chplan.RangeBucketGridNative {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeBucketGridNative{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{"SeriesID", "TimeUnix", "BucketCounts", "ExplicitBounds"},
		},
		Start:             start,
		End:               start.Add(9 * time.Minute),
		Step:              time.Minute,
		Range:             5 * time.Minute,
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "SeriesID"}},
		GroupByAliases:    []string{"SeriesID"},
		AnchorAlias:       "anchor_ts",
		TimestampCol:      "TimeUnix",
		BucketCountsCol:   "BucketCounts",
		ExplicitBoundsCol: "ExplicitBounds",
	}
}

// TestRangeBucketGridNativeBound_MaxRowsOverride_ChangesEmittedThreshold is
// issue #2665's own regression pin for the operator-override mechanism
// itself: chsql.WithRangeBucketGridNativeMaxRows must change the literal
// axis1 threshold the emitted throwIf guard compares against, and the
// unset path must keep emitting the package's own real-evidence-calibrated
// default (25,000,000, unchanged since issue #2651/#2653) — proving the
// ctx-threading mechanism this file's "Operator override" doc describes
// (range_bucket_grid_native_bound.go) actually reaches the emitted SQL,
// not just compiles.
func TestRangeBucketGridNativeBound_MaxRowsOverride_ChangesEmittedThreshold(t *testing.T) {
	node := overrideTestPlan()

	defaultSQL, _, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("emit (default): %v", err)
	}
	if !strings.Contains(defaultSQL, "25000000") {
		t.Errorf("default emit does not contain the real-evidence-calibrated axis1 default (25000000): %s", defaultSQL)
	}

	overrideCtx := chsql.WithRangeBucketGridNativeMaxRows(context.Background(), 12_345_678)
	overrideSQL, _, err := chsql.Emit(overrideCtx, node)
	if err != nil {
		t.Fatalf("emit (override): %v", err)
	}
	if !strings.Contains(overrideSQL, "12345678") {
		t.Errorf("override emit does not contain the operator-overridden axis1 threshold (12345678): %s", overrideSQL)
	}
	if strings.Contains(overrideSQL, "25000000") {
		t.Errorf("override emit still contains the default axis1 threshold (25000000) — the override did not take effect: %s", overrideSQL)
	}
}

// TestRangeBucketGridNativeBound_MaxRowsOverride_NonPositiveIsIgnored proves
// rangeBucketGridNativeMaxRowsFromCtx's own "non-positive is treated as
// absent" contract (range_bucket_grid_native_bound.go's own
// WithRangeBucketGridNativeMaxRows doc): a direct chsql.Emit caller that
// threads a non-positive override (impossible in production — see
// config.resolveRBGNMaxRows, which never lets a non-positive value reach
// this far) still gets the real default, not a broken zero-row ceiling
// that would reject every query outright.
func TestRangeBucketGridNativeBound_MaxRowsOverride_NonPositiveIsIgnored(t *testing.T) {
	node := overrideTestPlan()
	zeroCtx := chsql.WithRangeBucketGridNativeMaxRows(context.Background(), 0)
	sqlStr, _, err := chsql.Emit(zeroCtx, node)
	if err != nil {
		t.Fatalf("emit (zero override): %v", err)
	}
	if !strings.Contains(sqlStr, "25000000") {
		t.Errorf("a zero override should fall back to the real default (25000000), got: %s", sqlStr)
	}
}

// TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_ChangesEmittedThreshold
// is TestRangeBucketGridNativeBound_MaxRowsOverride_ChangesEmittedThreshold's
// own twin for axis2. 400000000 is issue #2665's own real-evidence-calibrated
// default (see range_bucket_grid_native_bound.go's "Issue #2665
// recalibration" doc).
func TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_ChangesEmittedThreshold(t *testing.T) {
	node := overrideTestPlan()

	defaultSQL, _, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("emit (default): %v", err)
	}
	if !strings.Contains(defaultSQL, "400000000") {
		t.Errorf("default emit does not contain the real-evidence-calibrated axis2 default (400000000): %s", defaultSQL)
	}

	overrideCtx := chsql.WithRangeBucketGridNativeMaxDensityUnits(context.Background(), 98_765_432)
	overrideSQL, _, err := chsql.Emit(overrideCtx, node)
	if err != nil {
		t.Fatalf("emit (override): %v", err)
	}
	if !strings.Contains(overrideSQL, "98765432") {
		t.Errorf("override emit does not contain the operator-overridden axis2 threshold (98765432): %s", overrideSQL)
	}
	if strings.Contains(overrideSQL, "400000000") {
		t.Errorf("override emit still contains the default axis2 threshold (400000000) — the override did not take effect: %s", overrideSQL)
	}
}

// TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_NonPositiveIsIgnored
// is TestRangeBucketGridNativeBound_MaxRowsOverride_NonPositiveIsIgnored's
// own twin for axis2.
func TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_NonPositiveIsIgnored(t *testing.T) {
	node := overrideTestPlan()
	negativeCtx := chsql.WithRangeBucketGridNativeMaxDensityUnits(context.Background(), -1)
	sqlStr, _, err := chsql.Emit(negativeCtx, node)
	if err != nil {
		t.Fatalf("emit (negative override): %v", err)
	}
	if !strings.Contains(sqlStr, "400000000") {
		t.Errorf("a negative override should fall back to the real default (400000000), got: %s", sqlStr)
	}
}

// TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_ZeroIsIgnored covers
// the exact boundary TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_
// NonPositiveIsIgnored's negative-only override cannot reach:
// rangeBucketGridNativeMaxDensityUnitsFromCtx's `ok && n > 0` guard
// (range_bucket_grid_native_bound.go) only distinguishes strict `>` from
// `>=` at n == 0 exactly — a negative override is already false under
// either spelling, so it can never catch a `>` -> `>=` boundary flip. Mirrors
// TestRangeBucketGridNativeBound_MaxRowsOverride_NonPositiveIsIgnored, axis1's
// own zero case.
func TestRangeBucketGridNativeBound_MaxDensityUnitsOverride_ZeroIsIgnored(t *testing.T) {
	node := overrideTestPlan()
	zeroCtx := chsql.WithRangeBucketGridNativeMaxDensityUnits(context.Background(), 0)
	sqlStr, _, err := chsql.Emit(zeroCtx, node)
	if err != nil {
		t.Fatalf("emit (zero override): %v", err)
	}
	if !strings.Contains(sqlStr, "400000000") {
		t.Errorf("a zero override should fall back to the real default (400000000), got: %s", sqlStr)
	}
}

// TestResolveRangeBucketGridNativeMaxRows pins the "override-or-default"
// contract ResolveRangeBucketGridNativeMaxRows answers WITHOUT a context —
// added for issue #2705's route-B apportionment, which must resolve the
// whole-query bound BEFORE dividing it by a shard count, and a ctx-keyed
// lookup cannot do that (see the function's own doc for why).
func TestResolveRangeBucketGridNativeMaxRows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override int64
		want     int64
	}{
		{"positive override wins", 10, 10},
		{"zero falls back to the real default", 0, 25_000_000},
		{"negative falls back to the real default", -1, 25_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chsql.ResolveRangeBucketGridNativeMaxRows(tc.override); got != tc.want {
				t.Errorf("ResolveRangeBucketGridNativeMaxRows(%d) = %d, want %d", tc.override, got, tc.want)
			}
		})
	}
}

// TestResolveRangeBucketGridNativeMaxDensityUnits is the axis2 twin.
func TestResolveRangeBucketGridNativeMaxDensityUnits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override int64
		want     int64
	}{
		{"positive override wins", 10, 10},
		{"zero falls back to the real default", 0, 400_000_000},
		{"negative falls back to the real default", -1, 400_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chsql.ResolveRangeBucketGridNativeMaxDensityUnits(tc.override); got != tc.want {
				t.Errorf("ResolveRangeBucketGridNativeMaxDensityUnits(%d) = %d, want %d", tc.override, got, tc.want)
			}
		})
	}
}
