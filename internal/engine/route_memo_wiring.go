package engine

import (
	"context"
	"sync"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
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
	return routeMemoDispatch{
		key:      key,
		decision: decision,
		eligible: eligible,
		fresh:    freshEnoughForRouteMemo(end, step, now),
	}
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
func (e *Engine) tryRouteMemoHit(
	ctx context.Context,
	langName string,
	plan chplan.Node,
	seedDecision *solver.Decision,
	budget *chclient.SampleBudget,
) (cursor chclient.Cursor, info *solver.ExecInfo, usedDecision *solver.Decision, key routememo.Key, dispatched bool) {
	if !e.routeMemoActive() || seedDecision == nil {
		return nil, nil, nil, routememo.Key{}, false
	}
	d := e.deriveRouteMemoDispatch(plan, seedDecision, time.Now())
	if !d.eligible || !d.fresh {
		return nil, nil, nil, routememo.Key{}, false
	}
	state, stale := e.RouteMemo.Lookup(d.key)
	if state != routememo.PreferB || stale {
		return nil, nil, nil, routememo.Key{}, false
	}
	if !e.Solver.BreakerClosed() {
		return nil, nil, nil, routememo.Key{}, false
	}
	release, ok := e.RouteMemo.AdmitDispatch()
	if !ok {
		return nil, nil, nil, routememo.Key{}, false
	}
	defer release()

	cur, execInfo, err := e.Solver.Executor.Execute(ctx, langName, d.decision, budget)
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
func (e *Engine) retryOnRouteAResourceFailure(
	ctx context.Context,
	langName string,
	plan chplan.Node,
	seedDecision *solver.Decision,
	budget *chclient.SampleBudget,
	err error,
) (cursor chclient.Cursor, info *solver.ExecInfo, usedDecision *solver.Decision, observeFn func(drainErr error), retried bool) {
	if !e.routeMemoActive() || seedDecision == nil {
		return nil, nil, nil, nil, false
	}
	// A client that is already gone must not trigger a dispatch for nobody
	// — check before any other work, including before Observe (a
	// cancellation racing a genuine resource failure carries no reliable
	// evidence either way, and the cost of skipping bookkeeping on the rare
	// race is far cheaper than fanning out for a disconnected caller).
	if ctx.Err() != nil {
		return nil, nil, nil, nil, false
	}

	outcome := classifyRouteOutcome(routememo.RouteA, err)
	d := e.deriveRouteMemoDispatch(plan, seedDecision, time.Now())
	e.RouteMemo.Observe(d.key, routememo.RouteA, outcome)
	if outcome != routememo.OutcomeResourceFailure {
		return nil, nil, nil, nil, false
	}
	if !d.eligible || !d.fresh || !e.Solver.BreakerClosed() {
		return nil, nil, nil, nil, false
	}
	release, ok := e.RouteMemo.BeginProbe(d.key)
	if !ok {
		return nil, nil, nil, nil, false
	}

	cur, execInfo, dispatchErr := e.Solver.Executor.Execute(ctx, langName, d.decision, budget)
	if dispatchErr != nil {
		// A pre-flight failure classifies NoEvidence by construction —
		// Observe is a documented no-op for it. The probe token is released
		// here since there is no cursor for the caller to drain and
		// therefore no later call site to release it.
		e.RouteMemo.Observe(d.key, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, dispatchErr))
		release()
		return nil, nil, nil, nil, false
	}

	key := d.key
	var once sync.Once
	observeFn = func(drainErr error) {
		// once-guarded: a caller invoking observeFn more than once (a bug
		// on its side) must not double-release the admission token or
		// double-flip the verdict on a second, possibly-different drainErr.
		once.Do(func() {
			e.RouteMemo.Observe(key, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, drainErr))
			release()
		})
	}
	return cur, execInfo, d.decision, observeFn, true
}
