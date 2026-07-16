package solver

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// RequestMeta carries the request-level grid metadata the Planner needs to
// classify a plan. It is the package-local stand-in for engine.Meta: the
// import-cycle rule (internal/engine imports internal/solver, never the
// reverse) forbids referencing engine.Meta here, so the engine adapter
// (internal/engine, engine.go) populates this small struct from its own
// Meta + Lang.
//
// The Planner uses it both as the cost-signal source (Step / OuterRange) and
// as the @-modifier guard's oracle: a windowed node's bounds must match the
// grid this Meta predicts at that spine depth.
type RequestMeta struct {
	// Lang is the head name ("promql" | "logql" | "traceql"). Only PromQL
	// query_range is routed; the field lets the Planner reject the others
	// without importing the engine's Lang registry.
	Lang string

	// Start / End are the request eval window (the outermost grid bounds).
	// Both must be non-zero for a windowed plan to be routable: zero bounds
	// resolve to now64() per statement, so two shards would disagree on the
	// wall-clock.
	Start time.Time
	End   time.Time

	// Step is the request resolution. Step == 0 is an instant query, never
	// time-slice routed.
	Step time.Duration
}

// Decision is the routing output. Slices are ordered oldest-first
// (composition order). A Decision is always produced — even when not routed —
// so the shadow header X-Cerberus-Route-Decision can report the reason.
type Decision struct {
	// Strategy is the decomposition strategy name — exactly
	// StrategyShardedTimeslice on a route, empty otherwise.
	Strategy string

	// K is the shard count on a route, 0 otherwise.
	K int

	// Reason is the shadow-header vocabulary value explaining the decision
	// (one of the Reason* consts).
	Reason string

	// Slices is the anchor-grid decomposition, populated only on a true
	// route (oldest-first). Empty when not routed.
	Slices []Slice

	// Cost grid — the RAW classifier scalars Planner.Plan already computed,
	// surfaced for the route A/B calibration corpus (stage 0). These are
	// populated for BOTH routed AND not-routed decisions: a route-A
	// (below-threshold) query must record its N/F/D too, because the
	// counterfactual overlap analysis compares route-A and route-B cost
	// distributions at equal (N, F, D). They are a purely additive readout
	// of values already derived in the eligibility pass — recording them
	// changes no routing behavior.
	//
	// The corpus buckets on these RAW scalars, never on Reason: ReasonHighD
	// folds into ReasonBelowThreshold on the not-routed shadow header, so the
	// Reason string alone hides the high-D class. NAnchors / Fanout /
	// CumulativeD / OuterRange / Step carry the unfolded signal.
	NAnchors    int           // N = OuterRange/Step + 1 (outermost spine)
	Fanout      int64         // F = max(Range/Step or Lookback/Step) over windows
	CumulativeD time.Duration // D = Σ spine lookback (Range / Lookback)
	OuterRange  time.Duration // OuterRange of the outermost spine
	Step        time.Duration // the request grid step
}

// StrategyShardedTimeslice is the only decomposition strategy emitted:
// disjoint sub-grids of the primary (anchor) dimension.
const StrategyShardedTimeslice = "sharded-timeslice"

// Reason vocabulary — the values that appear on the shadow header
// X-Cerberus-Route-Decision (docs §"The solver framework"). Every non-route
// path sets exactly one; a true route sets ReasonRouted.
const (
	// ReasonRouted: eligible AND cost thresholds cleared AND K >= 2 — the
	// plan routes B.
	ReasonRouted = "routed"

	// ReasonBelowThreshold: eligible but F < Fmin, N x F < MinAnchorPairs,
	// or the K clamp collapsed below 2 — not worth slicing.
	ReasonBelowThreshold = "below-threshold"

	// ReasonNotSliceable: some node in the plan is not registered
	// SliceInvariant (the signal-1 marker gate).
	ReasonNotSliceable = "not-sliceable"

	// ReasonInstant: an instant query (Step == 0 or OuterRange == 0) — no
	// anchor grid to slice.
	ReasonInstant = "instant"

	// ReasonHighD: the K clamp floor (K <= OuterRange / max(D, Step)) drove
	// K below 2 — too much cumulative spine lookback to slice.
	ReasonHighD = "high-D"

	// ReasonNow64: a now64 call appears somewhere (predicate, projection, or
	// ScalarSubquery.Input) — two statements would see different wall-clocks.
	ReasonNow64 = "now64"

	// ReasonGridMismatch: a windowed node's (Start, End, Step, OuterRange)
	// does not equal the grid the request predicts at that spine depth (an
	// @-pinned anchor).
	ReasonGridMismatch = "grid-mismatch"

	// ReasonIncommensurate: no slice quantum m in
	// [MinAnchorsPerSlice, N/2] satisfies m*Step = 0 (mod lcm of inner
	// resolutions) for a nested spine.
	ReasonIncommensurate = "incommensurate"

	// ReasonScalarHeavy: a ScalarSubquery whose interior scan-span x fan-out
	// exceeds the configured fraction of the outer plan — the slice benefit
	// cannot pay for replicating it.
	ReasonScalarHeavy = "scalar-heavy"

	// ReasonInstantJoin: an instant-mode (StepAligned==false) VectorJoin. The
	// VectorJoin node kind is registered slice-invariant, but the instant shape
	// synthesizes its join-side timestamp with now64(9) in the emitted SQL — a
	// wall-clock that diverges across shards and never reaches the plan-level
	// now64 scanner. Fails closed to route A; only the StepAligned (range-mode)
	// join, which step-aligns on the real per-anchor timestamp, routes B.
	ReasonInstantJoin = "instant-join"
)

// Slice is one shard of the anchor-grid decomposition. Bounds are
// anchor-grid-aligned; Plan is a re-anchored view of the optimized plan that
// SHARES the immutable off-spine subtrees with the original (only the
// O(spine-depth) re-gridded spine nodes are cloned).
type Slice struct {
	// Index is the position in the oldest-first composition order.
	Index int

	// Start / End are the slice's anchor-grid-aligned eval bounds. End sits
	// on the original grid; OuterRange = End - Start is a Step-multiple.
	Start time.Time
	End   time.Time

	// ScanFrom is the slice's input lower bound — solver-owned, offset- and
	// D-aware (docs §Decomposition). It is NOT inherited emitter behavior:
	// the matrix emitters are offset-blind, so the solver derives the scan
	// floor itself.
	ScanFrom time.Time

	// Plan is the re-anchored, share-immutable-off-spine view of the
	// optimized plan for this slice: only the windowed spine is cloned and
	// re-gridded; the off-spine subtrees are shared with the original (and
	// across sibling shards). The original plan is never mutated, and the
	// shards must not mutate a plan node in place (the no-mutate-after-slice
	// contract — see slicer.go and the immutability guards in the solver
	// tests). The solver runs each shard through emit only, which never does.
	Plan chplan.Node
}
