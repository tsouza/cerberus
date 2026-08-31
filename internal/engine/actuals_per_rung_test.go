package engine

import (
	"testing"

	"github.com/tsouza/cerberus/internal/actuals"
)

func TestMaybeSeedPerRungAdmissionFromActuals_NoOpWithoutBothWired(t *testing.T) {
	plan := perRungTestPlan()
	decision := perRungTestDecision()

	// Neither wired.
	e := &Engine{}
	e.maybeSeedPerRungAdmissionFromActuals(plan, "cerb:agg", decision)

	// Only PerRungAdmission wired.
	e = &Engine{PerRungAdmission: NewPerRungAdmissionLearner()}
	e.maybeSeedPerRungAdmissionFromActuals(plan, "cerb:agg", decision)
	if e.PerRungAdmission.ShouldDeclineBypass(perRungTestKey()) {
		t.Fatal("expected no seeding with Actuals nil")
	}

	// Only Actuals wired.
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	tracker.RecordPredicted("cerb:agg", 100)
	e = &Engine{Actuals: tracker}
	e.maybeSeedPerRungAdmissionFromActuals(plan, "cerb:agg", decision) // must not panic; nothing to seed
}

func TestMaybeSeedPerRungAdmissionFromActuals_SeedsOnLowCorroboratedActuals(t *testing.T) {
	plan := perRungTestPlan()
	decision := perRungTestDecision() // NAnchors=1441

	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg"
	// Two corroborating observations, both well under
	// NAnchors*perRungCheapRowsPerAnchor = 1441*20 = 28820.
	for i := 0; i < perRungEvidenceMinObservations; i++ {
		if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 100}, actuals.SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}

	e := &Engine{PerRungAdmission: NewPerRungAdmissionLearner(), Actuals: tracker}
	key := shapeKey(plan, decision)

	// A single seed call is one observation's worth (SeedPriorFromEstimate's
	// own doc) — it does not immediately decline the bypass, but it DOES
	// register a fresh entry, exactly like a single EXPLAIN ESTIMATE seed
	// would (explain_estimate_wiring_test.go's own assertion style).
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	if !e.PerRungAdmission.hasFreshEntry(key) {
		t.Fatal("expected a fresh entry after seeding from a corroborated cheap actuals reading")
	}
	if e.PerRungAdmission.ShouldDeclineBypass(key) {
		t.Fatal("a single seed must not immediately decline the bypass")
	}

	// A second seed call (mirroring a second request for the same shape)
	// reaches perRungEvidenceMinObservations and declines the bypass.
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	if !e.PerRungAdmission.ShouldDeclineBypass(key) {
		t.Fatal("expected the per-rung bypass to be declined after two corroborating seeds")
	}
}

func TestMaybeSeedPerRungAdmissionFromActuals_NoSeedBelowMinObservations(t *testing.T) {
	plan := perRungTestPlan()
	decision := perRungTestDecision()

	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg"
	// Only ONE observation — below perRungEvidenceMinObservations.
	if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 100}, actuals.SourcePacket); !ok {
		t.Fatal("RecordActual should succeed")
	}

	e := &Engine{PerRungAdmission: NewPerRungAdmissionLearner(), Actuals: tracker}
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)

	key := shapeKey(plan, decision)
	if e.PerRungAdmission.ShouldDeclineBypass(key) {
		t.Fatal("expected no seeding below the corroboration floor")
	}
}

func TestMaybeSeedPerRungAdmissionFromActuals_NoSeedWhenNotCheap(t *testing.T) {
	plan := perRungTestPlan()
	decision := perRungTestDecision() // NAnchors=1441, cheap floor = 28820

	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg"
	for i := 0; i < perRungEvidenceMinObservations; i++ {
		// Well ABOVE the cheap floor.
		if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 1_000_000}, actuals.SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}

	e := &Engine{PerRungAdmission: NewPerRungAdmissionLearner(), Actuals: tracker}
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)

	key := shapeKey(plan, decision)
	if e.PerRungAdmission.ShouldDeclineBypass(key) {
		t.Fatal("expected no seeding when the actual EMA is not cheap relative to anchor count")
	}
}
