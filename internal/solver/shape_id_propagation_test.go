package solver

import "testing"

// TestWithGrid_PropagatesShapeIDAndEstimate pins issue #2789's own additive
// readout: withGrid copies RequestMeta.ShapeID and RequestMeta.Estimate.Rows
// onto EVERY Decision it stamps — routed or not — exactly like the existing
// N/F/D/OuterRange/Step cost-grid fields, so internal/engine's actuals
// wiring can read them off the SAME Decision the caller already has without
// a second lookup.
func TestWithGrid_PropagatesShapeIDAndEstimate(t *testing.T) {
	meta := oomMeta()
	meta.ShapeID = "cerb:agg;rw"
	meta.Estimate = &ScanEstimate{Rows: 12345}

	p := &Planner{Cfg: autoCfg()}
	decision, routed := p.Plan(oomWindow(), meta)
	if !routed {
		t.Fatalf("expected the OOM fixture to route, got Reason=%q", decision.Reason)
	}
	if decision.ShapeID != "cerb:agg;rw" {
		t.Fatalf("expected ShapeID to propagate onto a routed Decision, got %q", decision.ShapeID)
	}
	if !decision.HasPredictedEstimate || decision.PredictedRows != 12345 {
		t.Fatalf("expected the Estimate to propagate onto a routed Decision, got HasPredictedEstimate=%v PredictedRows=%d",
			decision.HasPredictedEstimate, decision.PredictedRows)
	}
}

// TestWithGrid_PropagatesShapeIDOnNonRoutedDecision pins the same
// propagation for a plan that does NOT route — ModeSingle refuses every
// plan with ReasonRoutingDisabled, but withGrid still runs on that refusal
// (planner.go's own doc: "applied to BOTH routed and not-routed
// decisions").
func TestWithGrid_PropagatesShapeIDOnNonRoutedDecision(t *testing.T) {
	meta := oomMeta()
	meta.ShapeID = "cerb:agg;rw"

	cfg := DefaultConfig() // Mode == ModeSingle
	p := &Planner{Cfg: cfg}
	decision, routed := p.Plan(oomWindow(), meta)
	if routed {
		t.Fatal("expected ModeSingle to never route")
	}
	if decision.Reason != ReasonRoutingDisabled {
		t.Fatalf("expected ReasonRoutingDisabled, got %q", decision.Reason)
	}
	if decision.ShapeID != "cerb:agg;rw" {
		t.Fatalf("expected ShapeID to propagate onto a non-routed Decision too, got %q", decision.ShapeID)
	}
}

// TestWithGrid_EmptyShapeIDAndNilEstimateByDefault pins the kill-switch-off
// byte-unchanged contract: a caller that never sets RequestMeta.ShapeID /
// Estimate (every existing caller, and any caller with issue #2789's
// actuals feature off) gets a Decision with an empty ShapeID and
// HasPredictedEstimate=false, exactly as before this field existed.
func TestWithGrid_EmptyShapeIDAndNilEstimateByDefault(t *testing.T) {
	p := &Planner{Cfg: autoCfg()}
	decision, routed := p.Plan(oomWindow(), oomMeta())
	if !routed {
		t.Fatalf("expected the OOM fixture to route, got Reason=%q", decision.Reason)
	}
	if decision.ShapeID != "" {
		t.Fatalf("expected an empty ShapeID by default, got %q", decision.ShapeID)
	}
	if decision.HasPredictedEstimate || decision.PredictedRows != 0 {
		t.Fatalf("expected no predicted estimate by default, got HasPredictedEstimate=%v PredictedRows=%d",
			decision.HasPredictedEstimate, decision.PredictedRows)
	}
}
