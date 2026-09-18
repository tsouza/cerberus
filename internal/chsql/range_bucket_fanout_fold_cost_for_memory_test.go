package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

const gib = int64(1 << 30)

// TestRangeBucketFanoutFoldCostUnitsForMemory_ScalesWithTheCap pins that
// the fold-cost ceiling is a ratio of the cap it defends, not a number: two
// caps yield two ceilings in the same ratio, the sub-GiB remainder is
// credited proportionally, an unset cap answers the 1 GiB calibration, and
// no cap can drive the ceiling to a 0 the ctx readers would mistake for
// "unset".
func TestRangeBucketFanoutFoldCostUnitsForMemory_ScalesWithTheCap(t *testing.T) {
	t.Parallel()

	oneGiB := chsql.RangeBucketFanoutFoldCostUnitsForMemory(gib)
	for _, tc := range []struct {
		name string
		cap  int64
		want int64
	}{
		{name: "two caps, two ceilings, ratio preserved", cap: 2 * gib, want: 2 * oneGiB},
		{name: "half the cap, half the ceiling", cap: gib / 2, want: oneGiB / 2},
		{name: "a quarter of the cap, a quarter of the ceiling", cap: gib / 4, want: oneGiB / 4},
		{name: "1.5 GiB credits the remainder proportionally", cap: gib + gib/2, want: oneGiB + oneGiB/2},
		{name: "6 GiB, the cap real deployments run", cap: 6 * gib, want: 6 * oneGiB},
		{name: "unset cap answers the 1 GiB calibration", cap: 0, want: oneGiB},
		{name: "negative cap answers the 1 GiB calibration", cap: -1, want: oneGiB},
		{name: "a byte-scale cap floors at 1, never 0", cap: 1, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chsql.RangeBucketFanoutFoldCostUnitsForMemory(tc.cap); got != tc.want {
				t.Errorf("RangeBucketFanoutFoldCostUnitsForMemory(%d) = %d, want %d", tc.cap, got, tc.want)
			}
		})
	}
	if oneGiB != 15_000_000 {
		t.Errorf("the 1 GiB ceiling = %d, want the 15,000,000-unit calibration the default lane and the #3514 corpus were admitted under", oneGiB)
	}
}

// TestResolveRangeBucketFanoutFoldCostMaxUnits pins the override-or-derive
// contract the engine relies on: a positive override pins the ceiling
// whatever the cap, and an unset one derives it from the cap.
func TestResolveRangeBucketFanoutFoldCostMaxUnits(t *testing.T) {
	t.Parallel()

	if got := chsql.ResolveRangeBucketFanoutFoldCostMaxUnits(999, 4*gib); got != 999 {
		t.Errorf("override 999 under a 4 GiB cap resolved to %d, want the override", got)
	}
	if got, want := chsql.ResolveRangeBucketFanoutFoldCostMaxUnits(0, 4*gib), chsql.RangeBucketFanoutFoldCostUnitsForMemory(4*gib); got != want {
		t.Errorf("unset override under a 4 GiB cap resolved to %d, want the cap-derived %d", got, want)
	}
	if got, want := chsql.ResolveRangeBucketFanoutFoldCostMaxUnits(-5, 0), chsql.RangeBucketFanoutFoldCostUnitsForMemory(0); got != want {
		t.Errorf("negative override with no cap resolved to %d, want the 1 GiB calibration %d", got, want)
	}
}

// TestRangeBucketFanoutFoldCostCeiling_ThreadedValueReachesTheGuard pins
// that a cap-derived ceiling threaded through
// WithRangeBucketFanoutFoldCostMaxUnits is the literal the emitted fold-cost
// guard compares against — a 512 MiB deployment's guard fires at half the
// 1 GiB ceiling, not at the calibration constant.
func TestRangeBucketFanoutFoldCostCeiling_ThreadedValueReachesTheGuard(t *testing.T) {
	t.Parallel()

	halfCap := chsql.RangeBucketFanoutFoldCostUnitsForMemory(gib / 2)
	ctx := chsql.WithRangeBucketFanoutFoldCostMaxUnits(context.Background(), halfCap)
	sql, _, err := chsql.Emit(ctx, resourceBoundFanoutGroupPlan())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if got := strings.Count(sql, "> 7500000"); got != 1 {
		t.Errorf("expected the 512 MiB ceiling \"> 7500000\" exactly once, got %d\nSQL:\n%s", got, sql)
	}
	if strings.Contains(sql, "> 15000000") {
		t.Errorf("the 1 GiB calibration must not survive a smaller cap\nSQL:\n%s", sql)
	}
}
