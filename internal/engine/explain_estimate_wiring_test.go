package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- fixtures ---------------------------------------------------------------

// estimateTestPlan is a minimal, stable chplan.Node — routememo.KeyFor only
// needs a walkable tree, and every test in this file uses the SAME plan/N/F/
// Step so its Key is stable across calls, mirroring per_rung_admission_test.go's
// own fixture convention.
func estimateTestPlan() chplan.Node {
	return &chplan.Aggregate{
		Input:    &chplan.Scan{Table: "otel_metrics_sum"},
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "anchor_ts"}},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

const estimateTestNAnchors = 241

func estimateTestBaseline(reason string) *solver.Decision {
	return &solver.Decision{
		Reason:   reason,
		NAnchors: estimateTestNAnchors,
		Fanout:   20,
		Step:     15 * time.Second,
	}
}

func estimateTestKey() routememo.Key {
	d := estimateTestBaseline(solver.ReasonRouted)
	return shapeKey(estimateTestPlan(), d)
}

// countingEstimator is a fake Estimator that counts every ExplainEstimate
// call and returns a fixed result (or a fixed error).
type countingEstimator struct {
	calls    int
	estimate chclient.ScanEstimate
	err      error
}

func (c *countingEstimator) ExplainEstimate(_ context.Context, _ string, _ ...any) (chclient.ScanEstimate, error) {
	c.calls++
	return c.estimate, c.err
}

// noopEmit is an emit closure that renders a fixed, non-empty SQL string
// with no error — the fixture value every test in this file that reaches
// the round trip uses.
func noopEmit(_ context.Context, _ Lang, _ chplan.Node) (string, []any, error) {
	return "SELECT 1", nil, nil
}

// --- reachedCostGrid ---------------------------------------------------------

func TestReachedCostGrid(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		solver.ReasonRouted:                true,
		solver.ReasonBelowThreshold:        true,
		solver.ReasonAnchorGridIndivisible: true,
		solver.ReasonHighD:                 true,
		solver.ReasonIncommensurate:        true,
		solver.ReasonNow64:                 false,
		solver.ReasonNotSliceable:          false,
		solver.ReasonInstant:               false,
		solver.ReasonInstantJoin:           false,
		solver.ReasonGridMismatch:          false,
		solver.ReasonScalarHeavy:           false,
		solver.ReasonRoutingDisabled:       false,
		solver.ReasonExtractionFailed:      false,
	}
	for reason, want := range cases {
		if got := reachedCostGrid(reason); got != want {
			t.Errorf("reachedCostGrid(%q) = %v, want %v", reason, got, want)
		}
	}
}

// --- ScanEstimateAdvisor.Advise ---------------------------------------------

// TestScanEstimateAdvisor_SkipsSecondProbeForSameShape is the pinned proof of
// issue #2787's own cost constraint: a second identical-shape request must
// NOT re-issue the EXPLAIN ESTIMATE round trip.
func TestScanEstimateAdvisor_SkipsSecondProbeForSameShape(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 500_000, Marks: 61, Parts: 1}}
	a := NewScanEstimateAdvisor(est, nil)
	plan := estimateTestPlan()
	baseline := estimateTestBaseline(solver.ReasonRouted)

	first := a.Advise(context.Background(), nil, plan, routedCorpusLang{}, baseline, noopEmit)
	if first == nil || first.Rows != 500_000 {
		t.Fatalf("first probe: got %+v, want Rows=500000", first)
	}
	if est.calls != 1 {
		t.Fatalf("first probe issued %d round trips, want 1", est.calls)
	}

	second := a.Advise(context.Background(), nil, plan, routedCorpusLang{}, baseline, noopEmit)
	if second == nil || second.Rows != 500_000 {
		t.Fatalf("second probe (cached): got %+v, want Rows=500000", second)
	}
	if est.calls != 1 {
		t.Fatalf("second probe for the SAME shape issued %d round trips, want 1 (cache hit)", est.calls)
	}
}

// TestScanEstimateAdvisor_SkipsStructurallyIneligiblePlan pins narrowing (1):
// a baseline classification that never reached the cost-grid section must
// never spend a round trip.
func TestScanEstimateAdvisor_SkipsStructurallyIneligiblePlan(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 1}}
	a := NewScanEstimateAdvisor(est, nil)
	baseline := estimateTestBaseline(solver.ReasonNow64)

	got := a.Advise(context.Background(), nil, estimateTestPlan(), routedCorpusLang{}, baseline, noopEmit)
	if got != nil {
		t.Fatalf("got %+v, want nil (structurally ineligible)", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips for a structurally ineligible plan, want 0", est.calls)
	}
}

// TestScanEstimateAdvisor_SkipsWhenRouteMemoHasVerdict pins narrowing (3) for
// the route memo half: a Key the memo has already resolved (PreferB, via a
// real corroborated probe) needs no additional advisory signal.
func TestScanEstimateAdvisor_SkipsWhenRouteMemoHasVerdict(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 1}}
	a := NewScanEstimateAdvisor(est, nil)
	plan := estimateTestPlan()
	baseline := estimateTestBaseline(solver.ReasonRouted)
	key := estimateTestKey()

	memo := routememo.New(time.Hour)
	memo.Observe(key, routememo.RouteA, routememo.OutcomeResourceFailure)
	memo.Observe(key, routememo.RouteA, routememo.OutcomeResourceFailure)
	release, ok := memo.BeginProbe(key)
	if !ok {
		t.Fatalf("BeginProbe declined admission for a corroborated key")
	}
	memo.Observe(key, routememo.RouteB, routememo.OutcomeSuccess)
	release()
	if state, _ := memo.Lookup(key); state != routememo.PreferB {
		t.Fatalf("fixture failed to establish PreferB; got %v", state)
	}

	got := a.Advise(context.Background(), memo, plan, routedCorpusLang{}, baseline, noopEmit)
	if got != nil {
		t.Fatalf("got %+v, want nil (route memo already holds a verdict)", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips despite an existing route memo verdict, want 0", est.calls)
	}
}

// TestScanEstimateAdvisor_SkipsWhenPerRungAdmissionHasVerdict pins narrowing
// (3) for the per-rung admission half.
func TestScanEstimateAdvisor_SkipsWhenPerRungAdmissionHasVerdict(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 1}}
	learner := NewPerRungAdmissionLearner()
	a := NewScanEstimateAdvisor(est, learner)
	plan := estimateTestPlan()
	baseline := estimateTestBaseline(solver.ReasonRouted)
	key := estimateTestKey()

	learner.Observe(key, 10, estimateTestNAnchors) // any real observation seeds a fresh entry

	got := a.Advise(context.Background(), nil, plan, routedCorpusLang{}, baseline, noopEmit)
	if got != nil {
		t.Fatalf("got %+v, want nil (per-rung admission already holds a verdict)", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips despite an existing per-rung admission entry, want 0", est.calls)
	}
}

// TestScanEstimateAdvisor_ProbeFailureIsAdvisoryNil pins the fail-open
// contract: a round-trip error must never surface as anything other than
// "no estimate".
func TestScanEstimateAdvisor_ProbeFailureIsAdvisoryNil(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{err: errors.New("boom")}
	a := NewScanEstimateAdvisor(est, nil)
	baseline := estimateTestBaseline(solver.ReasonRouted)

	got := a.Advise(context.Background(), nil, estimateTestPlan(), routedCorpusLang{}, baseline, noopEmit)
	if got != nil {
		t.Fatalf("got %+v, want nil (probe failure must fail open)", got)
	}
	if est.calls != 1 {
		t.Fatalf("probe called %d times, want exactly 1 (a failed probe is not cached, but this call happens once)", est.calls)
	}
}

// TestScanEstimateAdvisor_EmitFailureIsAdvisoryNil mirrors the probe-failure
// test for an emission error — no round trip is even attempted.
func TestScanEstimateAdvisor_EmitFailureIsAdvisoryNil(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 1}}
	a := NewScanEstimateAdvisor(est, nil)
	baseline := estimateTestBaseline(solver.ReasonRouted)
	failingEmit := func(_ context.Context, _ Lang, _ chplan.Node) (string, []any, error) {
		return "", nil, errors.New("emit boom")
	}

	got := a.Advise(context.Background(), nil, estimateTestPlan(), routedCorpusLang{}, baseline, failingEmit)
	if got != nil {
		t.Fatalf("got %+v, want nil (emit failure must fail open)", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips despite an emit failure, want 0", est.calls)
	}
}

// TestScanEstimateAdvisor_NilAdvisorAndNilBaseline pin the two byte-unchanged
// guards: a nil *ScanEstimateAdvisor and a nil baseline Decision must both
// return nil without touching the Estimator.
func TestScanEstimateAdvisor_NilAdvisorAndNilBaseline(t *testing.T) {
	t.Parallel()
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 1}}

	var nilAdvisor *ScanEstimateAdvisor
	if got := nilAdvisor.Advise(context.Background(), nil, estimateTestPlan(), routedCorpusLang{}, estimateTestBaseline(solver.ReasonRouted), noopEmit); got != nil {
		t.Fatalf("nil advisor: got %+v, want nil", got)
	}

	a := NewScanEstimateAdvisor(est, nil)
	if got := a.Advise(context.Background(), nil, estimateTestPlan(), routedCorpusLang{}, nil, noopEmit); got != nil {
		t.Fatalf("nil baseline: got %+v, want nil", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips for a nil advisor/baseline, want 0", est.calls)
	}
}

// TestScanEstimateAdvisor_SeedsPerRungPriorOnNearEmpty pins issue #2787's
// per-rung prior-seeding path: a near-empty advisory estimate (output rows
// below perRungCheapRowsPerAnchor*N, mirroring PerRungAdmissionLearner.
// Observe's own comparison) seeds the SAME learner's rolling evidence, so a
// LATER real per-rung admission check sees a fresh entry without waiting for
// perRungEvidenceMinObservations real dispatches.
func TestScanEstimateAdvisor_SeedsPerRungPriorOnNearEmpty(t *testing.T) {
	t.Parallel()
	// Well under estimateTestNAnchors(241) * perRungCheapRowsPerAnchor(20).
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 100}}
	learner := NewPerRungAdmissionLearner()
	a := NewScanEstimateAdvisor(est, learner)
	plan := estimateTestPlan()
	baseline := estimateTestBaseline(solver.ReasonRouted)
	key := estimateTestKey()

	if a.Advise(context.Background(), nil, plan, routedCorpusLang{}, baseline, noopEmit) == nil {
		t.Fatal("expected a non-nil advisory estimate")
	}
	if !learner.hasFreshEntry(key) {
		t.Fatal("a near-empty advisory estimate did not seed the per-rung admission learner")
	}
}

// TestScanEstimateAdvisor_DenseEstimateDoesNotResetPerRungPrior pins the
// one-directional-seeding fix: a DENSE advisory estimate must never reset an
// already-accumulated real consecutiveCheap count. est.Rows is a scan-side
// quantity and Observe's own outputRows is a post-aggregation quantity — a
// large scan estimate says nothing reliable about composed-output
// cardinality, so treating it as cheap=false would discard real evidence
// from actual clean drains on an imprecise proxy (see
// maybeSeedPerRungPrior's own doc).
func TestScanEstimateAdvisor_DenseEstimateDoesNotResetPerRungPrior(t *testing.T) {
	t.Parallel()
	// Well OVER estimateTestNAnchors(241) * perRungCheapRowsPerAnchor(20).
	est := &countingEstimator{estimate: chclient.ScanEstimate{Rows: 10_000_000}}
	learner := NewPerRungAdmissionLearner()
	a := NewScanEstimateAdvisor(est, learner)
	plan := estimateTestPlan()
	baseline := estimateTestBaseline(solver.ReasonRouted)
	key := estimateTestKey()

	// Real accumulated evidence: one real clean-and-cheap drain, one
	// observation short of ShouldDeclineBypass's own threshold.
	learner.Observe(key, 10, estimateTestNAnchors)
	if learner.ShouldDeclineBypass(key) {
		t.Fatal("fixture: a single real observation must not already decline the bypass")
	}

	// The dense estimate itself skips seeding (narrowing (3): the learner
	// already holds a fresh entry for this key), so this also re-confirms
	// that guard fires before any reset could happen.
	if got := a.Advise(context.Background(), nil, plan, routedCorpusLang{}, baseline, noopEmit); got != nil {
		t.Fatalf("got %+v, want nil (per-rung admission already holds a fresh entry)", got)
	}
	if est.calls != 0 {
		t.Fatalf("issued %d round trips despite an existing per-rung admission entry, want 0", est.calls)
	}

	// Directly exercise maybeSeedPerRungPrior's own one-directional contract
	// (bypassing narrowing (3) so the dense-estimate path itself is pinned,
	// not only its precondition above).
	a.maybeSeedPerRungPrior(key, chclient.ScanEstimate{Rows: 10_000_000}, baseline)
	learner.Observe(key, 10, estimateTestNAnchors) // the second REAL cheap observation
	if !learner.ShouldDeclineBypass(key) {
		t.Fatal("a dense estimate reset the real accumulated evidence — two real cheap " +
			"observations must still decline the bypass")
	}
}
