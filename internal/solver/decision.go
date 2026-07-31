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
	// surfaced for the route A/B calibration corpus. These are
	// populated for BOTH routed AND not-routed decisions: a route-A
	// (below-threshold) query must record its N/F/D too, because the
	// counterfactual overlap analysis compares route-A and route-B cost
	// distributions at equal (N, F, D). They are a purely additive readout
	// of values already derived in the eligibility pass — recording them
	// changes no routing behavior.
	//
	// The corpus buckets on these RAW scalars, never on Reason. Reason is
	// recorded verbatim and does distinguish the classes from each other —
	// below-threshold, high-D, and the structural refusals are separate
	// tokens — but it says nothing about WHERE a plan sat relative to the
	// threshold that refused it, which is the quantity a counterfactual
	// re-fit needs. NAnchors / Fanout / CumulativeD / OuterRange / Step carry
	// that. (What does fold is the ROUTE token: every non-route reason
	// collapses to "A".)
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
// X-Cerberus-Route-Decision (docs/solver.md §"Eligibility signals"). Every
// non-route path sets exactly one; a true route sets ReasonRouted.
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

	// ReasonRoutingDisabled: Cfg.Mode is "single" — the operator switched
	// routing off deployment-wide, so the plan was classified but no cost
	// threshold was ever consulted. Distinct from ReasonBelowThreshold, which
	// asserts a threshold WAS evaluated and the plan fell under it: a corpus
	// row is evidence about where the threshold sits only in the latter case.
	ReasonRoutingDisabled = "routing-disabled"

	// ReasonInstantJoin: an instant-mode (StepAligned==false) VectorJoin. The
	// VectorJoin node kind is registered slice-invariant, but the instant shape
	// synthesizes its join-side timestamp with now64(9) in the emitted SQL — a
	// wall-clock that diverges across shards and never reaches the plan-level
	// now64 scanner. Fails closed to route A; only the StepAligned (range-mode)
	// join, which step-aligns on the real per-anchor timestamp, routes B.
	ReasonInstantJoin = "instant-join"

	// ReasonExtractionFailed: the plan walk found no [chplan.GridCarrier] it
	// could measure, so the Decision's cost grid is all zeros because NOTHING
	// WAS MEASURED — not because the plan is cheap. Every other refusal
	// asserts "the features were extracted and they lost"; without this token
	// that claim is unfalsifiable, since an unmeasured plan and a genuinely
	// zero-cost one produce the identical corpus row. It also covers a carrier
	// kind added to chplan ahead of this package's geometry table: such a plan
	// fails closed to route A and says so, instead of routing on invented
	// numbers.
	ReasonExtractionFailed = "extraction-failed"
)

// Reasons is the complete Reason vocabulary above, in declaration order.
//
// It exists because the vocabulary is MIRRORED outside this package — every
// Decision reaches the calibration corpus, and internal/routerrules re-declares
// the token set as the closed domain of the decision_reason column so a rule may
// filter on it. That mirror is a hand-maintained wire contract (routerrules
// deliberately imports neither the solver nor optcorpus), so it needs one
// enumerable source to be pinned against; adding a Reason* const without adding
// it here is what the lockstep test in that package catches.
var Reasons = []string{
	ReasonRouted,
	ReasonBelowThreshold,
	ReasonNotSliceable,
	ReasonInstant,
	ReasonHighD,
	ReasonNow64,
	ReasonGridMismatch,
	ReasonIncommensurate,
	ReasonScalarHeavy,
	ReasonRoutingDisabled,
	ReasonInstantJoin,
	ReasonExtractionFailed,
}

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

	// Plan is the re-anchored, share-immutable-off-spine view of the
	// optimized plan for this slice: only the windowed spine is cloned and
	// re-gridded; the off-spine subtrees are shared with the original (and
	// across sibling shards). The original plan is never mutated, and the
	// shards must not mutate a plan node in place (the no-mutate-after-slice
	// contract — see slicer.go and the immutability guards in the solver
	// tests). The solver runs each shard through emit only, which never does.
	Plan chplan.Node
}
