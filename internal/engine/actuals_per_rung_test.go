package engine

import (
	"testing"
	"time"

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

	// A second, IMMEDIATE seed call (mirroring the very next request for the
	// same shape, still inside the freshness window) must now be a no-op —
	// issue #3034's own fix: reseeding an already-fresh entry is exactly
	// what kept a decline's TTL from ever lapsing. Corroboration toward an
	// actual decline must come from evidence spread out in time (a real
	// Observe(), or a seed after the previous one has genuinely gone stale —
	// see TestMaybeSeedPerRungAdmissionFromActuals_DoesNotReseedAFreshEntry),
	// never from calling the seed twice in the same instant.
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	if e.PerRungAdmission.ShouldDeclineBypass(key) {
		t.Fatal("a second immediate seed call on an already-fresh entry must not itself decline the bypass — " +
			"the guard must have skipped it, not corroborated it")
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

// TestMaybeSeedPerRungAdmissionFromActuals_DoesNotReseedAFreshEntry is the
// direct regression test for issue #3034's own bug: before the fix, this
// hook called PerRungAdmissionLearner.SeedPriorFromEstimate — and therefore
// record's own unconditional `st.lastObserved = time.Now()` — on EVERY
// request whose actuals EMA still looked cheap, regardless of whether the
// learner already held a fresh verdict. That indefinitely renewed
// perRungEvidenceTTL and made a decline permanent, because the seed alone
// (no real per-rung dispatch ever runs again once declined) was the only
// thing still touching the entry. This asserts the literal mechanism the
// bug hinged on — lastObserved and consecutiveCheap after repeated,
// immediate seed calls — is provably unchanged from the first call, then
// confirms the guard still reseeds correctly once the entry genuinely goes
// stale.
func TestMaybeSeedPerRungAdmissionFromActuals_DoesNotReseedAFreshEntry(t *testing.T) {
	plan := perRungTestPlan()
	decision := perRungTestDecision()
	tracker := actuals.NewTracker(actuals.DefaultConfig())
	const shape = "cerb:agg"
	for i := 0; i < perRungEvidenceMinObservations; i++ {
		if _, ok := tracker.RecordActual(shape, actuals.Actual{ReadRows: 100}, actuals.SourcePacket); !ok {
			t.Fatal("RecordActual should succeed")
		}
	}
	e := &Engine{PerRungAdmission: NewPerRungAdmissionLearner(), Actuals: tracker}
	key := shapeKey(plan, decision)

	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	e.PerRungAdmission.mu.Lock()
	firstObservedAt := e.PerRungAdmission.states[key].lastObserved
	firstStreak := e.PerRungAdmission.states[key].consecutiveCheap
	e.PerRungAdmission.mu.Unlock()

	// Simulate the seed firing again on "effectively every request" while
	// the entry is still fresh — the pre-fix behavior this hook's own doc
	// now warns against.
	for i := 0; i < 5; i++ {
		e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	}

	e.PerRungAdmission.mu.Lock()
	laterObservedAt := e.PerRungAdmission.states[key].lastObserved
	laterStreak := e.PerRungAdmission.states[key].consecutiveCheap
	e.PerRungAdmission.mu.Unlock()

	if !laterObservedAt.Equal(firstObservedAt) {
		t.Fatal("repeated seed calls on an already-fresh entry kept refreshing lastObserved — " +
			"this is exactly issue #3034's bug: an indefinitely self-renewing seed never lets " +
			"perRungEvidenceTTL lapse, so a decline can never relax")
	}
	if laterStreak != firstStreak {
		t.Fatalf("repeated seed calls on an already-fresh entry changed consecutiveCheap %d -> %d, want unchanged",
			firstStreak, laterStreak)
	}

	// Now let the entry genuinely go stale (no seed calls in between,
	// mirroring perRungEvidenceTTL passing with no traffic) and confirm the
	// guard correctly steps aside once there is nothing fresh to protect —
	// the seed mechanism must still work when a reseed is actually due.
	e.PerRungAdmission.mu.Lock()
	e.PerRungAdmission.states[key].lastObserved = time.Now().Add(-perRungEvidenceTTL - time.Minute)
	e.PerRungAdmission.mu.Unlock()

	if e.PerRungAdmission.hasFreshEntry(key) {
		t.Fatal("fixture setup: expected the entry to read as stale before the next seed attempt")
	}
	e.maybeSeedPerRungAdmissionFromActuals(plan, shape, decision)
	e.PerRungAdmission.mu.Lock()
	postStaleObservedAt := e.PerRungAdmission.states[key].lastObserved
	e.PerRungAdmission.mu.Unlock()
	if !postStaleObservedAt.After(firstObservedAt) {
		t.Fatal("a seed call on a genuinely stale entry must still reseed — the guard only skips an " +
			"already-fresh entry, it must never permanently disable seeding for a shape")
	}
}
