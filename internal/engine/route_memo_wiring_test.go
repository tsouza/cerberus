package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// --- fixtures shared by every test in this file --------------------------

var (
	memoWiringGridStart = time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)
	memoWiringGridStep  = 15 * time.Second
	memoWiringGridOuter = time.Hour
	memoWiringGridEnd   = memoWiringGridStart.Add(memoWiringGridOuter)
)

// memoWiringEligiblePlan builds an eligible matrix RangeWindow(rate) over a
// Scan, pinned to the grid above — the same shape family
// solver_wiring_test.go's matrixPlan uses, with a real anchor count so
// Eligible produces K >= 2 (241 anchors / MinAnchorsPerSlice=30 -> K=8 under
// the default config).
func memoWiringEligiblePlan() chplan.Node {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            memoWiringGridStep,
		OuterRange:      memoWiringGridOuter,
		Start:           memoWiringGridStart,
		End:             memoWiringGridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// memoWiringNotRoutedDecision is the "seed" decision an initial classify()
// pass would have produced for memoWiringEligiblePlan under a config whose
// ModeAuto thresholds it does not clear (below-threshold) — it carries real
// NAnchors/Fanout/Step (populated for every decision, routed or not, per
// decision.go) so KeyFor has real cost-grid inputs to bucket.
func memoWiringNotRoutedDecision(t *testing.T) *solver.Decision {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	cfg.MinFanout = 1_000_000 // impossibly high — guarantees below-threshold
	p := &solver.Planner{Cfg: cfg}
	meta := solver.RequestMeta{Lang: solver.LangPromQL, Start: memoWiringGridStart, End: memoWiringGridEnd, Step: memoWiringGridStep}
	d, routed := p.Plan(memoWiringEligiblePlan(), meta)
	if routed {
		t.Fatalf("fixture must be below-threshold, not routed — got reason=%q", d.Reason)
	}
	if d.NAnchors == 0 {
		t.Fatalf("fixture decision carries no cost grid — NAnchors=0")
	}
	return d
}

// fakeSolverCursorClient is a minimal solver.CursorQuerier: every QueryCursor
// call either succeeds with a fixed row count or fails with a configured
// error. opens counts dispatches so a test can assert the Executor was (or
// was not) invoked.
type fakeSolverCursorClient struct {
	err            error
	rows           int
	maxMemoryBytes int64
	// opens is written concurrently — Executor.Execute dispatches K shards
	// in parallel goroutines, each calling QueryCursor on this SAME fake —
	// so it must be atomic, not a plain int (confirmed by -race: the first
	// version of this fake raced here).
	opens atomic.Int64
}

func (f *fakeSolverCursorClient) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	f.opens.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &fakeMemoWiringCursor{remaining: f.rows}, nil
}

func (f *fakeSolverCursorClient) MaxQueryMemoryBytes() int64 { return f.maxMemoryBytes }

// fakeMemoWiringCursor streams `remaining` empty-valued rows then stops.
type fakeMemoWiringCursor struct{ remaining int }

func (c *fakeMemoWiringCursor) Next() bool {
	if c.remaining <= 0 {
		return false
	}
	c.remaining--
	return true
}
func (c *fakeMemoWiringCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (c *fakeMemoWiringCursor) Err() error              { return nil }
func (c *fakeMemoWiringCursor) Close() error            { return nil }
func (c *fakeMemoWiringCursor) Inspected() int64        { return 0 }

// fakeSolverEmitter always succeeds with a fixed, valid (no now64) SQL
// string — the Executor's now64 belt-and-braces guard must not fire.
type fakeSolverEmitter struct{}

func (fakeSolverEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "SELECT 1", nil, nil
}

// alwaysClosedBreaker satisfies solver's package-local breakerPeeker via
// the Executor's exported field type — reuse Solver's own BreakerClosed
// zero-value contract instead (nil Executor.Breaker => BreakerClosed()
// reports true), so no fake is needed for the common case; a dedicated
// open-breaker fake is defined where a test needs one.

// newMemoWiringEngine builds an Engine wired end-to-end: a ModeAuto Solver
// backed by cursorClient, a fresh routememo.Memo, ready for tryRouteMemoHit
// / retryOnRouteAResourceFailure to dispatch through.
func newMemoWiringEngine(t *testing.T, cursorClient solver.CursorQuerier) (*Engine, *routememo.Memo) {
	t.Helper()
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	if err := cfg.Validate(); err != nil {
		t.Fatalf("solver config invalid: %v", err)
	}
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cursorClient})
	memo := routememo.New(time.Minute)
	return &Engine{Solver: sv, RouteMemo: memo}, memo
}

// --- tests -----------------------------------------------------------------

// TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict pins the core memo-hit
// contract: a live (non-stale) PreferB verdict routes B directly, without
// the caller ever touching route A.
func TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	// Establish the key exactly as the wiring itself would derive it, and
	// seed a live PreferB verdict — the state a prior successful probe or
	// retry would have left behind.
	d := eng.deriveRouteMemoDispatch(plan, seed, memoWiringGridEnd.Add(2*memoWiringGridStep))
	if !d.eligible {
		t.Fatalf("fixture plan must be structurally eligible")
	}
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, info, usedDecision, gotKey, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if !ok {
		t.Fatal("tryRouteMemoHit did not dispatch on a live PreferB verdict")
	}
	if gotKey != d.key {
		t.Errorf("returned key = %+v, want %+v", gotKey, d.key)
	}
	if cur == nil || info == nil || usedDecision == nil {
		t.Fatalf("dispatched=true but cur=%v info=%v usedDecision=%v", cur, info, usedDecision)
	}
	if usedDecision.K < 2 {
		t.Errorf("usedDecision.K = %d, want >= 2 (the FRESHLY derived eligible decision, not the not-routed seed)", usedDecision.K)
	}
	if cq.opens.Load() < 1 {
		// Execute opens one cursor PER SHARD (kEff of them, not one per
		// dispatch) — the exact count depends on the decision's K clamped
		// by Cfg.Parallel/MaxK, which this test does not pin. >=1 is the
		// meaningful assertion: a dispatch actually happened.
		t.Errorf("Executor cursor opens = %d, want >= 1 (a dispatch must have happened)", cq.opens.Load())
	}
}

// TestTryRouteMemoHit_UnknownKeyNeverDispatches pins the negative case: a
// key with no recorded verdict (the common case — every new query shape)
// must never dispatch route B via the memo-hit path.
func TestTryRouteMemoHit_UnknownKeyNeverDispatches(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	_, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit dispatched for an Unknown key — must never memo-hit without a recorded verdict")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 (no dispatch should have been attempted)", cq.opens.Load())
	}
}

// TestTryRouteMemoHit_StaleVerdictFallsBackToCaller pins Major-5's
// re-validation contract: a PreferB verdict that has crossed its
// re-validation midpoint is stale — Lookup reports it, but the memo-hit
// path must NOT act on it; the caller falls through to its own route-A
// dispatch.
func TestTryRouteMemoHit_StaleVerdictFallsBackToCaller(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 3}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	memo.SetNowForTest(func() time.Time { return fixedNow })
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	// Advance past the re-validation midpoint (memoEntryTTL/2) without
	// crossing the TTL itself.
	memo.SetNowForTest(func() time.Time { return fixedNow.Add(20 * time.Minute) })

	_, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit dispatched on a STALE PreferB verdict — must fall through to route A instead")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// fakeFailingSolverEmitter always fails Emit — models a pre-flight failure
// Executor.Execute CAN detect synchronously (unlike a per-shard QueryCursor
// open error, which happens inside an async runShard goroutine and only
// surfaces later, from the CALLER's drain of the returned cursor).
type fakeFailingSolverEmitter struct{ err error }

func (f fakeFailingSolverEmitter) Emit(context.Context, chplan.Node) (string, []any, error) {
	return "", nil, f.err
}

// TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA pins the half of
// the symmetric fallback (Major-2) tryRouteMemoHit can decide on its own: a
// PRE-FLIGHT failure (emit/breaker/gate/now64 — the only failure classes
// Executor.Execute's synchronous return can ever carry, see
// classifyRouteOutcome's doc) must report dispatched=false so the caller
// falls through to its own safe route-A path. A per-shard resource failure
// is a DIFFERENT case: Execute opens a streaming cursor asynchronously, so
// that failure only surfaces once the CALLER drains it — which is exactly
// why QueryPlanCursor attaches its own Retry closure to a memo-hit's
// CursorResult rather than expecting tryRouteMemoHit to detect it
// synchronously; that closure is covered at the QueryPlanCursor level.
func TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	sv := solver.New(cfg, fakeFailingSolverEmitter{err: errors.New("emit boom")}, solver.ExecDeps{Client: cq})
	memo := routememo.New(time.Minute)
	eng := &Engine{Solver: sv, RouteMemo: memo}
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, info, usedDecision, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if ok {
		t.Fatal("tryRouteMemoHit reported dispatched=true despite a pre-flight emit failure")
	}
	if cur != nil || info != nil || usedDecision != nil {
		t.Errorf("dispatched=false but returned non-nil values: cur=%v info=%v usedDecision=%v", cur, info, usedDecision)
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 — emit fails before any shard is dispatched", cq.opens.Load())
	}
	// A pre-flight failure classifies NoEvidence (never ResourceFailure —
	// see classifyRouteOutcome's doc), so Observe is correctly a no-op: the
	// pre-existing PreferB verdict must be UNCHANGED, not flipped to
	// BothFail on evidence that carries no information about resource use.
	state, stale := memo.Lookup(d.key)
	if state != routememo.PreferB || stale {
		t.Errorf("verdict after a NoEvidence-class pre-flight failure = (%v, stale=%v), want unchanged (PreferB, stale=false)", state, stale)
	}
}

// TestTryRouteMemoHit_MidDrainResourceFailure_ObserveViaClassify pins the
// OTHER half of Major-2: a per-shard resource failure that only surfaces
// once the returned cursor is DRAINED (Execute's own synchronous return
// cannot see it — see the doc on TestTryRouteMemoHit_PreFlightFailureFallsBackToRouteA)
// still ends up correctly classified and recorded once the caller reports
// it — exercising the exact classify-then-Observe sequence QueryPlanCursor's
// Retry closure runs on a memo-hit's drain failure, using the real
// classifyRouteOutcome + Memo.Observe this engine wires together (not a
// re-implementation of them).
func TestTryRouteMemoHit_MidDrainResourceFailure_ObserveViaClassify(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{err: chclient.ErrMemoryLimitExceeded}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	fixedNow := memoWiringGridEnd.Add(2 * memoWiringGridStep)
	d := eng.deriveRouteMemoDispatch(plan, seed, fixedNow)
	memo.Observe(d.key, routememo.RouteB, routememo.OutcomeSuccess)

	cur, _, _, gotKey, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil)
	if !ok {
		t.Fatal("tryRouteMemoHit must dispatch (Execute's synchronous return has no visibility into a per-shard open error)")
	}
	// Drain the cursor exactly as the real handler would — this is where
	// the shard's QueryCursor error actually surfaces.
	for cur.Next() {
	}
	drainErr := cur.Err()
	if drainErr == nil {
		t.Fatal("fixture must produce a drain error (fakeSolverCursorClient.err is set)")
	}
	// This is what QueryPlanCursor's memo-hit Retry closure does on a drain
	// failure: classify the real error and Observe it.
	memo.Observe(gotKey, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, drainErr))

	state, _ := memo.Lookup(gotKey)
	if state != routememo.BothFail {
		t.Errorf("verdict after the drained route-B resource failure = %v, want BothFail", state)
	}
}

// TestRetryOnRouteAResourceFailure_ProbesAfterCorroboration pins the A->B
// retry's core contract: a route-A resource failure alone is NOT enough —
// it takes minCorroboratingFailures consecutive failures with no
// intervening success before a probe (retry) is admitted.
func TestRetryOnRouteAResourceFailure_ProbesAfterCorroboration(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, memo := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)
	ctx := context.Background()

	// First resource failure: corroboration=1, below the threshold — must
	// NOT retry.
	_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if retried {
		t.Fatal("retried on the FIRST route-A resource failure — corroboration requires more than one")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens after 1st failure = %d, want 0", cq.opens.Load())
	}

	// Second consecutive resource failure, same key, no intervening
	// success: corroboration=2 (the default minCorroboratingFailures) —
	// NOW a probe is admitted and route B dispatches.
	cur, info, usedDecision, observeFn, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if !retried {
		t.Fatal("did not retry on the 2nd consecutive route-A resource failure")
	}
	if cur == nil || info == nil || usedDecision == nil || observeFn == nil {
		t.Fatalf("retried=true but cur=%v info=%v usedDecision=%v observeFn-is-nil=%v", cur, info, usedDecision, observeFn == nil)
	}
	if cq.opens.Load() < 1 {
		// Execute opens one cursor PER SHARD, not one per dispatch (see the
		// comment in TestTryRouteMemoHit_DispatchesOnLivePreferBVerdict).
		t.Errorf("Executor cursor opens after 2nd failure = %d, want >= 1 (a retry dispatch must have happened)", cq.opens.Load())
	}

	// The probe's SUCCESS is what creates the memo's first verdict for this
	// Key (Memo.Observe's OutcomeSuccess branch) — nothing else does. Before
	// the caller reports the drain outcome, the Key must still be Unknown;
	// calling observeFn(nil) (a clean drain) is what must flip it to
	// PreferB. This is the exact mechanism "a query that failed gets
	// retried on route B, and the decision is remembered" the whole
	// failure-driven route memo exists to provide.
	d := eng.deriveRouteMemoDispatch(plan, seed, time.Now())
	if state, _ := memo.Lookup(d.key); state != routememo.Unknown {
		t.Fatalf("verdict BEFORE observeFn is called = %v, want Unknown (nothing should be recorded from Execute's return alone)", state)
	}
	observeFn(nil) // the caller's actual drain succeeded cleanly
	state, stale := memo.Lookup(d.key)
	if state != routememo.PreferB || stale {
		t.Errorf("verdict AFTER observeFn(nil) = (%v, stale=%v), want (PreferB, stale=false)", state, stale)
	}
}

// TestRetryOnRouteAResourceFailure_NonResourceErrorNeverRetried pins
// Major-6: a route-A failure classified as anything OTHER than a resource
// failure (a timeout, a broken connection, a cancellation) must never
// trigger a retry dispatch, regardless of corroboration state.
func TestRetryOnRouteAResourceFailure_NonResourceErrorNeverRetried(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)
	ctx := context.Background()

	timeoutErr := &solver.SolverTimeoutError{Timeout: "60s"}
	for i := 0; i < 5; i++ {
		_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, timeoutErr)
		if retried {
			t.Fatalf("iteration %d: retried on a NoEvidence-class error (solver timeout)", i)
		}
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0 — no dispatch should ever have been attempted", cq.opens.Load())
	}
}

// TestRetryOnRouteAResourceFailure_ClientGoneNeverRetried pins the
// client-disconnect guard: a cancelled request context must never trigger
// a retry dispatch for a caller that is no longer there to receive it.
func TestRetryOnRouteAResourceFailure_ClientGoneNeverRetried(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	eng, _ := newMemoWiringEngine(t, cq)
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, _, retried := eng.retryOnRouteAResourceFailure(ctx, "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded)
	if retried {
		t.Fatal("retried on a cancelled context — no client is there to receive the retry's answer")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// TestRouteMemoWiring_InactiveWhenRouteMemoNil pins the off-by-default
// contract: with RouteMemo nil (the zero value — no cmd/cerberus wiring),
// neither function ever dispatches, regardless of plan/decision/error.
func TestRouteMemoWiring_InactiveWhenRouteMemoNil(t *testing.T) {
	t.Parallel()
	cq := &fakeSolverCursorClient{rows: 2}
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	sv := solver.New(cfg, fakeSolverEmitter{}, solver.ExecDeps{Client: cq})
	eng := &Engine{Solver: sv} // RouteMemo left nil
	plan := memoWiringEligiblePlan()
	seed := memoWiringNotRoutedDecision(t)

	if _, _, _, _, ok := eng.tryRouteMemoHit(context.Background(), "promql", plan, seed, nil); ok {
		t.Fatal("tryRouteMemoHit dispatched with RouteMemo nil")
	}
	if _, _, _, _, ok := eng.retryOnRouteAResourceFailure(context.Background(), "promql", plan, seed, nil, chclient.ErrMemoryLimitExceeded); ok {
		t.Fatal("retryOnRouteAResourceFailure dispatched with RouteMemo nil")
	}
	if cq.opens.Load() != 0 {
		t.Errorf("Executor cursor opens = %d, want 0", cq.opens.Load())
	}
}

// TestFreshEnoughForRouteMemo pins the live-edge freshness gate's exact
// boundary: End must have aged past liveEdgeFreshnessMarginSteps*Step as of
// now, not merely be in the past.
func TestFreshEnoughForRouteMemo(t *testing.T) {
	t.Parallel()
	step := 15 * time.Second
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		end  time.Time
		now  time.Time
		want bool
	}{
		{"end exactly at now: live edge, not fresh", base, base, false},
		{"end one step before now: exactly at the margin, fresh", base, base.Add(step), true},
		{"end well in the past: fresh", base, base.Add(time.Hour), true},
		{"end in the future (clock skew / clamp bug): not fresh", base, base.Add(-time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := freshEnoughForRouteMemo(tc.end, step, tc.now)
			if got != tc.want {
				t.Errorf("freshEnoughForRouteMemo(end=%v, step=%v, now=%v) = %v, want %v", tc.end, step, tc.now, got, tc.want)
			}
		})
	}
}

// TestFreshEnoughForRouteMemo_ZeroStepNeverFresh pins a defensive edge: a
// zero step (should not occur for a plan that passed Eligible, but a
// dispatch-site bug must fail closed, not divide/multiply into a bogus
// margin).
func TestFreshEnoughForRouteMemo_ZeroStepNeverFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if freshEnoughForRouteMemo(now.Add(-time.Hour), 0, now) {
		t.Fatal("freshEnoughForRouteMemo reported fresh with step=0 — must fail closed")
	}
}
