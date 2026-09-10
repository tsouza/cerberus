package engine

import (
	"context"
	"maps"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/solver"
)

// routeBParityRules turns on the two SettingsRules whose eligibility is
// unconditional on any plan that has a shape id at all — a workload name and
// the log_comment shape id — so the parity assertions below test the WIRING
// (does route B run SettingsRules.apply at all?) rather than any one rule's
// own plan-shape predicate, which the rules' own tests already cover.
func routeBParityRules() SettingsRules {
	return SettingsRules{
		QueryWorkload:   "cerberus_queries",
		LogCommentShape: true,
	}
}

// TestRouteBExecCtx_AppliesSettingsRules pins that a routed dispatch carries
// the flag-gated per-query settings rules at all.
//
// Before cerberus issue #3184 SettingsRules.apply was called only from
// execContext (route A), so a routed query carried no `workload` — flatly
// contradicting that env var's own documented contract, "stamp on every
// outgoing query" — no log_comment shape id, no result cache and no
// aggregation-in-order, while observeRoutedQuery went on recording
// enabledOpts() for the route-B corpus row anyway. Calibration therefore
// compared an optimized route A against an un-optimized route B and
// attributed the whole difference to an optimization posture that had never
// actually ridden the shards.
func TestRouteBExecCtx_AppliesSettingsRules(t *testing.T) {
	t.Parallel()

	rules := routeBParityRules()
	ctx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix,
		&solver.Decision{K: 4}, routeBTestPlan(), 0, rules, 0, false, ResourceBoundOverrides{}, 0, 0, nil, nil)
	settings := chclient.QuerySettingsFromContext(ctx)

	if got := settings[chclient.SettingWorkload]; got != rules.QueryWorkload {
		t.Errorf("route-B %s = %v, want %q — a routed query that carries no workload is invisible to every server-side scheduling policy an operator set up",
			chclient.SettingWorkload, got, rules.QueryWorkload)
	}
	wantComment := planShapeID(routeBTestPlan())
	if wantComment == "" {
		t.Fatal("routeBTestPlan has no shape id; the log_comment assertion below would be vacuous")
	}
	if got := settings[settingLogComment]; got != wantComment {
		t.Errorf("route-B %s = %v, want %q — without it a routed query cannot be grouped by shape in system.query_log",
			settingLogComment, got, wantComment)
	}
}

// TestRouteBExecCtx_SettingsMatchRouteAAtK1 is the anti-drift assertion the
// per-rule checks above cannot make: at K=1 a shard's apportioned memory cap
// IS the whole-query cap, so every memory-sized threshold must land on the
// same value and the two seams' ClickHouse settings maps must be byte-equal.
//
// This is what makes applySharedQuerySettings load-bearing rather than
// cosmetic. Route A and route B previously kept two hand-maintained parallel
// lists, which is how route B came to be missing SettingsRules.apply in the
// first place; this test fails for ANY future setting added to one seam and
// not the other, not just the ones #3184 happened to catch.
func TestRouteBExecCtx_SettingsMatchRouteAAtK1(t *testing.T) {
	t.Parallel()

	rules := routeBParityRules()
	plan := routeBTestPlan()
	decision := &solver.Decision{K: 1}

	// A nil Client means queryMemoryCap() is 0 on the route-A side, and
	// apportionShardMemoryCap(0, K) is 0 by its no-cap sentinel, so both seams
	// size their thresholds from the same input. The memory-cap SIZING itself
	// is pinned separately by TestRouteBExecCtx_SpillThresholdsSizedFromTheShardCap;
	// what this test pins is which KEYS each seam stamps.
	e := &Engine{Settings: rules}
	routeACtx, _ := e.execContext(context.Background(), plan, "promql", decision)
	routeA := chclient.QuerySettingsFromContext(routeACtx)

	routeBCtx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix,
		decision, plan, 0, rules, 0, false, ResourceBoundOverrides{}, 0, 0, nil, nil)
	routeB := chclient.QuerySettingsFromContext(routeBCtx)

	if len(routeA) == 0 {
		t.Fatal("route A stamped no settings at all; the equality below would be vacuous")
	}
	if !maps.Equal(routeA, routeB) {
		t.Errorf("route-A and route-B ClickHouse settings diverge at K=1.\n route A: %v\n route B: %v\n"+
			"Both seams must read ONE list (applySharedQuerySettings); a key present on one side only is exactly the drift issue #3184 reported.",
			routeA, routeB)
	}
}

// TestRouteBExecCtx_ExpHistogramTwoLevelStampedOnBothRoutes closes the
// vacuity hole TestRouteBExecCtx_SettingsMatchRouteAAtK1 leaves for a
// plan-shape-gated stamp. That test compares route A's and route B's whole
// settings maps on routeBTestPlan() — a bare Scan — so a key that only ever
// appears on an exp-histogram plan is equal-because-absent on both sides and
// the assertion says nothing about it. This one hands BOTH seams a plan that
// actually satisfies the predicate and asserts the key is present, with the
// same value, on each.
//
// Sizing is not part of the comparison because it cannot differ: the
// threshold is absolute rather than cap-relative (see
// expHistogramTwoLevelThresholdBytes), so unlike the spill thresholds there
// is no shard-apportioned variant for route B to get wrong.
func TestRouteBExecCtx_ExpHistogramTwoLevelStampedOnBothRoutes(t *testing.T) {
	t.Parallel()

	const memCap = int64(8 << 30)
	rules := routeBParityRules()
	rules.ExpHistogramTwoLevel = true
	plan := expHistogramWindowPlan()

	e := &Engine{Settings: rules}
	routeACtx, _ := e.execContext(context.Background(), plan, "promql", &solver.Decision{K: 1})
	routeBCtx := routeBExecCtx(context.Background(), "promql", chclient.ResponseShapeMatrix, &solver.Decision{K: 1},
		plan, memCap, rules, 0, false, ResourceBoundOverrides{}, 0, 0, nil, nil)

	routeA := chclient.QuerySettingsFromContext(routeACtx)
	routeB := chclient.QuerySettingsFromContext(routeBCtx)
	wantA, okA := routeA[settingGroupByTwoLevelThresholdBytes]
	if !okA {
		t.Fatalf("route A did not stamp %s on an exp-histogram window plan", settingGroupByTwoLevelThresholdBytes)
	}
	wantB, okB := routeB[settingGroupByTwoLevelThresholdBytes]
	if !okB {
		t.Fatalf("route B did not stamp %s on an exp-histogram window plan — a routed shard would run unbounded", settingGroupByTwoLevelThresholdBytes)
	}
	if wantA != wantB {
		t.Fatalf("%s = %v on route A but %v on route B", settingGroupByTwoLevelThresholdBytes, wantA, wantB)
	}
}
