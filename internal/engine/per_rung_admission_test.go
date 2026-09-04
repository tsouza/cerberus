package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- fixtures --------------------------------------------------------------

// perRungTestPlan is a minimal, stable chplan.Node — routememo.KeyFor only
// needs a walkable tree, and every test in this file uses the SAME plan/N/F/
// Step so its Key is stable across calls.
func perRungTestPlan() chplan.Node {
	return &chplan.Aggregate{
		Input:    &chplan.Scan{Table: "otel_metrics_histogram"},
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "anchor_ts"}},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}}},
	}
}

// perRungTestNAnchors mirrors the #2709 failing panel's own grid (24h/1m).
const perRungTestNAnchors = 1441

func perRungTestKey() routememo.Key {
	return routememo.KeyFor(perRungTestPlan(), perRungTestNAnchors, 1, time.Minute)
}

// perRungTestDecision is a routed, PerRungPredictive Decision over
// perRungTestPlan/perRungTestKey's own grid — the shape refinePerRungAdmission
// is meant to re-check.
func perRungTestDecision() *solver.Decision {
	return &solver.Decision{
		Strategy:          solver.StrategyShardedTimeslice,
		K:                 8,
		Reason:            solver.ReasonRouted,
		Slices:            []solver.Slice{{}},
		NAnchors:          perRungTestNAnchors,
		Fanout:            1,
		Step:              time.Minute,
		PerRungPredictive: true,
	}
}

// --- PerRungAdmissionLearner -----------------------------------------------

func TestPerRungAdmissionLearner_NoEvidenceKeepsTodaysBehavior(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	if l.ShouldDeclineBypass(perRungTestKey()) {
		t.Fatal("a never-observed shape must not decline the bypass — the always-safe default is " +
			"today's unchanged anchor-only admission")
	}
}

func TestPerRungAdmissionLearner_DeclinesAfterConsecutiveCheapDrains(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	key := perRungTestKey()

	// A cheap output well under perRungCheapRowsPerAnchor * N (1441*20).
	const cheapRows = 100

	l.Observe(key, cheapRows, perRungTestNAnchors)
	if l.ShouldDeclineBypass(key) {
		t.Fatal("a single cheap observation declined the bypass — " +
			"perRungEvidenceMinObservations exists precisely so one reading cannot flip the verdict alone")
	}

	l.Observe(key, cheapRows, perRungTestNAnchors)
	if !l.ShouldDeclineBypass(key) {
		t.Fatalf("%d consecutive cheap drains did not decline the bypass", perRungEvidenceMinObservations)
	}
}

func TestPerRungAdmissionLearner_ExpensiveDrainResetsTheStreak(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	key := perRungTestKey()
	const cheapRows = 100
	// An output at/above perRungCheapRowsPerAnchor * N — genuinely wide output.
	const expensiveRows = perRungTestNAnchors * perRungCheapRowsPerAnchor

	l.Observe(key, cheapRows, perRungTestNAnchors)
	l.Observe(key, expensiveRows, perRungTestNAnchors)
	if l.ShouldDeclineBypass(key) {
		t.Fatal("an expensive drain between two cheap ones must reset the streak, not just get ignored — " +
			"a shape that started producing real output is exactly the case the streak exists to protect " +
			"predictive routing for")
	}

	l.Observe(key, cheapRows, perRungTestNAnchors)
	if l.ShouldDeclineBypass(key) {
		t.Fatal("only one cheap observation followed the reset — must not decline yet")
	}
}

func TestPerRungAdmissionLearner_StaleEvidenceIsIgnored(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	key := perRungTestKey()
	const cheapRows = 100

	l.Observe(key, cheapRows, perRungTestNAnchors)
	l.Observe(key, cheapRows, perRungTestNAnchors)
	if !l.ShouldDeclineBypass(key) {
		t.Fatal("fixture setup: expected the streak to already qualify before aging it")
	}

	// Same package as production code: reach in and age the entry past its
	// TTL directly, exactly as routememo's own tests manipulate internal
	// state rather than needing a clock-injection seam for a single test.
	l.mu.Lock()
	l.states[key].lastObserved = time.Now().Add(-perRungEvidenceTTL - time.Minute)
	l.mu.Unlock()

	if l.ShouldDeclineBypass(key) {
		t.Fatal("evidence older than perRungEvidenceTTL must be treated as no-evidence — a metric's " +
			"real cardinality can grow, and a stale cheap verdict must not suppress predictive " +
			"routing forever")
	}
}

func TestPerRungAdmissionLearner_CapacityEvictsRatherThanGrowsUnbounded(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	for i := 0; i < perRungLearnerCapacity+8; i++ {
		key := routememo.KeyFor(perRungTestPlan(), i+1, 1, time.Minute)
		l.Observe(key, 1, int64(i+1))
	}
	l.mu.Lock()
	n := len(l.states)
	l.mu.Unlock()
	if n > perRungLearnerCapacity {
		t.Fatalf("learner grew to %d entries, want <= %d (perRungLearnerCapacity)", n, perRungLearnerCapacity)
	}
}

// --- refinePerRungAdmission --------------------------------------------------

func TestRefinePerRungAdmission_NilLearnerIsANoop(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	decision, routed := e.refinePerRungAdmission(perRungTestPlan(), perRungTestDecision(), true)
	if !routed || decision.Reason != solver.ReasonRouted || decision.K != 8 {
		t.Fatalf("nil PerRungAdmission must leave the decision byte-unchanged, got routed=%v reason=%q k=%d",
			routed, decision.Reason, decision.K)
	}
}

func TestRefinePerRungAdmission_OnlyTouchesPerRungPredictiveRoutes(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	key := perRungTestKey()
	l.Observe(key, 1, perRungTestNAnchors)
	l.Observe(key, 1, perRungTestNAnchors)
	if !l.ShouldDeclineBypass(key) {
		t.Fatal("fixture setup: expected decline-worthy evidence")
	}
	e := &Engine{PerRungAdmission: l}

	ordinary := perRungTestDecision()
	ordinary.PerRungPredictive = false // an ordinary MinFanout/MinAnchorPairs clearance
	decision, routed := e.refinePerRungAdmission(perRungTestPlan(), ordinary, true)
	if !routed || decision.Reason != solver.ReasonRouted {
		t.Fatalf("a non-PerRungPredictive route must never be second-guessed by this refinement, "+
			"got routed=%v reason=%q", routed, decision.Reason)
	}
}

func TestRefinePerRungAdmission_DowngradesOnceEvidenceSaysCheap(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	key := perRungTestKey()
	l.Observe(key, 1, perRungTestNAnchors)
	l.Observe(key, 1, perRungTestNAnchors)
	if !l.ShouldDeclineBypass(key) {
		t.Fatal("fixture setup: expected decline-worthy evidence")
	}
	e := &Engine{PerRungAdmission: l}

	decision, routed := e.refinePerRungAdmission(perRungTestPlan(), perRungTestDecision(), true)
	if routed {
		t.Fatal("evidence-declined per-rung route must not dispatch route B")
	}
	if decision.Strategy != "" || decision.K != 0 || decision.Slices != nil {
		t.Errorf("declined decision must clear Strategy/K/Slices, got Strategy=%q K=%d Slices=%v",
			decision.Strategy, decision.K, decision.Slices)
	}
	if decision.Reason != solver.ReasonBelowThreshold {
		t.Errorf("declined decision reason = %q, want %q", decision.Reason, solver.ReasonBelowThreshold)
	}
	if decision.PerRungPredictive {
		t.Error("declined decision must clear PerRungPredictive — it no longer describes a route")
	}
	if decision.NAnchors != perRungTestNAnchors {
		t.Error("declined decision must keep the cost-grid readout (NAnchors) for the shadow header/corpus")
	}
}

// --- wrapPerRungObserver / perRungObservingCursor ---------------------------

// perRungFakeCursor is a minimal chclient.Cursor whose Err()/Inspected() are
// fixed at construction, mirroring route_memo_wiring_test.go's own
// fakeMemoWiringCursor fixture style.
type perRungFakeCursor struct {
	err       error
	inspected int64
	closes    int
}

func (c *perRungFakeCursor) Next() bool              { return false }
func (c *perRungFakeCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (c *perRungFakeCursor) Err() error              { return c.err }
func (c *perRungFakeCursor) Inspected() int64        { return c.inspected }
func (c *perRungFakeCursor) Close() error {
	c.closes++
	return nil
}

func TestWrapPerRungObserver_PassesThroughWhenNotApplicable(t *testing.T) {
	t.Parallel()
	fake := &perRungFakeCursor{}

	if got := wrapPerRungObserver(fake, nil, nil, perRungTestPlan(), perRungTestDecision()); got != chclient.Cursor(fake) {
		t.Error("a nil learner must return the cursor unchanged")
	}

	notPerRung := perRungTestDecision()
	notPerRung.PerRungPredictive = false
	l := NewPerRungAdmissionLearner()
	if got := wrapPerRungObserver(fake, l, nil, perRungTestPlan(), notPerRung); got != chclient.Cursor(fake) {
		t.Error("a non-PerRungPredictive decision must return the cursor unchanged")
	}
}

func TestWrapPerRungObserver_RecordsOnlyOnACleanDrain(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	plan := perRungTestPlan()
	decision := perRungTestDecision()

	// A cancelled/errored drain with a SMALL Inspected() count must NOT be
	// recorded as cheap — exactly #2709's own reported symptom (a client
	// cancel after 19s), which would otherwise teach the learner the
	// opposite of the truth.
	cancelled := &perRungFakeCursor{err: errors.New("context canceled"), inspected: 1}
	wrapped := wrapPerRungObserver(cancelled, l, nil, plan, decision)
	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close returned an unexpected error: %v", err)
	}
	if l.ShouldDeclineBypass(perRungTestKey()) {
		t.Fatal("a cancelled drain must never contribute evidence, clean or not")
	}

	// Two SEPARATE clean, cheap dispatches (two distinct wrapped cursors,
	// mirroring two distinct requests) accumulate toward the decline.
	for i := 0; i < perRungEvidenceMinObservations; i++ {
		clean := &perRungFakeCursor{inspected: 1}
		wrapped := wrapPerRungObserver(clean, l, nil, plan, decision)
		if err := wrapped.Close(); err != nil {
			t.Fatalf("Close returned an unexpected error: %v", err)
		}
	}
	if !l.ShouldDeclineBypass(perRungTestKey()) {
		t.Fatal("two clean, cheap drains did not accumulate evidence")
	}
}

// --- issue #3034: declined shapes gain a reachable relax path --------------

// TestPerRungAdmission_DeclinedShapeRelaxesOnceEvidenceGoesStale is the
// end-to-end regression test for issue #3034: a shape that declines the
// per-rung bypass must gain a REAL, evidence-driven path back to
// eligibility — not merely a TTL entry that some other mechanism keeps
// renewing forever. This declines a shape with two real, clean Observe()
// calls (exactly what wrapPerRungObserver feeds from a real per-rung
// dispatch), confirms refinePerRungAdmission downgrades it, ages the
// evidence past perRungEvidenceTTL with NO further calls in between
// (mirroring perRungEvidenceTTL passing with no seed traffic), and confirms
// the SAME Decision — a fresh PerRungPredictive=true prediction, exactly
// what the solver recomputes on every request independent of the learner's
// own state — now reaches dispatch un-declined, which is what lets
// wrapPerRungObserver wrap a real cursor and call a real Observe() again.
func TestPerRungAdmission_DeclinedShapeRelaxesOnceEvidenceGoesStale(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	plan := perRungTestPlan()
	key := perRungTestKey()

	l.Observe(key, 1, perRungTestNAnchors)
	l.Observe(key, 1, perRungTestNAnchors)
	if !l.ShouldDeclineBypass(key) {
		t.Fatal("fixture setup: expected decline-worthy evidence")
	}

	e := &Engine{PerRungAdmission: l}
	decision := perRungTestDecision()
	if refined, routed := e.refinePerRungAdmission(plan, decision, true); routed || refined.PerRungPredictive {
		t.Fatal("fixture setup: expected the declined shape to route A before aging the evidence")
	}

	// Age the evidence past perRungEvidenceTTL with NO seed/Observe calls in
	// between — same-package direct field access, exactly like this file's
	// own TestPerRungAdmissionLearner_StaleEvidenceIsIgnored.
	l.mu.Lock()
	l.states[key].lastObserved = time.Now().Add(-perRungEvidenceTTL - time.Minute)
	l.mu.Unlock()

	if l.ShouldDeclineBypass(key) {
		t.Fatal("evidence past perRungEvidenceTTL must relax the decline")
	}

	// The solver computes PerRungPredictive fresh on every request
	// (refinePerRungAdmission's own doc), so the next request for this
	// shape presents the SAME un-declined Decision again — confirm it now
	// passes through un-declined, reaching wrapPerRungObserver for a real
	// Observe() on the next dispatch.
	refined, routed := e.refinePerRungAdmission(plan, perRungTestDecision(), true)
	if !routed || !refined.PerRungPredictive {
		t.Fatal("a relaxed decline must let the solver's fresh PerRungPredictive prediction through un-declined")
	}

	// Confirm the relaxed decision genuinely reaches a real Observe() again:
	// wrapPerRungObserver must wrap (not pass through) this cursor, and a
	// clean drain through it must be able to move the streak off zero,
	// closing the self-confirming loop issue #3034 describes.
	clean := &perRungFakeCursor{inspected: 1}
	wrapped := wrapPerRungObserver(clean, l, nil, plan, refined)
	if wrapped == chclient.Cursor(clean) {
		t.Fatal("wrapPerRungObserver must wrap a relaxed, un-declined PerRungPredictive decision, not pass it through")
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close returned an unexpected error: %v", err)
	}
	l.mu.Lock()
	streak := l.states[key].consecutiveCheap
	l.mu.Unlock()
	if streak == 0 {
		t.Fatal("a real clean drain through the relaxed decision did not reach Observe()")
	}
}

func TestWrapPerRungObserver_DoubleCloseRecordsOnce(t *testing.T) {
	t.Parallel()
	l := NewPerRungAdmissionLearner()
	plan := perRungTestPlan()
	decision := perRungTestDecision()
	key := routememo.KeyFor(plan, decision.NAnchors, decision.Fanout, decision.Step)

	clean := &perRungFakeCursor{inspected: 1}
	wrapped := wrapPerRungObserver(clean, l, nil, plan, decision)
	_ = wrapped.Close()
	_ = wrapped.Close()

	if clean.closes != 2 {
		t.Fatalf("fixture: expected the underlying cursor's own Close to be called twice, got %d", clean.closes)
	}
	l.mu.Lock()
	streak := l.states[key].consecutiveCheap
	l.mu.Unlock()
	if streak != 1 {
		t.Fatalf("double Close recorded %d observations, want exactly 1 (sync.Once must guard it)", streak)
	}
}
