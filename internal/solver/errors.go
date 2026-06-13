package solver

import (
	"errors"
	"fmt"
)

// errSolverTimeout is the dedicated cancel cause for the wall-clock
// deadline (docs §Parallel #4). It is set as the WithCancelCause cause when
// Config.Timeout elapses, so a solver-chosen deadline is distinguishable
// from a real context.DeadlineExceeded (client / upstream deadline) and
// from a real shard error. Distinct cause => breaker-NEUTRAL => maps to a
// typed 504 at the handler. Matched via errors.Is.
var errSolverTimeout = errors.New("solver: shard execution exceeded wall-clock timeout")

// SolverTimeoutError is the concrete error a routed request surfaces when
// the wall-clock deadline elapses before all shards finish. It wraps
// errSolverTimeout (errors.Is matches) and carries the configured timeout
// so the handler can render a 504 with a useful message. It is
// breaker-neutral by construction: a gateway-chosen deadline is not a
// signal about ClickHouse health.
type SolverTimeoutError struct {
	// Timeout is the configured Config.Timeout the request exceeded.
	Timeout string
}

func (e *SolverTimeoutError) Error() string {
	return fmt.Sprintf("solver: routed request exceeded wall-clock timeout %s", e.Timeout)
}

func (e *SolverTimeoutError) Unwrap() error { return errSolverTimeout }

// errOutputCapExceeded is the sentinel matched (via errors.Is) when the
// composed shardCursor crosses Config.MaxOutputRows. Its message is
// DELIBERATELY distinct from upstream's max-samples text (chclient
// .ErrTooManySamples) — that text is a parity surface, and the output cap
// is a NEW, cerberus-specific 422 guarding the shared gateway heap against
// a high-cardinality success (docs §"Result composition"). Reusing the
// upstream wording would conflate two different limits and corrupt the
// max-samples parity assertion.
var errOutputCapExceeded = errors.New("solver: composed output row cap exceeded")

// OutputCapError is the concrete error shardCursor.Err() returns when the
// concatenated output crosses Config.MaxOutputRows. It wraps
// errOutputCapExceeded (errors.Is matches) and carries the cap. Maps to a
// distinct 422 at the handler — never the upstream max-samples message.
type OutputCapError struct {
	// Limit is the configured Config.MaxOutputRows the composed stream
	// crossed.
	Limit int64
}

func (e *OutputCapError) Error() string {
	// Intentionally NOT the upstream "result set exceeds N samples" text:
	// the output-rows cap is a distinct gateway-memory guard, and the
	// parity lanes assert the two messages never collide.
	return fmt.Sprintf("solver: composed result exceeds output-row cap of %d rows", e.Limit)
}

func (e *OutputCapError) Unwrap() error { return errOutputCapExceeded }

// ErrSolverEmit prefixes a SQL-emit failure during the pre-flight emit
// loop (docs §Parallel #1: "engine: emit:" classification preserved). An
// emit failure aborts the routed request with ZERO CH work. Distinct from
// a now64 assertion failure (errNow64InShardSQL) so the two can be told
// apart in classification.
var ErrSolverEmit = errors.New("solver: shard SQL emit failed")

// errNow64InShardSQL fires when a shard's emitted SQL string contains a
// now64( call despite the Planner's static now64 gate — the belt-and-braces
// assertion from docs §Routing #3 / §Parallel #1. Two statements resolving
// now64() independently would see different wall-clocks, breaking the
// disjoint-anchor parity argument, so the Executor refuses to run such a
// shard. It is an internal invariant violation (the Planner should already
// have routed it to A), surfaced as an error rather than silently honoured.
var errNow64InShardSQL = errors.New("solver: shard SQL contains now64( despite static gate")
