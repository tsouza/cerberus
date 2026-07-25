package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/solver"
)

// TestClassifyRouteOutcomeTable enumerates every reachable route-A and
// route-B error type and asserts it classifies into exactly the bucket
// docs/solver.md documents. This test must FAIL if a NoEvidence-class error
// is ever reclassified as a ResourceFailure (or vice versa) — that
// regression would either poison the memo with unrelated evidence
// (Major-6) or blind it to the one signal it exists to learn from.
func TestClassifyRouteOutcomeTable(t *testing.T) {
	cases := []struct {
		name  string
		route routememo.Route
		err   error
		want  routememo.Outcome
	}{
		{"nil success route A", routememo.RouteA, nil, routememo.OutcomeSuccess},
		{"nil success route B", routememo.RouteB, nil, routememo.OutcomeSuccess},

		{"CH memory-cap abort route A", routememo.RouteA, chclient.ErrMemoryLimitExceeded, routememo.OutcomeResourceFailure},
		{"CH memory-cap abort route B", routememo.RouteB, chclient.ErrMemoryLimitExceeded, routememo.OutcomeResourceFailure},
		{"wrapped CH memory-cap abort", routememo.RouteA, wrapErr(chclient.ErrMemoryLimitExceeded), routememo.OutcomeResourceFailure},

		{"route-B output cap crossed", routememo.RouteB, &solver.OutputCapError{Limit: 1}, routememo.OutcomeResourceFailure},
		{"route-B output cap crossed, wrapped", routememo.RouteB, wrapErr(&solver.OutputCapError{Limit: 1}), routememo.OutcomeResourceFailure},

		{"solver wall-clock deadline", routememo.RouteB, &solver.SolverTimeoutError{Timeout: "60s"}, routememo.OutcomeNoEvidence},
		{"circuit breaker open", routememo.RouteA, chclient.ErrCircuitOpen, routememo.OutcomeNoEvidence},
		{"sample budget exceeded", routememo.RouteA, chclient.ErrTooManySamples, routememo.OutcomeNoEvidence},
		{"solver emit failure", routememo.RouteB, solver.ErrSolverEmit, routememo.OutcomeNoEvidence},
		{"context canceled", routememo.RouteA, context.Canceled, routememo.OutcomeNoEvidence},
		{"context deadline exceeded", routememo.RouteA, context.DeadlineExceeded, routememo.OutcomeNoEvidence},
		{"generic transport error (broken conn already exhausted upstream)", routememo.RouteA, errors.New("read: connection reset by peer"), routememo.OutcomeNoEvidence},

		// Belt-and-braces route gate: OutputCapError is structurally
		// route-B-only, but classification must not accidentally treat it
		// as a resource failure if it somehow arrived tagged route A.
		{"output cap error mis-tagged route A", routememo.RouteA, &solver.OutputCapError{Limit: 1}, routememo.OutcomeNoEvidence},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRouteOutcome(tc.route, tc.err)
			if got != tc.want {
				t.Fatalf("classifyRouteOutcome(%v, %v) = %v, want %v", tc.route, tc.err, got, tc.want)
			}
		})
	}
}

func wrapErr(err error) error {
	return errWrapper{err}
}

type errWrapper struct{ inner error }

func (e errWrapper) Error() string { return "wrapped: " + e.inner.Error() }
func (e errWrapper) Unwrap() error { return e.inner }
