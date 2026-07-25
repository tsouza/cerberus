package engine

import (
	"errors"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// classifyRouteOutcome maps a dispatch's terminal error (nil on success)
// into exactly one of the three routememo.Outcome buckets the
// failure-driven route memo consumes (internal/routememo). Detection stays
// typed — every check is a sentinel already defined elsewhere for a
// different purpose (chclient's driver-level errors, the solver's own
// typed rejections) — never a string match, and never a NEW sentinel
// invented just for this classification.
//
// Only OutcomeSuccess and OutcomeResourceFailure are ever written into memo
// state (routememo.Memo.Observe); every other error classifies
// OutcomeNoEvidence, so an unrelated failure can never poison a routing
// verdict (Major-6):
//
//   - a solver-imposed wall-clock deadline (*solver.SolverTimeoutError),
//   - an open circuit breaker (chclient.ErrCircuitOpen) — no CH query ran,
//   - a dropped connection chclient already retried and exhausted
//     internally (it surfaces as a generic transport error carrying no
//     sentinel this function recognizes — by construction, not omission),
//   - the sample budget (chclient.ErrTooManySamples) — the CH query
//     finished; cerberus rejected the drain, which is not evidence about
//     ROUTE cost,
//   - a pre-flight SQL-emit failure (solver.ErrSolverEmit) — zero CH work
//     ran,
//   - client/context cancellation.
//
// The one resource-failure case unique to route B is its own gateway-heap
// guard: a composed shardCursor crossing Config.MaxOutputRows
// (*solver.OutputCapError) is a resource rejection of the same family as a
// ClickHouse-side memory-cap abort — both mean "this shape is too big to
// run the way it just ran" — so it is classified OutcomeResourceFailure
// exactly like chclient.ErrMemoryLimitExceeded. Route A structurally never
// raises OutputCapError (the cap only guards the composed multi-shard
// stream), so gating it on route==RouteB is belt-and-braces, not
// load-bearing.
func classifyRouteOutcome(route routememo.Route, err error) routememo.Outcome {
	if err == nil {
		return routememo.OutcomeSuccess
	}
	if errors.Is(err, chclient.ErrMemoryLimitExceeded) {
		return routememo.OutcomeResourceFailure
	}
	if route == routememo.RouteB {
		var capErr *solver.OutputCapError
		if errors.As(err, &capErr) {
			return routememo.OutcomeResourceFailure
		}
	}
	return routememo.OutcomeNoEvidence
}
