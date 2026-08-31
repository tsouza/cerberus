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

// explain_estimate_wiring.go closes the engine-side half of cerberus issue
// #2787 (internal/chclient/explain_estimate.go is the transport half): it
// decides WHETHER a given request is worth an advisory EXPLAIN ESTIMATE
// round trip, caches the result per plan shape, and feeds it back into
// Engine.classify's solver.RequestMeta so Planner.Plan can consume it — see
// internal/solver/planner.go's own doc for exactly how K consumes it.
//
// The issue states its cost constraint as VERIFIED, not optional:
// per_rung_admission.go's own doc already rejected "a new live round-trip on
// every per-rung request" once, for the SAME per_rung predictive-route
// population this feature also touches. ScanEstimateAdvisor exists
// specifically to not reintroduce that failure mode, via three independent
// narrowings, every one of which must hold before a round trip is spent:
//
//  1. ModeAuto only, and only a plan whose baseline (no-estimate)
//     classification already reached the cost-grid section of classify() —
//     reachedCostGrid below. A plan a structural/correctness gate refuses
//     (now64, not-sliceable, an instant query, ...) cannot be affected by an
//     estimate at all (planner.go's own near-empty check and K-ceiling raise
//     both sit strictly inside that section), so probing one is pure waste.
//  2. Cached per plan shape (routememo.Key, the SAME literal-free
//     fingerprint the route memo and the per-rung admission learner already
//     key on) with a bounded TTL and a bounded resident size — mirroring
//     PerRungAdmissionLearner's own perRungLearnerCapacity /
//     perRungEvidenceTTL posture.
//  3. Skipped entirely whenever the route memo OR the per-rung admission
//     learner already holds a verdict for the shape: a Key either mechanism
//     has already resolved needs no additional advisory signal, so spending
//     a round trip on it would buy nothing.
//
// A probe failure — emission error, breaker-open, transport error — is
// treated as "no estimate" and never surfaces to the caller: this signal is
// advisory-only by the issue's own risk boundary, so it must never become a
// reason a query fails that would otherwise have succeeded.

// scanEstimateCacheCapacity / scanEstimateCacheTTL mirror
// PerRungAdmissionLearner's own perRungLearnerCapacity / perRungEvidenceTTL:
// a bounded resident cache with coarse (any-single-entry) eviction, whose
// worst case is only ever "probe again a little sooner than the shape truly
// needed" — the always-safe direction for an advisory-only signal.
const (
	scanEstimateCacheCapacity = 4096
	scanEstimateCacheTTL      = 30 * time.Minute
)

// scanEstimateCacheEntry is one plan shape's cached advisory estimate.
type scanEstimateCacheEntry struct {
	estimate chclient.ScanEstimate
	cachedAt time.Time
}

// Estimator is the narrow chclient seam ScanEstimateAdvisor depends on —
// *chclient.Client in production, faked in tests so this file's gating /
// caching logic is testable without a live ClickHouse.
type Estimator interface {
	ExplainEstimate(ctx context.Context, sql string, args ...any) (chclient.ScanEstimate, error)
}

// ScanEstimateAdvisor is the OPTIONAL, per-Engine-instance cache and gate
// implementing this file's own doc. A nil *ScanEstimateAdvisor behaves
// exactly like a nil Engine.RouteMemo / Engine.PerRungAdmission: every call
// site below is written to be reached only through Engine.classify's own nil
// guard, so an Engine that never wires one is byte-unchanged.
type ScanEstimateAdvisor struct {
	client Estimator
	// perRungAdmission is OPTIONAL (may be nil, exactly like
	// Engine.PerRungAdmission itself): when set, a near-empty advisory
	// estimate also seeds a prior into it — see maybeSeedPerRungPrior.
	perRungAdmission *PerRungAdmissionLearner

	mu      sync.Mutex
	entries map[routememo.Key]scanEstimateCacheEntry
}

// NewScanEstimateAdvisor constructs an advisor bound to client.
// perRungAdmission may be nil.
func NewScanEstimateAdvisor(client Estimator, perRungAdmission *PerRungAdmissionLearner) *ScanEstimateAdvisor {
	return &ScanEstimateAdvisor{
		client:           client,
		perRungAdmission: perRungAdmission,
		entries:          make(map[routememo.Key]scanEstimateCacheEntry),
	}
}

// reachedCostGrid reports whether baselineReason means classify() reached
// the cost-grid section — the only place a RequestMeta.Estimate is ever
// consulted (planner.go's own near-empty check and K-ceiling raise both sit
// inside it). Every other reason means a structural or correctness gate
// refused the plan strictly BEFORE that section, so an estimate could not
// have changed the outcome and probing for one is pure waste — precisely
// the round trip issue #2787's own cost constraint exists to avoid.
//
// solver.ReasonBelowThreshold is ALSO reachable from classify()'s own
// windowless-plan short-circuit (planner.go: "eligible but no windowed node
// carries an anchor grid to slice"), strictly before the cost-grid section —
// a plan this reports for is rare (an eligible PromQL shape with no
// [chplan.GridCarrier] at all) and probing it wastes one round trip rather
// than affecting the outcome, the same always-safe direction every other
// imprecision in this file already accepts.
func reachedCostGrid(baselineReason string) bool {
	switch baselineReason {
	case solver.ReasonRouted, solver.ReasonBelowThreshold, solver.ReasonAnchorGridIndivisible,
		solver.ReasonHighD, solver.ReasonIncommensurate:
		return true
	default:
		return false
	}
}

// shapeKey computes the SAME literal-free routememo.Key the route memo and
// the per-rung admission learner already key on, from decision's own cost
// grid readout — see routememo.KeyFor's own doc for why (N, F, Step) alone
// is a safe, closed, plan-shape fingerprint.
func shapeKey(plan chplan.Node, decision *solver.Decision) routememo.Key {
	return routememo.KeyFor(plan, decision.NAnchors, decision.Fanout, decision.Step)
}

// hasExistingVerdict reports whether ANY of the memo/admission mechanisms
// this file must defer to already holds a verdict for key — see this file's
// own doc, narrowing (3).
func hasExistingVerdict(routeMemo *routememo.Memo, perRungAdmission *PerRungAdmissionLearner, key routememo.Key) bool {
	if routeMemo != nil {
		if state, _ := routeMemo.Lookup(key); state != routememo.Unknown {
			return true
		}
	}
	if perRungAdmission != nil && perRungAdmission.hasFreshEntry(key) {
		return true
	}
	return false
}

// Advise returns the advisory *solver.ScanEstimate for plan, or nil when the
// round trip is skipped or fails — see this file's own doc for the three
// independent narrowings that gate it, and this method's own comments for
// where each one is enforced. baseline is the classification ALREADY
// computed once with no estimate (Engine.classify's own first pass): pure,
// no I/O, so recomputing it here would cost nothing extra, but the caller
// already has it, so it is threaded through rather than redone. emit
// renders plan to the SQL the probe runs against — the caller's own
// emitForHead closed over its Engine fields (Engine.classify's call site),
// substitutable in tests — so this file needs no config surface of its own
// to duplicate Engine's existing DeltaPrefixLookback / ResourceBoundOverrides
// / RangeBucketGridNativeMaxRows fields.
func (a *ScanEstimateAdvisor) Advise(
	ctx context.Context,
	routeMemo *routememo.Memo,
	plan chplan.Node,
	lang Lang,
	baseline *solver.Decision,
	emit func(ctx context.Context, lang Lang, plan chplan.Node) (string, []any, error),
) *solver.ScanEstimate {
	if a == nil || baseline == nil {
		return nil
	}
	// Narrowing (1): only a plan whose baseline classification reached the
	// cost-grid section is worth probing at all.
	if !reachedCostGrid(baseline.Reason) {
		return nil
	}
	key := shapeKey(plan, baseline)
	// Narrowing (3): the route memo or the per-rung admission learner
	// already knows what to do with this shape — an advisory signal would
	// buy nothing.
	if hasExistingVerdict(routeMemo, a.perRungAdmission, key) {
		return nil
	}
	// Narrowing (2): per-shape cache, ahead of the round trip.
	if cached, ok := a.cached(key); ok {
		return advisoryFromEstimate(cached)
	}

	sql, args, err := emit(ctx, lang, plan)
	if err != nil {
		// Emission failure here is not this probe's problem to report — the
		// real dispatch path will hit and report the SAME error momentarily.
		// Fail open: no estimate, exactly as if this file did not exist.
		return nil
	}
	est, err := a.client.ExplainEstimate(ctx, sql, args...)
	if err != nil {
		// Advisory-only, fail-open (this file's own doc): a probe failure —
		// breaker-open, transport error — must never turn into a query
		// failure for a signal that was never a correctness gate.
		return nil
	}

	a.store(key, est)
	a.maybeSeedPerRungPrior(key, est, baseline)
	return advisoryFromEstimate(est)
}

// cached returns key's cached estimate when present and still fresh.
func (a *ScanEstimateAdvisor) cached(key routememo.Key) (chclient.ScanEstimate, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[key]
	if !ok || time.Since(entry.cachedAt) > scanEstimateCacheTTL {
		return chclient.ScanEstimate{}, false
	}
	return entry.estimate, true
}

// store records key's estimate, evicting one arbitrary entry at capacity —
// coarser than a true LRU, exactly like PerRungAdmissionLearner's own
// eviction: the cost of evicting the wrong entry here is trivial (one shape
// probes again a little sooner), so a full LRU buys nothing worth its own
// bookkeeping.
func (a *ScanEstimateAdvisor) store(key routememo.Key, est chclient.ScanEstimate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.entries[key]; !ok && len(a.entries) >= scanEstimateCacheCapacity {
		for k := range a.entries {
			delete(a.entries, k)
			break
		}
	}
	a.entries[key] = scanEstimateCacheEntry{estimate: est, cachedAt: time.Now()}
}

// maybeSeedPerRungPrior seeds a.perRungAdmission with a prior derived from
// est (issue #2787's "seed... with priors instead of waiting for two real
// production failures", applied to the ONE learner this file can seed
// safely — see PerRungAdmissionLearner.SeedPriorFromEstimate's own doc for
// why the route memo's failure ledger is deliberately NOT seeded this way).
//
// ONE-DIRECTIONAL ON PURPOSE: this only ever calls SeedPriorFromEstimate
// with cheap=true, never cheap=false. est.Rows is a SCAN-side quantity (raw
// rows the index analysis could not prune); Observe's own outputRows is a
// post-aggregation quantity (the per-rung dispatch's actual COMPOSED
// result). A near-zero scan estimate is strong, safe evidence the composed
// output is ALSO near-zero — an aggregate cannot emit a meaningful bucket-
// ladder value from samples that were never read — but a LARGE scan
// estimate says nothing reliable about output cardinality (dense raw data
// can still collapse to a small composed result), so treating it as
// evidence AGAINST the shape would risk resetting real, already-accumulated
// consecutiveCheap evidence from actual clean drains on a proxy the issue's
// own risk boundary warns against conflating: "the estimate targets the
// scan shape, not the fanout product." Seeding only the safe direction
// keeps this strictly additive to what Observe already learns from reality.
//
// No-op when a.perRungAdmission is nil, baseline carries no anchor count, or
// the estimate is not near-empty.
func (a *ScanEstimateAdvisor) maybeSeedPerRungPrior(key routememo.Key, est chclient.ScanEstimate, baseline *solver.Decision) {
	if a.perRungAdmission == nil || baseline.NAnchors <= 0 {
		return
	}
	// Real EXPLAIN ESTIMATE row counts never approach int64 overflow (~9.2e18).
	if int64(est.Rows) >= int64(baseline.NAnchors)*perRungCheapRowsPerAnchor { //nolint:gosec // G115
		return
	}
	a.perRungAdmission.SeedPriorFromEstimate(key, true)
}

// advisoryFromEstimate converts the chclient transport type into the
// solver-package-local advisory value classify threads onto RequestMeta.
// Estimate — see solver.RequestMeta.Estimate's own doc for why solver does
// not import chclient's type directly.
func advisoryFromEstimate(est chclient.ScanEstimate) *solver.ScanEstimate {
	return &solver.ScanEstimate{Parts: est.Parts, Rows: est.Rows, Marks: est.Marks}
}
