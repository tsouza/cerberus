package engine

import (
	"context"
	"errors"
	"time"

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
// costlyCancellationFloor is how long a dispatch must have been RUNNING before
// a client cancellation counts as cost evidence rather than a caller who simply
// went away.
//
// The distinction is real and this is the honest place to draw it. A dashboard
// panel abandoned at 200 ms says nothing about route cost; a query the client
// cut at 30 s had already committed 30 s of ClickHouse work and is exactly the
// failure this repo kept mistaking for "no evidence". Production measurement on
// the classic-histogram APM panel: of 16 real failures, 15 were client
// cancellations (CH code 735) at ~30 s and only ONE was a memory-limit abort —
// so classifying every cancellation as no-evidence left the memo blind to 94%
// of what was actually going wrong.
//
// The floor is deliberately far above any plausible human abandonment and far
// below the 30 s deadline that produced those cancellations, so it separates
// the two populations without needing to know either timeout.
const costlyCancellationFloor = 5 * time.Second

func classifyRouteOutcome(route routememo.Route, err error) routememo.Outcome {
	return classifyRouteOutcomeAfter(route, err, 0)
}

// classifyRouteOutcomeAfter is classifyRouteOutcome with the dispatch's elapsed
// wall time, so a cancellation that had already done real work classifies as
// cost evidence. `elapsed == 0` means "not measured" and preserves the original
// cancellation-is-no-evidence reading.
func classifyRouteOutcomeAfter(route routememo.Route, err error, elapsed time.Duration) routememo.Outcome {
	if err == nil {
		return routememo.OutcomeSuccess
	}
	if errors.Is(err, chclient.ErrMemoryLimitExceeded) {
		return routememo.OutcomeResourceFailure
	}
	// A cancellation that survived costlyCancellationFloor is evidence about
	// THIS route's cost: the work was committed and the caller gave up waiting
	// for it. Recorded, never retried — the caller is already gone, so a retry
	// dispatch would run for nobody (that is the doomed-trial shape this file's
	// ctx.Err() guard exists to prevent). It teaches the NEXT request instead.
	if elapsed >= costlyCancellationFloor && errors.Is(err, context.Canceled) {
		return routememo.OutcomeResourceFailure
	}
	if elapsed >= costlyCancellationFloor && errors.Is(err, context.DeadlineExceeded) {
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
