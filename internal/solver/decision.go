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

	// Estimate is the OPTIONAL advisory scan read-out for this plan's inner
	// scan, or nil when no estimate was fetched — the overwhelmingly common
	// case: internal/engine's explain_estimate_wiring.go and
	// cardinality_probe_wiring.go each only probe a ModeAuto-eligible
	// candidate, cache the result per plan shape (the cardinality probe keyed
	// additionally on metric — see that file's own doc), and skip their round
	// trip entirely once the route memo or the per-rung admission learner
	// already holds a verdict for that shape. A nil Estimate leaves
	// classify's K derivation byte-identical to the pre-#2787 pure-geometry
	// formula — see planner.go's own doc on where and how a non-nil Estimate
	// is consulted.
	//
	// Keeping Planner pure (its own doc: "no per-request mutable state, no
	// RNG... a pure function of (plan, meta, Cfg)") is exactly why this rides
	// in as a value on RequestMeta instead of the Planner performing the
	// round trip itself: the I/O already happened, by the caller, before
	// Plan/Classify/Eligible ever runs.
	Estimate *ScanEstimate
}

// ScanEstimate is the package-local advisory read-out of a ClickHouse scan
// probe — EXPLAIN ESTIMATE (issue #2787, internal/chclient.ScanEstimate) or
// the bounded cardinality pre-probe (issue #2788,
// internal/chclient.CardinalityEstimate) — the transport counterparts
// internal/engine converts from (see RequestMeta.Estimate's own doc for why
// solver does not import chclient's types directly, mirroring this whole
// struct's "package-local stand-in for engine.Meta" convention). classify()
// consumes every field only as a bias input to the K clamp, never as a
// threshold a plan must clear to be eligible at all: every structural/
// correctness gate above runs unconditionally, with or without an Estimate.
type ScanEstimate struct {
	// Parts / Marks are EXPLAIN ESTIMATE-only: the number of parts / granules
	// the index analysis selected. Left zero when only the cardinality
	// pre-probe populated this Estimate — classify() never reads either
	// field, so a zero here changes no routing outcome; they exist for
	// telemetry/corpus readers.
	Parts uint64
	Marks uint64

	// Rows is a row-count bias input to the K clamp (planner.go's own doc).
	// EXPLAIN ESTIMATE populates it as a GRANULE-RESOLUTION UPPER BOUND
	// (selected marks times the table's granule size, typically 8192), never
	// selectivity-aware. The cardinality pre-probe populates it with a REAL
	// count() over the plan's already-pruned scan window instead — strictly
	// more precise for the same K-clamp arithmetic — and internal/engine's
	// cardinality_probe_wiring.go prefers that real count whenever both
	// probes ran for the same request (see its own mergeCardinalityEstimate
	// doc).
	Rows uint64

	// DistinctSeries is the cardinality pre-probe's uniqUpTo(100)(...)
	// read-out (issue #2788) — the number of distinct series backing the
	// scan window, up to the uniqUpTo(100) cap (see chplan.FnUniqUpTo's own
	// doc for the saturation behaviour above it). Zero when the cardinality
	// pre-probe did not run (EXPLAIN ESTIMATE alone never populates this
	// field — it has no comparable per-series signal). classify() does not
	// read this field at all: it exists solely for
	// internal/engine/cardinality_probe_wiring.go's own per-rung admission
	// seeding, which needs the fan-out signal EXPLAIN ESTIMATE's row-only
	// upper bound cannot provide.
	DistinctSeries uint64
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

	// PerRungPredictive is true exactly when this route came from ModeAuto's
	// per-rung bypass (planner.go's perRung branch, gated on
	// minAnchorsForPerRungShard): F is unmeasurable for a per-rung carrier
	// (carrierGeometry.perRungIntermediate), so admission read the anchor
	// axis alone rather than MinFanout/MinAnchorPairs, which cannot see how
	// much DATA backs the grid.
	//
	// This is a pure, additive readout of the SAME classification Plan
	// already computed — it changes no routing behavior by itself. It exists
	// so a caller (internal/engine's per-rung admission refinement) can tell
	// "routed because a real cost threshold was cleared" apart from "routed
	// because geometry alone cleared a bar that cannot see cost", and apply
	// evidence-based scrutiny only to the latter population. False on every
	// other route and on every non-route.
	PerRungPredictive bool
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

	// ReasonAnchorGridIndivisible: eligible and above every cost threshold,
	// but a carrier on the spine answers false to
	// chplan.GridCarrier.AnchorGridDivides — its peak intermediate is
	// Theta(rows x Lookback/Step), constant in the grid width, so slicing the
	// anchor grid replicates that work per shard instead of partitioning it.
	// The thresholds cannot see this: they read F = Lookback/Step as a
	// divisor, and for such a carrier it is a redundancy multiplier, so the
	// proxy's sign is inverted and a HIGH F is evidence against routing.
	//
	// Measured on ClickHouse 26.6, classic-histogram fan-out spine
	// at Step=15s / Lookback=5m / OuterRange=1h, K=12: route B cost 23x the
	// ClickHouse work (185,101 ms vs 8,070 ms) and read 36x the rows, to
	// recover 8.7% of a perfectly-divisible peak.
	//
	// ModeAuto only. Eligible() ignores it, so a genuine route-A resource
	// failure still routes through the failure-driven memo: the model sets the
	// prior, measurement overrides it.
	ReasonAnchorGridIndivisible = "anchor-grid-indivisible"

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

	// ReasonIncommensurate: the slice quantum emitted from the selected K does
	// not satisfy m*Step = 0 (mod lcm of end-phased nested resolutions) — the
	// nested grids whose anchors are generated backward from their own End.
	// Epoch-aligned (StepAlign) nested grids are phase-invariant and never
	// raise this.
	ReasonIncommensurate = "incommensurate"

	// ReasonScalarHeavy: a ScalarSubquery / InSubquery whose interior carries
	// a windowed node the Planner cannot prove anchor-compatible with the
	// grid predicted at the point it is embedded — see
	// Planner.scalarInteriorAnchorCompatible — so replicating it K× is not
	// provably bounded by what the outer spine's own per-shard share already
	// costs.
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

	// ReasonEstimateNearEmpty: eligible and above every pure-geometry cost
	// threshold, but an advisory EXPLAIN ESTIMATE (issue #2787,
	// RequestMeta.Estimate) showed the window's total scan is at or below
	// Config.EstimateNearEmptyRowFloor — real DATA, not grid geometry, says
	// sharding this window would pay K concurrent round trips for
	// negligible work. Like ReasonAnchorGridIndivisible this is a cost
	// verdict ABOVE the geometry thresholds, not evidence about where
	// MinFanout / MinAnchorPairs themselves sit. Only set when
	// meta.Estimate is non-nil — the overwhelmingly common nil case leaves
	// this reason unreachable and every other Reason's meaning unchanged.
	ReasonEstimateNearEmpty = "estimate-near-empty"

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
	ReasonAnchorGridIndivisible,
	ReasonNotSliceable,
	ReasonInstant,
	ReasonHighD,
	ReasonNow64,
	ReasonGridMismatch,
	ReasonIncommensurate,
	ReasonScalarHeavy,
	ReasonRoutingDisabled,
	ReasonInstantJoin,
	ReasonEstimateNearEmpty,
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
