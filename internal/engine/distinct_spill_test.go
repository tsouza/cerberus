package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/solver"
)

// TestApplyDistinctSpillSettings_StampsOnEveryPlanWhenEnabled pins the gate:
// the resolved distinct_spill verdict alone decides the stamp, whatever the
// plan's shape — a DISTINCT is an emitter-introduced modifier, not a plan
// node, so a shape-gated stamp would miss the lowerings that introduce one.
func TestApplyDistinctSpillSettings_StampsOnEveryPlanWhenEnabled(t *testing.T) {
	t.Parallel()

	// validatedDistinctSpillBytes is half of testQueryMemoryCap, the same
	// cap-relative arithmetic the group_by/sort/join stamps use.
	const validatedDistinctSpillBytes int64 = 1 << 30

	for _, p := range compositionPlans() {
		for _, enabled := range []bool{true, false} {
			ctx := applyDistinctSpillSettings(context.Background(), testQueryMemoryCap, enabled)
			got := settingValue(ctx, settingMaxBytesBeforeExternalDistinct)
			switch {
			case enabled && got != validatedDistinctSpillBytes:
				t.Errorf("%s, enabled: max_bytes_before_external_distinct = %v; want %v (half the %d-byte cap)",
					p.name, got, validatedDistinctSpillBytes, testQueryMemoryCap)
			case !enabled && got != nil:
				t.Errorf("%s, disabled (server < 26.9): max_bytes_before_external_distinct = %v; want absent", p.name, got)
			}
		}
	}
}

// TestApplyDistinctSpillSettings_CapRelative pins the threshold to
// spillThreshold, including the uncapped fallback: 0 would DISABLE the
// external DISTINCT, so an unconfigured cap must still stamp
// spillThresholdBytes.
func TestApplyDistinctSpillSettings_CapRelative(t *testing.T) {
	t.Parallel()

	for _, memCap := range []int64{6 * gib, 0} {
		ctx := applyDistinctSpillSettings(context.Background(), memCap, true)
		want := spillThreshold(memCap)
		if got := settingValue(ctx, settingMaxBytesBeforeExternalDistinct); got != want {
			t.Errorf("cap %d: max_bytes_before_external_distinct = %v; want %v (spillThreshold)", memCap, got, want)
		}
		if want <= 0 {
			t.Errorf("cap %d: spillThreshold = %d; a non-positive threshold disables the external DISTINCT", memCap, want)
		}
	}
}

// TestDistinctSpill_StampedOnBothRoutes drives the two dispatch seams with
// the flag on and requires each to carry the threshold sized from its OWN
// memory allowance: route A from the whole cap, route B from the per-shard
// cap, so a shard's DISTINCT spills before the shard's own limit.
func TestDistinctSpill_StampedOnBothRoutes(t *testing.T) {
	t.Parallel()

	const (
		memCap = int64(8 << 30)
		shards = 4
	)
	rules := compositionBaseRules()
	rules.DistinctSpill = true
	plan := routeBTestPlan()

	routeA := routeAQuerySettings(context.Background(), plan, memCap, rules)
	if got, want := settingValue(routeA, settingMaxBytesBeforeExternalDistinct), spillThreshold(memCap); got != want {
		t.Errorf("route A: max_bytes_before_external_distinct = %v; want %v", got, want)
	}

	decision := &solver.Decision{K: shards}
	shardCap := apportionShardMemoryCap(memCap, shards)
	routeB := routeBExecCtx(context.Background(), "traceql", chclient.ResponseShapeMatrix, decision, plan, memCap, rules,
		0, false, ResourceBoundOverrides{}, shards, 0, 0, nil)
	if got, want := settingValue(routeB, settingMaxBytesBeforeExternalDistinct), spillThreshold(shardCap); got != want {
		t.Errorf("route B: max_bytes_before_external_distinct = %v; want %v (half the %d-byte shard cap)", got, want, shardCap)
	}

	rules.DistinctSpill = false
	if got := settingValue(routeAQuerySettings(context.Background(), plan, memCap, rules), settingMaxBytesBeforeExternalDistinct); got != nil {
		t.Errorf("flag off: route A stamped max_bytes_before_external_distinct = %v; want absent", got)
	}
}
