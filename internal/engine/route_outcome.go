package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
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
//   - a client/context cancellation that had not yet run for
//     costlyCancellationFloor — see that constant for why a LONG one is
//     evidence and a short one is not.
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
//
// This spelling passes elapsed == 0 ("not measured"). A call site that KNOWS
// how long the dispatch ran must use classifyRouteOutcomeAfter instead, or a
// costly cancellation silently reads as no evidence.
func classifyRouteOutcome(route routememo.Route, err error) routememo.Outcome {
	return classifyRouteOutcomeAfter(route, err, 0)
}

// costlyCancellationFloor is how long a dispatch must have been RUNNING before
// a client cancellation counts as cost evidence rather than a caller who simply
// went away.
//
// The distinction is real and this is the honest place to draw it. A dashboard
// panel abandoned at 200 ms says nothing about route cost; a query the client
// cut at 30 s had already committed 30 s of ClickHouse work and is exactly the
// failure this repo kept mistaking for "no evidence". Measured at realistic
// scale on a classic-histogram APM-style dashboard: of 16 real failures, 15
// were client cancellations (CH code 735) at ~30 s and only ONE was a
// memory-limit abort — so classifying every cancellation as no-evidence left
// the memo blind to 94% of what was actually going wrong.
//
// The floor is deliberately far above any plausible human abandonment and far
// below the 30 s deadline that produced those cancellations, so it separates
// the two populations without needing to know either timeout.
const costlyCancellationFloor = 5 * time.Second

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
	if isTimeSliceableResourceBound(err) {
		return routememo.OutcomeResourceFailure
	}
	// A cancellation that survived costlyCancellationFloor is evidence about
	// THIS route's cost: the work was committed and the caller gave up waiting
	// for it. Recorded, never retried — the caller is already gone, so a retry
	// dispatch would run for nobody (that is the doomed-trial shape
	// retryOnRouteAResourceFailure's ctx.Err() guard exists to prevent, which
	// is why that guard RECORDS this outcome before it returns). It teaches
	// the NEXT request instead.
	if elapsed >= costlyCancellationFloor &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
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

// timeSliceableResourceBoundMessages are the emitter-planted resource-bound
// guards whose bound is RELIEVED by evaluating a narrower time range — the
// only guards whose rejection is honest evidence that route B should be tried.
//
// Every one of these counts a cost that scales with the request's own anchor
// grid or with the raw rows its scan window admits, both of which a shard
// divides. Sharding a query that tripped one of them genuinely lowers each
// shard's cost below the same bound, which is exactly what makes the memo's
// A->B escalation the right response.
//
// Deliberately EXCLUDED, and the exclusion is the load-bearing half: the
// histogram-merge budgets (chplan.HistogramMergeBudgetMessage and its classic
// sibling) bound an ACROSS-SERIES merge whose cost is driven by series
// cardinality and bucket width, not by the time range. Time-slicing splits
// anchors, never series, so every shard would carry the same merged bucket
// range and trip the identical bound — an escalation that cannot succeed,
// spending a dispatch to fail again. Shape-fault guards (info() conflicting
// label, duplicate labelset, many-to-many match) are excluded for a stronger
// reason still: they are user errors that no execution strategy resolves.
var timeSliceableResourceBoundMessages = []string{
	chsql.RangeBucketGridNativeBudgetMessage,
	chsql.RangeBucketGridNativeDensityBudgetMessage,
	chsql.RangeBucketFanoutBudgetMessage,
	chsql.RangeLWRFanoutBudgetMessage,
	chsql.RateWindowFanoutBudgetMessage,
}

// isTimeSliceableResourceBound reports whether err is one of cerberus's own
// emitted resource-bound rejections that a narrower time range would satisfy.
//
// Why this has to exist at all. Before #2681 these guards were calibrated so
// loosely that a query which would exhaust memory usually sailed past them and
// died on ClickHouse's own limit instead — which classifies
// OutcomeResourceFailure, so the memo learned from it and escalated to route
// B. Tightening the bounds to the measured cliff moves that same query from
// "OOMs, then self-heals" to "rejected pre-flight", and a pre-flight rejection
// carried NO evidence: the guard would have become terminal for exactly the
// queries sharding can answer. Classifying it as the resource failure it
// plainly is keeps the self-healing path intact under an honest bound.
//
// Matching is on the decoded guard message with a PREFIX test, because
// ClickHouse appends its own "while executing 'FUNCTION throwIf(...)" trailer
// to the guard's literal. chclient.ThrowIfMessage is what supplies it, and it
// deliberately accepts both the wrapped and bare exception shapes: the row
// dial and the columnar (ch-go) dial surface the same rejection differently,
// and a classifier that recognised only one would leave the memo blind on
// whichever path it was not written against.
func isTimeSliceableResourceBound(err error) bool {
	guardMsg, ok := chclient.ThrowIfMessage(err)
	if !ok {
		return false
	}
	for _, msg := range timeSliceableResourceBoundMessages {
		if strings.HasPrefix(guardMsg, msg) {
			return true
		}
	}
	return false
}
