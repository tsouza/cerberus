// Package solver is the sharded-pushdown query orchestrator
// (docs/solver.md). It recognizes the narrow class of plans
// whose single-statement execution is memory-unbounded on ClickHouse —
// high anchor fan-out F = Range/Step — and re-anchors K deep copies of the
// already-optimized chplan onto disjoint slices of the anchor grid so each
// shard runs the same compat-gated SQL restricted to its anchor sub-grid.
//
// The package is built from:
//
//   - Config — the tuning surface, with one DefaultConfig and a fail-fast
//     Validate.
//   - Planner — pure, read-only eligibility classification of a
//     post-optimize plan into a Decision (the shadow-header signal).
//   - Slicer — the anchor-grid geometry that splits the eval grid into K
//     disjoint, on-grid slices and re-anchors a deep copy per slice.
//   - Executor — schedules the K shard queries (serial or bounded-parallel).
//   - shardCursor — composes the K shard result streams into one cursor.
//
// Import-cycle rule: internal/engine holds a *solver.Solver, so this package
// must NOT import internal/engine. The request metadata the Planner needs is
// carried by the package-local RequestMeta, populated by the engine adapter —
// never engine.Meta. This package imports only internal/chplan, the executor
// interfaces, and the standard library.
package solver

import (
	"fmt"
	"time"
)

// Routing modes (CERBERUS_EVAL_ROUTE). The force knob every test lane uses.
const (
	// ModeAuto routes an eligible plan only when it clears the cost
	// thresholds (Fmin, MinAnchorPairs, K >= 2). The production default
	// once the auto flip lands; classification still computes for every
	// plan so the shadow header is always populated.
	ModeAuto = "auto"

	// ModeSingle disables the solver entirely: the Planner still computes a
	// Decision (for the shadow header) but always returns routed=false, so
	// every request stays on route A. The library's ship-dark default.
	ModeSingle = "single"

	// ModeSharded drops the cost thresholds to the floor (K_min = 2) so
	// every ELIGIBLE plan routes; ineligible plans (un-sliceable, instant,
	// now64, grid-mismatch, ...) still stay on route A, so force-sharded
	// never breaks anything. The force knob the parity lanes run under.
	ModeSharded = "sharded"
)

// Config tunes the solver. Every field maps to a CERBERUS_* env var parsed by
// ConfigFromEnv (config_env.go) — kept in this package rather than
// internal/config to avoid an import cycle; this package owns the defaults and
// the invariants. The defaults are deliberately conservative against the
// over-routing attack (docs/solver.md §"Eligibility signals"): Grafana's
// auto-step makes the dominant production shape rate[5m] @ 15s hit F=20,
// N>=241, which must NOT route at these thresholds unless the total expansion
// is spike-class.
type Config struct {
	// Mode is "auto" | "single" | "sharded" (CERBERUS_EVAL_ROUTE).
	Mode string

	// MinFanout is Fmin (CERBERUS_SHARD_MIN_FANOUT): the minimum anchor
	// fan-out F = max(Range/Step) a plan must reach to be worth slicing.
	MinFanout int

	// MinAnchorPairs is the N x F product floor
	// (CERBERUS_SHARD_MIN_ANCHOR_PAIRS): the total expanded (sample, anchor)
	// pair count a plan must reach. The motivating spike had ~4820.
	MinAnchorPairs int

	// MaxK caps the shard count. It is a BACKSTOP, not the primary sizing
	// lever: K is already bounded above by N/MinAnchorsPerSlice (every slice
	// owns at least that many anchors) and by the high-D clamp, so MaxK only
	// binds on grids large enough that those two allow more. See
	// defaultMaxK for why the default is what it is.
	MaxK int

	// MinAnchorsPerSlice is the grid quantum: each slice must own at least
	// this many anchors (and never fewer than 2, the singleton-tail floor).
	MinAnchorsPerSlice int

	// Parallel is P, the per-request shard concurrency.
	Parallel int

	// Timeout (CERBERUS_SOLVER_TIMEOUT) bounds a routed request end-to-end.
	Timeout time.Duration

	// MaxOutputRows (CERBERUS_SHARD_MAX_OUTPUT_ROWS) caps the composed
	// per-request output rows with a new typed 422, so a high-cardinality
	// success cannot OOM the shared gateway heap.
	MaxOutputRows int64

	// AdaptiveEnabled (CERBERUS_SOLVER_ADAPTIVE_ENABLED, default TRUE) wires
	// the failure-driven route memo (internal/routememo, docs/solver.md
	// §"Failure-driven route memo"): a route-A dispatch that fails with
	// resource exhaustion is retried once on route B — transparently, so the
	// caller still gets an answer — and the outcome is remembered so future
	// cost-equivalent traffic routes directly.
	//
	// It is ON by default because it is the ONLY half of the routing decision
	// that reacts to what actually happened. ModeAuto's thresholds are a
	// PREDICTION made once, from plan shape alone; with this off, a wrong
	// prediction stays wrong forever and a route-A resource failure is a 5xx
	// rather than a slower answer. Turning it on is what makes ModeAuto mean
	// "start on route A and escalate on real evidence" rather than "guess up
	// front and never learn". Default-off was the old convention for a new
	// runtime-behavior-changing feature; the availability argument outweighs
	// it here, since the feature only ever turns a FAILURE into an answer.
	//
	// cmd/cerberus reads this field to decide whether to construct a
	// *routememo.Memo and wire it onto engine.Engine.RouteMemo; this package
	// itself never imports internal/routememo (see .go-arch-lint.yml —
	// routememo is importable by engine and cmd only).
	AdaptiveEnabled bool

	// RouteMemoEntryTTL (CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL) overrides how
	// long the route memo trusts a recorded verdict before it ages out,
	// replacing the routememo package's own default (routememo.Memo's
	// SetEntryTTL). Left at its Go zero value (0) here — the zero value
	// means "use the routememo package default", not "TTL of zero
	// duration": cmd/cerberus passes this straight to SetEntryTTL
	// unconditionally, and routememo's own SetEntryTTL treats a
	// non-positive input as a no-op, so an unset operator value can never
	// disable the memo's aging out. This field is the single source of
	// truth for the override; the routememo package's default constant is
	// not duplicated here.
	RouteMemoEntryTTL time.Duration

	// RouteMemoReValidationFraction
	// (CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION) overrides the
	// divisor that places the route memo's re-validation midpoint within
	// RouteMemoEntryTTL, replacing the routememo package's own default
	// (routememo.Memo's SetReValidationFraction). Left at its Go zero value
	// (0) here for the same reason as RouteMemoEntryTTL: cmd/cerberus passes
	// this straight to SetReValidationFraction unconditionally, and
	// routememo's own SetReValidationFraction no-ops on a non-positive
	// input.
	RouteMemoReValidationFraction int

	// EstimateNearEmptyRowFloor (CERBERUS_SHARD_ESTIMATE_NEAR_EMPTY_ROW_FLOOR,
	// issue #2787) is the advisory ADD-ON to the K clamp: when a
	// RequestMeta.Estimate is present and its total Rows falls at or below
	// this floor, classify treats the plan as advisory-near-empty and skips
	// sharding outright (ReasonEstimateNearEmpty), independent of what the
	// pure grid-geometry thresholds (MinFanout, MinAnchorPairs) would
	// otherwise have decided. This is the #2709 case named in defaultMaxK's
	// own doc: a wide-window panel over a table with almost nothing in it
	// clears every geometric threshold and pays K concurrent round trips for
	// no benefit — geometry alone cannot see this, only data can. See
	// planner.go's own doc for the exact comparison and
	// docs/solver.md's calibration numbers for why this default was chosen.
	//
	// A zero Estimate.Rows on a plan the index CAN prune fully (a disjoint
	// time range, an unmatched label) is expected and correctly near-empty;
	// this floor is deliberately small and additive so it never overrides
	// the existing cost thresholds for a genuinely dense plan — it only ever
	// REFUSES a route the geometry-only path would otherwise have taken, the
	// same fail-safe direction ReasonAnchorGridIndivisible already uses.
	EstimateNearEmptyRowFloor int64

	// MaxKWithEstimate (CERBERUS_SHARD_MAX_K_WITH_ESTIMATE, issue #2787) is
	// the ADVISORY ceiling K may reach when a RequestMeta.Estimate shows the
	// window is dense enough to justify it (EstimateMinRowsPerAdditionalShard
	// below) — the #2685 case named in defaultMaxK's own doc: a
	// production-cardinality panel whose OWN grid geometry asks for more
	// shards than the structural MaxK backstop allows, and whose real data
	// volume genuinely supports the extra sharding. It must be >= MaxK
	// (Validate enforces this); MaxK alone remains the ceiling whenever no
	// Estimate is present, so a deployment that never wires the chopt
	// FeatureExplainEstimate feature is byte-unchanged.
	MaxKWithEstimate int

	// EstimateMinRowsPerAdditionalShard
	// (CERBERUS_SHARD_ESTIMATE_MIN_ROWS_PER_ADDITIONAL_SHARD, issue #2787) is
	// the density floor each shard ABOVE MaxK must clear before the estimate
	// is allowed to raise K past the structural backstop: classify only
	// raises the ceiling to floor(Estimate.Rows / EstimateMinRowsPerAdditionalShard),
	// clamped to MaxKWithEstimate, so a shard the estimate cannot back with
	// real scan volume is never minted just because the grid geometry alone
	// would have asked for it. See docs/solver.md's calibration numbers.
	EstimateMinRowsPerAdditionalShard int64
}

// Default tuning constants (docs/solver.md).
const (
	defaultMinFanout      = 16
	defaultMinAnchorPairs = 4000
	// defaultMaxK was raised 8 -> 32 for #2685 and returned to 8 for #2709.
	// Both moves were right about the case in front of them, and the two cases
	// pull in opposite directions — so this constant is a TRADE, not a tuning,
	// and the next person to move it should know what they are giving up.
	//
	// WHY 32 (issue #2685). At 8 the ceiling was the BINDING constraint on
	// wide-window relief rather than a backstop: a 6h dashboard panel at real
	// production cardinality (253 series, bucket width 68, measured on
	// ClickHouse 26.6) derives K = N/MinAnchorsPerSlice = 22 from its own grid,
	// was truncated to 8, and at 8 each shard's scan — widened by the window
	// lookback — lands at 53.8M density cost units against a 54M ceiling. Over
	// by 0.4%, so the whole query failed while the sizing the solver had
	// already computed for itself would have fit.
	//
	// WHY BACK TO 8 (issue #2709). K derives from grid geometry alone, and
	// nothing in the decision looks at how much DATA sits behind the grid. A
	// 24h/1m panel is 1441 anchors and takes the full ceiling whether the table
	// holds a billion rows or almost none. Shards run CERBERUS_SHARD_PARALLEL
	// (3) at a time, so K=32 is ~11 sequential rounds against ~3 at K=8 — and
	// on a small or newly-provisioned deployment those eight extra rounds are
	// pure round-trip overhead for a table with nothing to divide. The e2e
	// dashboard sweep measured that panel at 19s and Grafana's proxy cut it,
	// on exactly the wide-window shape #2677 set out to make usable.
	//
	// So #2685's failure mode returns at production cardinality: a panel whose
	// own grid asks for K>8 is clipped again, and one of those was measured
	// 0.4% over the density ceiling. An operator hitting it raises
	// CERBERUS_SHARD_MAX_K, which is the knob that exists for exactly this.
	// The durable fix is to bound K by the work available rather than by the
	// grid quantum, so neither deployment size has to be traded for the other —
	// tracked in #2709.
	defaultMaxK               = 8
	defaultMinAnchorsPerSlice = 16
	defaultParallel           = 3
	defaultTimeout            = 60 * time.Second
	defaultMaxOutputRows      = 2_000_000

	// defaultEstimateNearEmptyRowFloor, defaultMaxKWithEstimate and
	// defaultEstimateMinRowsPerAdditionalShard are issue #2787's advisory
	// EXPLAIN ESTIMATE thresholds. Calibrated against
	// test/perf/nightly/sentinels.go's real-shaped near-empty and dense
	// fixtures (docs/solver.md §"Advisory EXPLAIN ESTIMATE calibration") —
	// see that section for the measured before/after query counts these
	// values were chosen from, mirroring how defaultMaxK's own doc pins its
	// value to a measured density-ceiling table rather than a guess.
	//
	// A near-empty floor of 1,000 rows is two orders of magnitude below the
	// smallest MinAnchorPairs-clearing production shape measured
	// (docs/solver.md, ~4,820 anchor-pairs) — small enough that no
	// legitimately worthwhile fan-out is ever mistaken for empty, and large
	// enough to catch the #2709 case (a panel whose table has "almost
	// nothing" in it) outright rather than only after it has already scanned
	// a few hundred rows across K shards.
	defaultEstimateNearEmptyRowFloor = 1_000

	// defaultMaxKWithEstimate raises the structural MaxK=8 backstop to 32 —
	// exactly the #2685 ceiling that was reverted to 8 for #2709 (defaultMaxK's
	// own doc) — but ONLY when EstimateMinRowsPerAdditionalShard is also
	// cleared, so the #2685 win is available again without reopening #2709's
	// regression: geometry alone can no longer ask for it, only geometry
	// PLUS a real, measured data volume can.
	defaultMaxKWithEstimate = 32

	// defaultEstimateMinRowsPerAdditionalShard: each shard above MaxK must be
	// backed by at least 50,000 estimated rows (a granule-resolution UPPER
	// bound, so the real figure is typically lower). At the #2685 incident's
	// own measured shape (K=22 asked for by geometry, clipped to 8, each
	// shard's widened scan landing at 53.8M density cost units against a 54M
	// ceiling — defaultMaxK's own doc), the total estimated scan comfortably
	// clears 22 x 50,000 = 1,100,000 rows, so this floor raises K for
	// exactly that incident's shape while still refusing to mint a shard for
	// a window whose estimate cannot back even one more.
	defaultEstimateMinRowsPerAdditionalShard = 50_000
)

// DefaultConfig returns the conservative library defaults. Mode defaults to
// "single" — the solver ships dark — so DefaultConfig is safe to wire as the
// in-process default without enabling routing.
func DefaultConfig() Config {
	return Config{
		Mode:               ModeSingle,
		MinFanout:          defaultMinFanout,
		MinAnchorPairs:     defaultMinAnchorPairs,
		MaxK:               defaultMaxK,
		MinAnchorsPerSlice: defaultMinAnchorsPerSlice,
		Parallel:           defaultParallel,
		Timeout:            defaultTimeout,
		MaxOutputRows:      defaultMaxOutputRows,
		AdaptiveEnabled:    true,

		EstimateNearEmptyRowFloor:         defaultEstimateNearEmptyRowFloor,
		MaxKWithEstimate:                  defaultMaxKWithEstimate,
		EstimateMinRowsPerAdditionalShard: defaultEstimateMinRowsPerAdditionalShard,
	}
}

// Validate fail-fast checks the solver-internal invariants. The pool / gate /
// P arithmetic (docs/solver.md §"Execution and cursor model") lives in
// chclient + internal/config, outside this package, so it is intentionally
// NOT validated here — only the constraints the Planner and Slicer in this
// package depend on.
//
// The Mode check applies in every mode (an unknown route knob is a
// misconfiguration regardless). The numeric invariants apply unconditionally
// too: even "single" computes a classification, so the thresholds must be
// self-consistent.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeAuto, ModeSingle, ModeSharded:
	default:
		return fmt.Errorf("solver: invalid Mode %q (want %q, %q, or %q)",
			c.Mode, ModeAuto, ModeSingle, ModeSharded)
	}
	if c.Parallel < 1 {
		return fmt.Errorf("solver: Parallel (P) must be >= 1, got %d", c.Parallel)
	}
	if c.MaxK < 2 {
		return fmt.Errorf("solver: MaxK must be >= 2, got %d", c.MaxK)
	}
	if c.MinAnchorsPerSlice < 2 {
		return fmt.Errorf("solver: MinAnchorsPerSlice must be >= 2, got %d", c.MinAnchorsPerSlice)
	}
	if c.MinFanout < 1 {
		return fmt.Errorf("solver: MinFanout (Fmin) must be >= 1, got %d", c.MinFanout)
	}
	if c.MaxOutputRows <= 0 {
		return fmt.Errorf("solver: MaxOutputRows must be > 0, got %d", c.MaxOutputRows)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("solver: Timeout must be > 0, got %s", c.Timeout)
	}
	if c.MaxKWithEstimate < c.MaxK {
		return fmt.Errorf("solver: MaxKWithEstimate (%d) must be >= MaxK (%d)", c.MaxKWithEstimate, c.MaxK)
	}
	if c.EstimateNearEmptyRowFloor < 0 {
		return fmt.Errorf("solver: EstimateNearEmptyRowFloor must be >= 0, got %d", c.EstimateNearEmptyRowFloor)
	}
	if c.EstimateMinRowsPerAdditionalShard < 1 {
		return fmt.Errorf("solver: EstimateMinRowsPerAdditionalShard must be >= 1, got %d", c.EstimateMinRowsPerAdditionalShard)
	}
	return nil
}
