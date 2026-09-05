package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// liveEdgeFreshnessMarginSteps is the failure-driven route memo's live-edge
// exception (docs §"Failure-driven route memo"). Route B's answer-
// equivalence proof holds only once every shard's window has closed with
// respect to the live ClickHouse table state — a shard reading a strictly
// newer snapshot than an earlier shard is a real, bounded exception the
// proof does not cover. The newest grid anchor is, by construction, still
// inside the write window of the step that produced it, so anchors
// strictly older than one step have crossed the write frontier for any
// step-aligned ingestion pattern — a grid-relative justification, not a
// wall-clock-duration guess. A request whose End has not aged past this
// margin is never routed by the memo mechanism at all — probe, retry, and
// memo-hit alike — regardless of what the memo has recorded for its Key.
const liveEdgeFreshnessMarginSteps = 1

// routeMemoTrivialMagnitudeRowsPerAnchor bounds "trivially small" for a
// stale-PreferB re-validation's tracked magnitude (routememo.Memo.
// MagnitudeFor), RELATIVE to the dispatch's own anchor count — mirroring
// per_rung_admission.go's perRungCheapRowsPerAnchor per-anchor cheapness
// reasoning (a composed route-B result averaging fewer than this many
// output rows per anchor has too little data behind it, at any grid width,
// to justify K-way contention). This is a SECOND, independent constant
// rather than a shared one: per_rung_admission.go's own doc is explicit
// that the per-rung learner and the failure-driven route memo are
// deliberately independent mechanisms answering different questions (one
// downgrades an already-eligible per-rung bypass; this one decides whether
// a SCARCE rescue-dispatch token is worth spending re-confirming a route
// memo verdict) — coupling their thresholds would make a future change to
// either one silently retune the other.
const routeMemoTrivialMagnitudeRowsPerAnchor = 20

// recordRouteMemoMagnitude feeds cur's inspected-row count into memo's
// magnitude EMA for key, on a CLEAN route-B drain only (callers gate on
// drainErr == nil / a nil dispatch error before calling this) — see
// routememo/magnitude.go's own doc. Guards the int64->uint64 conversion the
// same way per_rung_admission.go's perRungObservingCursor already does:
// Inspected() is documented non-negative on a clean drain, so the guard is
// defensive, not expected to ever trip. A nil cur or nil memo is a no-op —
// the former can't happen at either of this function's call sites but
// costs nothing to guard against, the latter mirrors every other memo
// method's own "unwired mechanism" contract.
func recordRouteMemoMagnitude(memo *routememo.Memo, key routememo.Key, cur chclient.Cursor) {
	if memo == nil || cur == nil {
		return
	}
	if n := cur.Inspected(); n >= 0 {
		memo.RecordActualMagnitude(key, uint64(n)) //nolint:gosec // G115 -- guarded by the >= 0 check above
	}
}

// routeMemoHitObserveDrainOutcome builds a memo-hit CursorResult's
// ObserveDrainOutcome hook (QueryPlanCursor's own call site) — a named,
// directly testable function rather than an inline closure, since the
// memo-hit's OTHER hook (Retry) already warranted one. The handler calls
// this UNCONDITIONALLY with the real drain error (nil on the ordinary
// clean finish), so a clean memo-hit drain — the SINGLE MOST common route-B
// dispatch shape once a Key is established — is what closes issue #3035's
// sampling-bias gap the widest: no verdict-state write happens (a clean
// memo-hit needs no re-confirmation, unchanged from before this hook
// existed), only the observational magnitude axis is fed.
func routeMemoHitObserveDrainOutcome(memo *routememo.Memo, key routememo.Key, cur chclient.Cursor) func(drainErr error) {
	return func(drainErr error) {
		if drainErr == nil {
			recordRouteMemoMagnitude(memo, key, cur)
		}
	}
}

// routeMemoActive reports whether the failure-driven route memo is wired
// and has something to consult. A nil RouteMemo or a nil Solver both mean
// the mechanism is off, matching e.classify's own "Solver nil -> no
// classification" contract — every function in this file is a no-op
// through this guard, so an engine that never sets RouteMemo is
// byte-unchanged.
func (e *Engine) routeMemoActive() bool {
	return e.RouteMemo != nil && e.Solver != nil
}

// freshEnoughForRouteMemo reports whether end has aged past the live-edge
// margin as of now.
func freshEnoughForRouteMemo(end time.Time, step time.Duration, now time.Time) bool {
	if step <= 0 {
		return false
	}
	return !end.After(now.Add(-time.Duration(liveEdgeFreshnessMarginSteps) * step))
}

// routeMemoDispatch bundles what every non-baseline dispatch site (probe,
// retry, memo-hit) needs, all re-derived at THIS call, independent of
// whatever an earlier classify() pass computed for a possibly-stale view
// (Minor-1): the literal-free routing Key, a freshly-eligible routed
// Decision (nil/false if not structurally eligible right now), and the
// live-edge freshness verdict.
type routeMemoDispatch struct {
	key      routememo.Key
	decision *solver.Decision
	eligible bool
	fresh    bool
}

// deriveRouteMemoDispatch re-derives eligibility, the Key, and the
// freshness gate for plan at dispatch time. seedDecision (the initial
// classify() pass, which DOES run once per request regardless of the memo)
// seeds nAnchors/fanout/step into the Key — the ELIGIBILITY verdict itself
// is freshly computed here on every call, never trusted from seedDecision.
func (e *Engine) deriveRouteMemoDispatch(plan chplan.Node, seedDecision *solver.Decision, now time.Time) routeMemoDispatch {
	start, end, step := solver.GridOf(plan)
	meta := solver.RequestMeta{Lang: solver.LangPromQL, Start: start, End: end, Step: step}
	key := routememo.KeyFor(plan, seedDecision.NAnchors, seedDecision.Fanout, seedDecision.Step)
	decision, eligible := e.Solver.Eligible(plan, meta)
	recordRouteMemoPressureActive(e.RouteMemo)
	return routeMemoDispatch{
		key:      key,
		decision: decision,
		eligible: eligible,
		fresh:    freshEnoughForRouteMemo(end, step, now),
	}
}

// routeMemoPressureState mirrors the failure-driven route memo's
// UnderPressure() level across calls so recordRouteMemoPressureActive can
// report a TRANSITION delta (telemetry.RecordRouteMemoPressureTransition)
// instead of a raw per-call snapshot, which would double-count every
// decision made while pressure stays at the same level. Package-level, not
// an Engine field: the underlying Memo is process-wide by contract
// (routeMemoActive's own "nil means off" gate covers the only Engine that
// matters per process), so one flag per process mirrors it exactly without
// widening Engine's own field set.
var routeMemoPressureState atomic.Bool

// recordRouteMemoPressureActive re-samples m.UnderPressure() at
// dispatch-decision time (deriveRouteMemoDispatch's own call site, common to
// both tryRouteMemoHit and retryOnRouteAResourceFailure) and reports ONLY a
// false->true / true->false transition on RouteMemoPressureActive. Uses
// context.Background(), mirroring chclient's breaker-state recordTrip: this
// is process-state telemetry, not a per-request observation, so there is no
// request context to thread through deriveRouteMemoDispatch's existing
// signature for it.
func recordRouteMemoPressureActive(m *routememo.Memo) {
	now := m.UnderPressure()
	if routeMemoPressureState.Swap(now) == now {
		return
	}
	delta := int64(-1)
	if now {
		delta = 1
	}
	telemetry.RecordRouteMemoPressureTransition(context.Background(), delta)
}

// tryRouteMemoHit consults the route memo BEFORE route A is dispatched: a
// live (non-stale) PreferB verdict routes B directly, subject to every gate
// applying regardless of the recorded verdict (eligibility, freshness,
// breaker, admission).
//
// dispatched is true once Executor.Execute itself returns without a
// pre-flight error (breaker/emit/gate/now64) — NOT once route B is known to
// have SUCCEEDED. Execute opens a streaming cursor; a shard's own
// resource-exhaustion failure (the case that actually matters for memo
// bookkeeping) surfaces later, from the CALLER's drain of the returned
// cursor, never from Execute's synchronous return. So this function commits
// no verdict on route B's eventual fate — the verdict here was ALREADY
// PreferB before this call, and a clean drain needs no re-confirmation
// (Lookup's own TTL/re-validation clock handles staleness independently).
// Only a drain FAILURE needs reporting, immediately, rather than waiting for
// re-validation — key is returned so the caller can build that hook (see
// Major-2's symmetric fallback: on ANY route-B drain failure, Observe the
// failure and fall back to route A, exactly as if this function had never
// been called).
// responseShape is the dispatching adapter's engine.Meta.ResponseShape,
// threaded as a value (like langName) rather than read off ctx because this
// is a route-B dispatch and routeBExecCtx owns stamping it — see that
// function for why every Execute call site declares the shape itself.
func (e *Engine) tryRouteMemoHit(
	ctx context.Context,
	langName string,
	responseShape string,
	plan chplan.Node,
	seedDecision *solver.Decision,
	budget *chclient.SampleBudget,
	attrStrategies chsql.AttrStrategies,
) (cursor chclient.Cursor, info *solver.ExecInfo, usedDecision *solver.Decision, key routememo.Key, dispatched bool) {
	if !e.routeMemoActive() || seedDecision == nil {
		return nil, nil, nil, routememo.Key{}, false
	}
	d := e.deriveRouteMemoDispatch(plan, seedDecision, time.Now())
	if !d.eligible {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNotEligible)
		return nil, nil, nil, routememo.Key{}, false
	}
	if !d.fresh {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNotFresh)
		return nil, nil, nil, routememo.Key{}, false
	}
	state, stale := e.RouteMemo.Lookup(d.key)
	if state != routememo.PreferB {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNoPreferB)
		return nil, nil, nil, routememo.Key{}, false
	}
	if stale {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineStale)
		return nil, nil, nil, routememo.Key{}, false
	}
	if !e.Solver.BreakerClosed() {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineBreakerOpen)
		return nil, nil, nil, routememo.Key{}, false
	}
	release, ok := e.RouteMemo.AdmitDispatch()
	if !ok {
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNoDispatchToken)
		return nil, nil, nil, routememo.Key{}, false
	}
	defer release()
	dispatchDone := telemetry.ObserveRoutedDispatchInflight(ctx)
	defer dispatchDone()

	cur, execInfo, err := e.Solver.Executor.Execute(
		routeBExecCtx(ctx, langName, responseShape, d.decision, e.DeltaPrefixLookback, e.DeltaPrefixReadEnabled, e.resourceBoundOverrides(), e.RangeBucketGridNativeMaxRows, e.RangeBucketGridNativeMaxDensityUnits, e.Actuals, attrStrategies), langName, d.decision, budget,
	)
	if err != nil {
		// A pre-flight failure (breaker/emit/gate/now64) classifies
		// NoEvidence by construction — Observe is a documented no-op for it,
		// called here only for completeness, not because it changes state.
		e.RouteMemo.Observe(d.key, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, err))
		return nil, nil, nil, routememo.Key{}, false
	}
	return cur, execInfo, d.decision, d.key, true
}

// retryOnRouteAResourceFailure is the A->B retry: called after a route-A
// dispatch failed, err is that failure. It classifies err; only a genuine
// resource-exhaustion failure is ever retried (Major-6 — an unrelated
// failure, a timeout, a broken connection, a client cancellation, must
// never poison or drive the memo, and must never spend a retry dispatch on
// nobody's behalf).
//
// retried is true once Executor.Execute itself returns without a
// pre-flight error — a cursor was opened, not that route B is known to have
// succeeded. This retry IS the probe that decides the Key's very first
// verdict (BeginProbe only ever admits an Unknown Key): a clean drain is
// what CREATES a PreferB entry (Memo.Observe's OutcomeSuccess branch), so —
// unlike a memo-hit, where the verdict already exists and a success needs
// no re-confirmation — this dispatch's real outcome cannot be assumed and
// must be reported from the caller's ACTUAL drain result, success or
// failure alike. observeFn is that hook: the caller MUST call it exactly
// once, with the drain error (nil on a clean finish), whenever retried is
// true. Skipping it silently drops the one observation this probe exists to
// make, leaving the Key permanently Unknown no matter how many times route A
// goes on failing it.
// responseShape is the dispatching adapter's engine.Meta.ResponseShape,
// threaded for the same reason as on tryRouteMemoHit: this dispatch opens
// route-B cursors, so it declares the shape through routeBExecCtx. It matters
// most HERE — the retry runs on a ctx the handler supplies at drain time,
// which carries nothing the original dispatch stamped.
//
// elapsed is how long the route-A dispatch this failure came from had been
// running. It is what lets a cancellation past costlyCancellationFloor
// classify as cost evidence rather than "the caller went away" — the failure
// mode that accounts for nearly every real route-A failure observed, and the
// reason this parameter is not optional: passing zero here would leave the
// classifier permanently blind to it.
func (e *Engine) retryOnRouteAResourceFailure(
	ctx context.Context,
	langName string,
	responseShape string,
	plan chplan.Node,
	seedDecision *solver.Decision,
	budget *chclient.SampleBudget,
	err error,
	elapsed time.Duration,
	attrStrategies chsql.AttrStrategies,
) (cursor chclient.Cursor, info *solver.ExecInfo, usedDecision *solver.Decision, observeFn func(drainErr error), retried bool) {
	if !e.routeMemoActive() || seedDecision == nil {
		return nil, nil, nil, nil, false
	}

	outcome := classifyRouteOutcomeAfter(routememo.RouteA, err, elapsed)

	// A client that is already gone must not trigger a dispatch for nobody:
	// a retry would run for nobody's benefit, which is the doomed-trial shape
	// this guard exists to prevent. The EVIDENCE is still worth keeping,
	// though — and a costly cancellation is precisely the case where the dead
	// context IS the failure being classified, so returning before Observe
	// would discard the only signal this mechanism gets from it. Record, never
	// retry: it teaches the next request instead. Anything short of a resource
	// failure stays unrecorded exactly as before (Observe is a documented
	// no-op for OutcomeNoEvidence, and a Success racing a cancellation carries
	// no reliable evidence either way).
	if ctx.Err() != nil {
		if outcome == routememo.OutcomeResourceFailure {
			dead := e.deriveRouteMemoDispatch(plan, seedDecision, time.Now())
			e.RouteMemo.Observe(dead.key, routememo.RouteA, outcome)
		}
		return nil, nil, nil, nil, false
	}

	d := e.deriveRouteMemoDispatch(plan, seedDecision, time.Now())
	if outcome != routememo.OutcomeResourceFailure {
		// Success or NoEvidence: plain Observe (Success drops any recorded
		// state; NoEvidence is a documented no-op) — neither ever earns a
		// probe, so there is nothing for the atomic record-and-admit method
		// below to decide.
		e.RouteMemo.Observe(d.key, routememo.RouteA, outcome)
		if outcome == routememo.OutcomeSuccess {
			telemetry.RecordRouteABSuccess(ctx, telemetry.RouteChoiceA)
		}
		return nil, nil, nil, nil, false
	}
	if !d.eligible {
		e.RouteMemo.Observe(d.key, routememo.RouteA, outcome)
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNotEligible)
		return nil, nil, nil, nil, false
	}
	if !d.fresh {
		e.RouteMemo.Observe(d.key, routememo.RouteA, outcome)
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineNotFresh)
		return nil, nil, nil, nil, false
	}
	if !e.Solver.BreakerClosed() {
		// Still record the failure even when a gate blocks probing — the
		// corroboration count (or the stale-PreferB re-validation refresh)
		// must progress regardless of whether THIS failure is admitted, or
		// a Key that keeps failing while e.g. transiently ineligible would
		// never accumulate enough evidence to ever probe once it clears.
		e.RouteMemo.Observe(d.key, routememo.RouteA, outcome)
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineBreakerOpen)
		return nil, nil, nil, nil, false
	}
	// Snapshot whether THIS failure is heading into the stale-PreferB
	// re-validation rescue (as opposed to a brand-new Unknown key's
	// first-ever probe) BEFORE the atomic call below mutates state — purely
	// to inform the magnitude-trivial decline right after it, never as a
	// substitute for that call's own internal atomic staleness check (see
	// its doc for why record-and-decide must stay one critical section).
	// Reading it here cannot reintroduce that race: this is an ADDITIONAL,
	// non-mutating read for a second, independent, dispatch-token-budget
	// decision, not a second attempt at the state-transition decision
	// itself — at worst a concurrent request makes this snapshot stale by
	// the time the atomic call runs, which costs only dispatch-token
	// efficiency, never a wrong answer (routememo never decides HOW route B
	// computes its answer, only whether to try it).
	preState, preStale := e.RouteMemo.Lookup(d.key)
	magRows, magObservations, magOK := e.RouteMemo.MagnitudeFor(d.key)
	trivialRevalidation := preState == routememo.PreferB && preStale &&
		magOK && magObservations >= routememo.MinCorroboratingFailures &&
		d.decision.NAnchors > 0 &&
		magRows < float64(d.decision.NAnchors)*routeMemoTrivialMagnitudeRowsPerAnchor

	// Record the failure AND decide probe admission atomically — see
	// ObserveRouteAFailureAndMaybeBeginProbe's doc for why this must not be
	// two separate calls (a stale PreferB entry's re-validation rescue
	// depends on the admission check seeing pre-refresh staleness). The
	// method reports pressureDeclined atomically with the decision itself,
	// so the decline reason never has to re-sample UnderPressure()
	// separately and race the very transition this call just made.
	release, ok, pressureDeclined := e.RouteMemo.ObserveRouteAFailureAndMaybeBeginProbe(d.key)
	if !ok {
		if pressureDeclined {
			telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineUnderPressure)
		} else {
			telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineProbeNotAdmitted)
		}
		return nil, nil, nil, nil, false
	}
	if trivialRevalidation {
		// The admission above already recorded the failure (corroboration
		// / re-validation-clock bookkeeping progresses exactly as if this
		// rescue had run) — only the DISPATCH itself is declined, giving
		// the token straight back unspent rather than spending one of
		// maxConcurrentRoutedDispatches re-confirming a route whose own
		// well-corroborated history says it barely moves any data. A
		// brand-new Unknown key's first-ever probe is never affected:
		// MagnitudeFor has no reading for a key that has never once
		// completed a route-B dispatch, so trivialRevalidation is always
		// false there.
		release()
		telemetry.RecordRouteMemoHitSkipped(ctx, telemetry.RouteMemoDeclineTrivialMagnitude)
		return nil, nil, nil, nil, false
	}
	dispatchDone := telemetry.ObserveRoutedDispatchInflight(ctx)

	cur, execInfo, dispatchErr := e.Solver.Executor.Execute(
		routeBExecCtx(ctx, langName, responseShape, d.decision, e.DeltaPrefixLookback, e.DeltaPrefixReadEnabled, e.resourceBoundOverrides(), e.RangeBucketGridNativeMaxRows, e.RangeBucketGridNativeMaxDensityUnits, e.Actuals, attrStrategies), langName, d.decision, budget,
	)
	if dispatchErr != nil {
		// A pre-flight failure classifies NoEvidence by construction —
		// Observe is a documented no-op for it. The probe token is released
		// here since there is no cursor for the caller to drain and
		// therefore no later call site to release it.
		e.RouteMemo.Observe(d.key, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, dispatchErr))
		release()
		dispatchDone()
		return nil, nil, nil, nil, false
	}

	key := d.key
	var once sync.Once
	observeFn = func(drainErr error) {
		// once-guarded: a caller invoking observeFn more than once (a bug
		// on its side) must not double-release the admission token or
		// double-flip the verdict on a second, possibly-different drainErr.
		once.Do(func() {
			drainOutcome := classifyRouteOutcome(routememo.RouteB, drainErr)
			e.RouteMemo.Observe(key, routememo.RouteB, drainOutcome)
			if drainOutcome == routememo.OutcomeSuccess {
				telemetry.RecordRouteABSuccess(ctx, telemetry.RouteChoiceB)
				// Observe above already landed (or refreshed) the PreferB
				// verdict for key, so this magnitude reading lands on that
				// SAME entry, never a doomed-to-be-replaced Unknown one —
				// see recordRouteMemoMagnitude's own doc. This is the probe
				// / stale-rescue half of the sampling-bias fix issue #3035
				// describes: magnitude no longer comes ONLY from the
				// per-rung predictive cursor (per_rung_admission.go), it
				// also comes from every clean first-probe and re-validation
				// drain the failure-driven route memo itself dispatches.
				recordRouteMemoMagnitude(e.RouteMemo, key, cur)
			}
			release()
			dispatchDone()
		})
	}
	return cur, execInfo, d.decision, observeFn, true
}
