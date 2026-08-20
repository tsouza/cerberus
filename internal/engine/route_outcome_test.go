package engine

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestClassifyRouteOutcome_CostlyCancellationIsEvidence pins the change that
// makes the failure-driven memo able to see the failure mode that real
// traffic actually exhibits.
//
// Before this, ONLY a ClickHouse memory-limit abort counted as evidence. On the
// classic-histogram APM-style dashboard that missed 15 of 16 real failures: they arrived
// as client cancellations (CH code 735) at ~30 s, which classified NoEvidence,
// so the memo learned nothing no matter how many times the panel failed.
//
// The floor is what keeps this honest — a caller who navigates away at 200 ms
// must stay NoEvidence, because that says nothing about route cost.
func TestClassifyRouteOutcome_CostlyCancellationIsEvidence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		err     error
		elapsed time.Duration
		want    routememo.Outcome
	}{
		{
			name:    "cancelled after real work is cost evidence",
			err:     context.Canceled,
			elapsed: costlyCancellationFloor,
			want:    routememo.OutcomeResourceFailure,
		},
		{
			name:    "deadline exceeded after real work is cost evidence",
			err:     context.DeadlineExceeded,
			elapsed: 30 * time.Second,
			want:    routememo.OutcomeResourceFailure,
		},
		{
			name:    "caller went away early is NOT evidence",
			err:     context.Canceled,
			elapsed: 200 * time.Millisecond,
			want:    routememo.OutcomeNoEvidence,
		},
		{
			name:    "unmeasured elapsed keeps the original reading",
			err:     context.Canceled,
			elapsed: 0,
			want:    routememo.OutcomeNoEvidence,
		},
		{
			name:    "success is still success",
			err:     nil,
			elapsed: time.Minute,
			want:    routememo.OutcomeSuccess,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRouteOutcomeAfter(routememo.RouteA, tc.err, tc.elapsed); got != tc.want {
				t.Fatalf("classifyRouteOutcomeAfter(%v, %v) = %v, want %v", tc.err, tc.elapsed, got, tc.want)
			}
		})
	}
}
