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

// Config tunes the solver. Every field maps to a CERBERUS_* env var wired by
// internal/config in a later PR; this package owns only the defaults and the
// invariants. The defaults are deliberately conservative against the
// over-routing attack (docs §Routing): Grafana's auto-step makes the dominant
// production shape rate[5m] @ 15s hit F=20, N>=241, which must NOT route at
// these thresholds unless the total expansion is spike-class.
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

	// MaxK caps the shard count.
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

	// MemoryApportion (CERBERUS_SHARD_MEMORY_APPORTION): when true, the
	// per-shard max_memory_usage is cap/P (256 MiB floor), holding total
	// exposure at exactly the single-query cap.
	MemoryApportion bool

	// Autotune enables the self-driving threshold loop
	// (CERBERUS_SOLVER_AUTOTUNE, default true). When true AND Mode == ModeAuto,
	// a background loop periodically refits MinFanout / MinAnchorPairs from the
	// router corpus, certifies the candidate off-policy against the OOM floor,
	// and hot-reloads it into the Planner (Planner.SetThresholds). Disabled pins
	// the thresholds at their configured values — byte-identical to a
	// fixed-threshold build. It has no effect outside ModeAuto: single and
	// sharded carry no cost gate to tune.
	Autotune bool

	// AutotuneInterval is the cadence of the self-driving loop
	// (CERBERUS_SOLVER_AUTOTUNE_INTERVAL, default 15m). Ignored when Autotune is
	// false or Mode != ModeAuto.
	AutotuneInterval time.Duration
}

// Default tuning constants (docs §Routing / §"The solver framework").
const (
	defaultMinFanout          = 16
	defaultMinAnchorPairs     = 4000
	defaultMaxK               = 8
	defaultMinAnchorsPerSlice = 16
	defaultParallel           = 3
	defaultTimeout            = 60 * time.Second
	defaultMaxOutputRows      = 2_000_000
	defaultAutotune           = true
	defaultAutotuneInterval   = 15 * time.Minute
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
		MemoryApportion:    false,
		Autotune:           defaultAutotune,
		AutotuneInterval:   defaultAutotuneInterval,
	}
}

// Validate fail-fast checks the solver-internal invariants. The pool / gate /
// P arithmetic (docs §Parallel #9) lives in chclient + internal/config, which
// this PR does not wire, so it is intentionally NOT validated here — only the
// constraints the Planner and Slicer in this package depend on.
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
	// AutotuneInterval only has to be self-consistent when the loop can run
	// (Autotune && auto mode); a stale interval on a disabled loop is harmless,
	// but validating unconditionally keeps the failure at startup, not at the
	// first tick.
	if c.Autotune && c.AutotuneInterval <= 0 {
		return fmt.Errorf("solver: AutotuneInterval must be > 0 when Autotune is set, got %s", c.AutotuneInterval)
	}
	return nil
}
