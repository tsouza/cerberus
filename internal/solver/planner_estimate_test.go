package solver

import "testing"

// TestPlan_EstimateNearEmpty_SkipsSharding pins issue #2787's near-empty
// advisory: the OOM fixture (TestPlan_OOMShapeRoutes' own fixture, which
// clears every pure-geometry threshold and routes at K=8) is refused
// instead, with ReasonEstimateNearEmpty, when RequestMeta.Estimate reports a
// total row estimate at or below Config.EstimateNearEmptyRowFloor — real
// DATA, not grid geometry, overriding a geometry-only verdict in the
// fail-safe direction (skip a route the pure thresholds would have taken).
func TestPlan_EstimateNearEmpty_SkipsSharding(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := oomMeta()
	meta.Estimate = &ScanEstimate{Rows: uint64(p.Cfg.EstimateNearEmptyRowFloor)}

	d, routed := p.Plan(oomWindow(), meta)
	if routed {
		t.Fatalf("near-empty estimate must refuse the route; got routed=true K=%d", d.K)
	}
	if d.Reason != ReasonEstimateNearEmpty {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonEstimateNearEmpty)
	}
}

// TestPlan_EstimateAboveFloor_StillRoutes pins the boundary: one row over
// the floor is NOT near-empty and the fixture routes exactly as it does with
// no estimate at all (TestPlan_OOMShapeRoutes).
func TestPlan_EstimateAboveFloor_StillRoutes(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := oomMeta()
	meta.Estimate = &ScanEstimate{Rows: uint64(p.Cfg.EstimateNearEmptyRowFloor) + 1}

	d, routed := p.Plan(oomWindow(), meta)
	if !routed {
		t.Fatalf("estimate one row above the floor must not refuse the route; reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
}

// TestPlan_EstimateJustifiesKAboveMaxK pins issue #2787's other advisory
// half: the OOM fixture's own grid derives K=12 (TestPlan_OOMShapeRoutes'
// own comment), clipped to the structural MaxK=8 backstop with no estimate.
// A dense-enough RequestMeta.Estimate raises the ceiling to
// MaxKWithEstimate, so the SAME fixture now routes at its full
// geometry-derived K=12 instead of being clipped — the #2685 relief,
// available again without reopening #2709's regression because it now
// requires a real, measured data volume rather than geometry alone.
func TestPlan_EstimateJustifiesKAboveMaxK(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := oomMeta()
	// Twelve shards' worth of EstimateMinRowsPerAdditionalShard, comfortably
	// clearing the derived K=12 with headroom to spare.
	meta.Estimate = &ScanEstimate{Rows: uint64(12 * p.Cfg.EstimateMinRowsPerAdditionalShard)}

	d, routed := p.Plan(oomWindow(), meta)
	if !routed {
		t.Fatalf("dense estimate must still route; reason=%q", d.Reason)
	}
	if d.K != 12 {
		t.Fatalf("K = %d, want 12 (the fixture's own derived K, no longer clipped by MaxK)", d.K)
	}
}

// TestPlan_SparseEstimate_StaysClippedAtMaxK pins the other side of the
// justification test: an estimate that clears the near-empty floor but
// cannot back even one additional shard above MaxK leaves the fixture
// clipped exactly as it is with no estimate at all — the estimate must
// never raise K further than the DATA it reports actually supports.
func TestPlan_SparseEstimate_StaysClippedAtMaxK(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := oomMeta()
	// Above the near-empty floor, but well under one additional shard's
	// worth of EstimateMinRowsPerAdditionalShard.
	meta.Estimate = &ScanEstimate{Rows: uint64(p.Cfg.EstimateNearEmptyRowFloor) + 1}

	d, routed := p.Plan(oomWindow(), meta)
	if !routed {
		t.Fatalf("estimate above the near-empty floor must still route; reason=%q", d.Reason)
	}
	if d.K != 8 {
		t.Fatalf("K = %d, want 8 (unjustified by the sparse estimate, still clipped at MaxK)", d.K)
	}
}

// TestPlan_NilEstimate_ByteIdenticalToPreEstimate pins the default-off
// contract: a nil RequestMeta.Estimate (every request until issue #2787's
// chopt feature is explicitly enabled) reproduces TestPlan_OOMShapeRoutes'
// own K=8-clipped verdict exactly.
func TestPlan_NilEstimate_ByteIdenticalToPreEstimate(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(oomWindow(), oomMeta())
	if !routed || d.K != 8 || d.Reason != ReasonRouted {
		t.Fatalf("routed=%v K=%d reason=%q, want routed=true K=8 reason=%q", routed, d.K, d.Reason, ReasonRouted)
	}
}
