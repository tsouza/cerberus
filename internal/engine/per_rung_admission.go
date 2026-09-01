package engine

import (
	"sync"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// per_rung_admission.go closes issue #2709: internal/solver/planner.go's
// ModeAuto per-rung bypass (minAnchorsForPerRungShard) admits a
// RangeBucketGridNative-carrying plan onto route B on the ANCHOR AXIS alone,
// because that carrier's F is unmeasurable at plan time (see that constant's
// own doc). Anchor count is pure grid GEOMETRY — it says nothing about how
// much DATA backs the grid — and #2709 found a real case where geometry alone
// gets it wrong: a 24h/1m classic-histogram dashboard panel over a
// low-cardinality metric (a few dozen to a couple hundred series) clears the
// anchor floor by more than 10x, so it predictively routes B, but K
// concurrent ClickHouse queries contending over a table with almost nothing
// in it is pure overhead — one unsharded query would have finished first.
//
// #2709 itself establishes (see the issue and this package's own tests) that
// no purely GEOMETRIC signal can fix this: a genuine production incident this
// bypass exists to catch (issue #2677) has FEWER anchors (as few as ~90) than
// this false-positive panel (1,441), so any anchor-only threshold that admits
// the real incident also admits the false positive, and any threshold that
// excludes the false positive also excludes the real incident. The two
// populations are only separable by how much data the grid actually divides
// — which requires evidence, not geometry.
//
// This file supplies that evidence the ONLY way it can be had without a new
// live round-trip on every per-rung request: it watches what a per-rung
// PREDICTIVE dispatch (solver.Decision.PerRungPredictive) ACTUALLY drained,
// and once a plan SHAPE has repeatedly, cleanly produced a small composed
// result despite clearing the anchor floor, it declines the bypass for
// FUTURE requests of that same shape — falling back to the anchor-only
// default (today's behavior, unchanged) until that evidence exists. This is
// the "track observed per-shard cost, learn when a shape is worth splitting,
// fall back to the conservative default with no evidence" escalation #2709
// itself names as the correct one once geometry alone is shown insufficient.
//
// Deliberately narrow: it never REFUSES a plan the Planner judged eligible,
// never promotes route A to route B, and never touches ModeSharded or
// Eligible()'s failure-driven memo seam — Plan's own per-rung bypass, and
// the geometry-only threshold it clears at, are completely unchanged. This
// only ever downgrades an already-PerRungPredictive route B to route A, and
// only once evidence says the shape does not need it.

// perRungEvidenceMinObservations mirrors routememo's own
// minCorroboratingFailures: a single clean-and-cheap drain cannot, by
// itself, teach the learner anything — a low-traffic blip or an
// unrepresentative first sample must not flip a shape's verdict alone.
const perRungEvidenceMinObservations = 2

// perRungCheapRowsPerAnchor bounds "cheap" RELATIVE to the anchor count
// rather than as an absolute row count, because N is already part of the
// routing Key (AnchorLg) and the false-positive class #2709 describes is a
// WIDE grid over FEW underlying series — a fixed absolute floor would wrongly
// call a genuinely wide, genuinely high-cardinality panel "cheap" just because
// its anchor count inflates the total. A composed route-B result averaging
// fewer than this many output rows per anchor is producing, at every anchor,
// a small enough set of distinct series/label groups that no realistic
// backing-table size behind it would have justified K-way contention to
// answer it — sharding divides the anchor axis, not label cardinality, so a
// query this narrow per anchor has few underlying series to scan regardless
// of window width.
const perRungCheapRowsPerAnchor = 20

// perRungEvidenceTTL bounds how long a learned verdict is trusted, mirroring
// routememo's own memoEntryTTL (30m): a metric's real cardinality can grow
// (a new label value, a service scaling out), and a verdict computed against
// yesterday's shape must not silently suppress predictive routing forever
// once that has happened. An observation older than this resets the state as
// if nothing had been learned yet, which is the ordinary "no evidence exists"
// default, not a fresh strike against the shape.
const perRungEvidenceTTL = 30 * time.Minute

// perRungLearnerCapacity mirrors routememo's own memoMaxEntries: a bound
// resident size so unbounded key cardinality cannot grow this cache without
// limit. Eviction here is coarser than routememo's true LRU (any single
// entry may be evicted once at capacity, not necessarily the oldest) because
// the cost of evicting the wrong entry is trivial — it only resets that one
// shape back to "no evidence yet", the always-safe default.
const perRungLearnerCapacity = 4096

// perRungAdmissionState is one plan-shape's rolling evidence.
type perRungAdmissionState struct {
	consecutiveCheap int
	lastObserved     time.Time
}

// PerRungAdmissionLearner is the OPTIONAL, per-Engine-instance cache backing
// this file's own doc. A nil *PerRungAdmissionLearner behaves exactly like a
// nil Engine.RouteMemo: every method on it is written to be called only
// through Engine.PerRungAdmission's own nil guard at the call sites in
// engine.go, so the zero (unwired) Engine stays byte-unchanged — see
// NewPerRungAdmissionLearner's caller, buildPerRungAdmission
// (cmd/cerberus/main.go).
type PerRungAdmissionLearner struct {
	mu     sync.Mutex
	states map[routememo.Key]*perRungAdmissionState
}

// NewPerRungAdmissionLearner constructs an empty learner.
func NewPerRungAdmissionLearner() *PerRungAdmissionLearner {
	return &PerRungAdmissionLearner{states: make(map[routememo.Key]*perRungAdmissionState)}
}

// Observe records one COMPLETED, CLEANLY-DRAINED per-rung predictive route-B
// dispatch's total composed output row count for key's shape. The caller is
// responsible for only calling this on a clean drain (Cursor.Err() == nil) —
// see perRungObservingCursor and executeRouted's own eager-path call site —
// because a CANCELLED or partially-drained dispatch (exactly #2709's own
// reported symptom: a client cancel after 19s) reports a truncated row count
// that looks artificially cheap. Recording that would teach the learner the
// opposite of the truth: the dispatch was too slow to finish, not too small
// to matter.
func (l *PerRungAdmissionLearner) Observe(key routememo.Key, outputRows, nAnchors int64) {
	if nAnchors <= 0 {
		return
	}
	l.record(key, outputRows < nAnchors*perRungCheapRowsPerAnchor)
}

// record is the single critical-section body shared by Observe and
// SeedPriorFromEstimate: both ultimately reduce to one boolean verdict
// ("was this evidence cheap") over key's rolling state, and diverge only in
// where that verdict came from — a real drained row count for Observe, an
// advisory EXPLAIN ESTIMATE comparison for SeedPriorFromEstimate.
func (l *PerRungAdmissionLearner) record(key routememo.Key, cheap bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.states[key]
	if !ok {
		if len(l.states) >= perRungLearnerCapacity {
			for k := range l.states {
				delete(l.states, k)
				break
			}
		}
		st = &perRungAdmissionState{}
		l.states[key] = st
	}
	if cheap {
		st.consecutiveCheap++
	} else {
		st.consecutiveCheap = 0
	}
	st.lastObserved = time.Now()
}

// SeedPriorFromEstimate records an advisory EXPLAIN ESTIMATE verdict (issue
// #2787) for key's shape as if it were ONE clean observation, in the SAME
// direction Observe's own cheap/not-cheap classification already uses —
// never a distinct code path a reviewer has to separately trust. This is
// what lets a shape decline the per-rung bypass on its FIRST request instead
// of waiting for perRungEvidenceMinObservations real production dispatches
// to drain cleanly and cheaply, which is exactly the "seed... with priors
// instead of waiting for two real production failures" case issue #2787
// asks for — for THIS learner specifically, because it can only ever
// DOWNGRADE an already-PerRungPredictive route back to route A
// (refinePerRungAdmission's own doc), never promote or block one: a wrong
// prior costs one shape an unnecessary shard for perRungEvidenceTTL, the
// same always-safe direction a wrong REAL observation already costs.
//
// cheap accepts either value for the same reason Observe does (this method
// is a general building block, not itself the policy), but BOTH callers —
// explain_estimate_wiring.go's maybeSeedPerRungPrior (issue #2787, a live
// EXPLAIN ESTIMATE round trip) and actuals_wiring.go's
// maybeSeedPerRungAdmissionFromActuals (issue #2789, a zero-I/O read of a
// PAST dispatch's tracked actuals) — only ever pass true: a near-empty
// scan-side signal is safe evidence FOR near-empty (an aggregate cannot
// emit a meaningful value from samples it never read) but not reliable
// evidence AGAINST it (dense raw data can still collapse to a small
// composed result) — see either call site's own doc for why treating a
// large reading as cheap=false would risk resetting real accumulated
// evidence on an imprecise proxy. A single seed only ever advances
// consecutiveCheap by one observation's worth (never jumps straight past
// perRungEvidenceMinObservations), so a shape whose estimate is wrong once
// is not permanently mis-seeded — the next REAL Observe call, clean or not,
// corrects it exactly as it would a real first observation.
func (l *PerRungAdmissionLearner) SeedPriorFromEstimate(key routememo.Key, cheap bool) {
	l.record(key, cheap)
}

// ShouldDeclineBypass reports whether key's shape has accumulated enough
// FRESH, consecutive cheap observations to decline the anchor-only per-rung
// bypass on its next request. Returns false (the always-safe default: keep
// today's geometry-only admission) when there is no entry, or the entry has
// aged past perRungEvidenceTTL.
func (l *PerRungAdmissionLearner) ShouldDeclineBypass(key routememo.Key) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.states[key]
	if !ok {
		return false
	}
	if time.Since(st.lastObserved) > perRungEvidenceTTL {
		return false
	}
	return st.consecutiveCheap >= perRungEvidenceMinObservations
}

// hasFreshEntry reports whether key has ANY unexpired entry at all —
// unlike ShouldDeclineBypass, which also answers false for a fresh entry
// that has not yet accumulated perRungEvidenceMinObservations. Used only by
// explain_estimate_wiring.go's ScanEstimateAdvisor to decide whether this
// learner already holds SOME verdict for key (of either polarity) before
// spending an advisory EXPLAIN ESTIMATE round trip on it — issue #2787's own
// "skip when the memo or admission system already holds a verdict"
// constraint, applied to this learner specifically.
func (l *PerRungAdmissionLearner) hasFreshEntry(key routememo.Key) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.states[key]
	if !ok {
		return false
	}
	return time.Since(st.lastObserved) <= perRungEvidenceTTL
}

// perRungAdmissionKey computes the routememo.Key for decision's own grid
// readout, reused for both the pre-dispatch admission lookup and the
// post-drain observation that feeds it — the SAME shape must hash to the
// SAME key on both sides or the learner never converges.
func perRungAdmissionKey(plan chplan.Node, decision *solver.Decision) routememo.Key {
	return routememo.KeyFor(plan, decision.NAnchors, decision.Fanout, decision.Step)
}

// declinePerRungBypass demotes decision to the non-route Plan's ModeAuto
// branch would have produced had the per-rung bypass not fired on cost
// grounds — Strategy/K/Slices/PerRungPredictive clear, Reason becomes
// solver.ReasonBelowThreshold (the SAME token an anchor-only-insufficient
// per-rung carrier already reports; this is that same cost gate, now
// additionally informed by real evidence). The cost-grid fields
// (NAnchors/Fanout/CumulativeD/OuterRange/Step) are left untouched: they are
// a pure readout of the same plan and stay accurate on a declined route.
func declinePerRungBypass(decision *solver.Decision) *solver.Decision {
	declined := *decision
	declined.Strategy = ""
	declined.K = 0
	declined.Slices = nil
	declined.Reason = solver.ReasonBelowThreshold
	declined.PerRungPredictive = false
	return &declined
}

// refinePerRungAdmission re-checks a routed, PerRungPredictive Decision
// against e.PerRungAdmission's own learned evidence before dispatch. Decision
// and routed pass through byte-unchanged when PerRungAdmission is nil (the
// default — see Engine.PerRungAdmission's own doc), when decision is nil, when
// routed is already false, or when the route did not come from the per-rung
// bypass (PerRungPredictive false) — every other route (an ordinary
// MinFanout/MinAnchorPairs clearance, ModeSharded, or Eligible()'s
// failure-driven memo escalation) is a real cost/evidence-based decision
// already and is never second-guessed here.
func (e *Engine) refinePerRungAdmission(plan chplan.Node, decision *solver.Decision, routed bool) (*solver.Decision, bool) {
	if e.PerRungAdmission == nil || decision == nil || !routed || !decision.PerRungPredictive {
		return decision, routed
	}
	key := perRungAdmissionKey(plan, decision)
	if !e.PerRungAdmission.ShouldDeclineBypass(key) {
		return decision, routed
	}
	return declinePerRungBypass(decision), false
}

// perRungObservingCursor wraps a route-B cursor for a per-rung PREDICTIVE
// dispatch so its own Close() feeds the learner exactly what the CALLER
// actually drained. QueryPlanCursor's caller owns the drain (executeRoutedCursor's
// own doc: "there is no later engine-side site to record from"), so this
// wrapper IS that site — it changes no behavior the caller observes (every
// method but Close delegates straight through the embedded Cursor) and only
// ever reads Err()/Inspected() after Close(), never influencing either.
//
// routeMemo is issue #2789's hook 2 (routememo.Memo.RecordActualMagnitude's
// own doc): OPTIONAL and independent of learner — either, both, or neither
// may be nil, and each is fed from the SAME clean drain independently. A
// per-rung predictive dispatch's routememo.Key (perRungAdmissionKey) is
// computed with the identical routememo.KeyFor(plan, decision.NAnchors,
// decision.Fanout, decision.Step) formula route_memo_wiring.go's own
// deriveRouteMemoDispatch uses for the SAME plan/decision, so a magnitude
// recorded here lands on the SAME Key the route memo's own routing state
// already tracks — without this wrapper needing to touch
// route_memo_wiring.go's own dispatch call graph at all.
type perRungObservingCursor struct {
	chclient.Cursor
	learner   *PerRungAdmissionLearner
	routeMemo *routememo.Memo
	key       routememo.Key
	nAnchors  int64
	once      sync.Once
}

// Close delegates to the wrapped cursor first (this must never change teardown
// timing or behavior), then records the observation exactly once — guarded by
// sync.Once because a cursor may be Closed more than once by a defensive
// caller, and a second observation must not double-count. Only a CLEAN drain
// (Err() == nil) is trusted evidence — see PerRungAdmissionLearner.Observe's
// own doc for why a cancelled/truncated drain must never be recorded as
// "cheap"; the same reasoning applies to routeMemo's magnitude reading.
func (c *perRungObservingCursor) Close() error {
	err := c.Cursor.Close()
	c.once.Do(func() {
		if c.Err() != nil {
			return
		}
		if c.learner != nil {
			c.learner.Observe(c.key, c.Inspected(), c.nAnchors)
		}
		if c.routeMemo != nil && c.Inspected() >= 0 {
			c.routeMemo.RecordActualMagnitude(c.key, uint64(c.Inspected())) //nolint:gosec // G115 -- guarded by the >= 0 check above
		}
	})
	return err
}

// wrapPerRungObserver returns cursor unchanged when BOTH learner and
// routeMemo are nil, or the dispatch was not a per-rung predictive route —
// every other route-B cursor stays exactly what Executor.Execute returned.
func wrapPerRungObserver(cursor chclient.Cursor, learner *PerRungAdmissionLearner, routeMemo *routememo.Memo, plan chplan.Node, decision *solver.Decision) chclient.Cursor {
	if (learner == nil && routeMemo == nil) || decision == nil || !decision.PerRungPredictive {
		return cursor
	}
	return &perRungObservingCursor{
		Cursor:    cursor,
		learner:   learner,
		routeMemo: routeMemo,
		key:       perRungAdmissionKey(plan, decision),
		nAnchors:  int64(decision.NAnchors),
	}
}
