package solver

import "testing"

// TestPlan_SetThresholds_FlipsRouting proves the autotune hot-swap actually
// drives the classifier: the same plan that routes A under a fan-out gate above
// its F routes B once SetThresholds lowers the gate below F. This is the
// behavioral link the loop depends on — the atomic overlay is read on the Plan
// hot path, not merely stored.
func TestPlan_SetThresholds_FlipsRouting(t *testing.T) {
	t.Parallel()

	// Fan-out gate (25) above the OOM shape's F (=20): routes A / below-threshold.
	cfg := autoCfg()
	cfg.MinFanout = 25
	p := &Planner{Cfg: cfg}

	d, routed := p.Plan(oomWindow(), oomMeta())
	if routed || d.Reason != ReasonBelowThreshold {
		t.Fatalf("pre-swap: routed=%v reason=%q, want route A (below-threshold)", routed, d.Reason)
	}

	// Hot-swap the gate below F. The overlay must now win over Cfg.MinFanout.
	p.SetThresholds(15, cfg.MinAnchorPairs)
	if gotF, gotP := p.CurrentThresholds(); gotF != 15 || gotP != cfg.MinAnchorPairs {
		t.Fatalf("CurrentThresholds = (%d, %d), want (15, %d)", gotF, gotP, cfg.MinAnchorPairs)
	}

	d, routed = p.Plan(oomWindow(), oomMeta())
	if !routed || d.Reason != ReasonRouted {
		t.Fatalf("post-swap: routed=%v reason=%q, want route B (routed)", routed, d.Reason)
	}
	if d.Strategy != StrategyShardedTimeslice {
		t.Fatalf("strategy = %q, want %q", d.Strategy, StrategyShardedTimeslice)
	}
}

// TestPlan_NilOverlay_UsesConfig confirms a Planner with no hot-swap applied
// reads its configured thresholds — the byte-identical fixed-threshold path.
func TestPlan_NilOverlay_UsesConfig(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	gotF, gotP := p.CurrentThresholds()
	if gotF != defaultMinFanout || gotP != defaultMinAnchorPairs {
		t.Fatalf("CurrentThresholds = (%d, %d), want (%d, %d)",
			gotF, gotP, defaultMinFanout, defaultMinAnchorPairs)
	}
}
