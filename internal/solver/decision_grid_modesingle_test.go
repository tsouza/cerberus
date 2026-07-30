package solver

import "testing"

// TestDecision_CostGrid_PopulatedUnderModeSingle pins that the operator
// kill-switch does not blind the calibration corpus.
//
// Mode=single is the ship-dark default: the Planner still classifies every
// plan, and every one of those Decisions reaches otel.cerberus_router_corpus.
// A deployment that has never enabled routing is exactly the one whose corpus
// an operator reads to decide whether to enable it, so a zero grid there is
// not a harmless omission — it is the whole dataset.
//
// The two public entry points reach the mode gate by different paths (Plan's
// classification seam, Eligible's route-memo seam) and must not disagree about
// the same plan's geometry.
func TestDecision_CostGrid_PopulatedUnderModeSingle(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig() // Mode == single
	p := &Planner{Cfg: cfg}
	plan, meta := oomWindow(), oomMeta()

	// The grid the classifier actually computed for this plan, read back
	// through the same withGrid production uses. Compared field-by-field
	// below rather than against hard-coded numbers, so the test tracks the
	// fixture instead of duplicating it.
	sig, _, _, eligible := p.classify(plan, meta)
	if !eligible {
		t.Fatalf("fixture must be an ELIGIBLE plan so the ModeSingle branch is the one that refuses it")
	}
	want := (&Decision{}).withGrid(sig, meta)

	// Anti-tautology guard: an all-zero grid would make the comparison below
	// vacuous, so pin that the fixture carries real geometry to lose.
	if want.NAnchors == 0 || want.Fanout == 0 || want.CumulativeD == 0 ||
		want.OuterRange == 0 || want.Step == 0 {
		t.Fatalf("fixture carries no geometry: N=%d F=%d D=%s OuterRange=%s Step=%s",
			want.NAnchors, want.Fanout, want.CumulativeD, want.OuterRange, want.Step)
	}

	planD, planRouted := p.Plan(plan, meta)
	eligD, eligRouted := p.Eligible(plan, meta)

	for _, entry := range []struct {
		name   string
		d      *Decision
		routed bool
	}{
		{"Plan", planD, planRouted},
		{"Eligible", eligD, eligRouted},
	} {
		if entry.routed {
			t.Fatalf("%s() routed under Mode=single — ship-dark guarantee broken", entry.name)
		}
		if entry.d == nil {
			t.Fatalf("%s() returned a nil Decision", entry.name)
		}
		if entry.d.NAnchors != want.NAnchors || entry.d.Fanout != want.Fanout ||
			entry.d.CumulativeD != want.CumulativeD || entry.d.OuterRange != want.OuterRange ||
			entry.d.Step != want.Step {
			t.Errorf("%s() grid = N=%d F=%d D=%s OuterRange=%s Step=%s, want N=%d F=%d D=%s OuterRange=%s Step=%s",
				entry.name,
				entry.d.NAnchors, entry.d.Fanout, entry.d.CumulativeD, entry.d.OuterRange, entry.d.Step,
				want.NAnchors, want.Fanout, want.CumulativeD, want.OuterRange, want.Step)
		}
	}
}

// TestModeSingle_RefusalIsRoutingDisabledNotBelowThreshold pins the reason a
// kill-switched deployment records.
//
// below-threshold asserts something specific: a cost threshold WAS evaluated
// and this plan fell under it. That makes the row evidence about where the
// threshold sits, and rules whose advice is "move the threshold" select on it.
// Under Mode=single no threshold is consulted at all, so recording
// below-threshold would seed those rules with a population that cannot support
// their advice — the same defect as the zero cost grid, one field over: a
// Decision claiming a conclusion the classifier never reached.
func TestModeSingle_RefusalIsRoutingDisabledNotBelowThreshold(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig() // Mode == single
	p := &Planner{Cfg: cfg}
	plan, meta := oomWindow(), oomMeta()

	for _, entry := range []struct {
		name string
		call func() (*Decision, bool)
	}{
		{"Plan", func() (*Decision, bool) { return p.Plan(plan, meta) }},
		{"Eligible", func() (*Decision, bool) { return p.Eligible(plan, meta) }},
	} {
		d, routed := entry.call()
		if routed || d == nil {
			t.Fatalf("%s() = routed=%v decision=%v, want a non-nil refusal", entry.name, routed, d)
		}
		if d.Reason != ReasonRoutingDisabled {
			t.Errorf("%s() reason = %q, want %q", entry.name, d.Reason, ReasonRoutingDisabled)
		}
	}
}
