package engine

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/routememo"
)

// throwIfErr builds the error shape chclient surfaces for an emitted
// throwIf guard: the guard's own message plus the "while executing" trailer
// ClickHouse appends, which is why the classifier prefix-matches.
func throwIfErr(msg string) error {
	return &chclient.ThrowIfError{
		Message: msg + ": while executing 'FUNCTION throwIf(...)'",
		Cause:   errors.New("code: 395"),
	}
}

// TestClassifyRouteOutcome_TimeSliceableBoundsAreResourceFailures pins the
// classification #2681's tightened bounds depend on.
//
// Tightening the density guard to the measured OOM cliff converts a query
// that used to die on ClickHouse's own memory limit — which the memo learns
// from — into one rejected pre-flight. Unless that rejection is ALSO evidence,
// the honest bound would be strictly worse than the loose one: terminal for
// exactly the queries route B can answer.
func TestClassifyRouteOutcome_TimeSliceableBoundsAreResourceFailures(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		chsql.RangeBucketGridNativeBudgetMessage,
		chsql.RangeBucketGridNativeDensityBudgetMessage,
		chsql.RangeBucketFanoutBudgetMessage,
		chsql.RangeLWRFanoutBudgetMessage,
		chsql.RateWindowFanoutBudgetMessage,
	} {
		got := classifyRouteOutcome(routememo.RouteA, throwIfErr(msg))
		if got != routememo.OutcomeResourceFailure {
			t.Errorf("guard %q classified %v, want OutcomeResourceFailure — a bound a narrower "+
				"time range would satisfy must drive the memo toward route B, or tightening it "+
				"makes the guard terminal", msg, got)
		}
	}
}

// TestClassifyRouteOutcome_CardinalityBoundsAreNotEvidence pins the exclusion,
// which is the half that can silently rot: adding a merge budget to the
// sliceable set would spend a route-B dispatch on a bound sharding cannot
// relieve, since slicing splits anchors and the merge cost is driven by series
// cardinality and bucket width.
func TestClassifyRouteOutcome_CardinalityBoundsAreNotEvidence(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		chplan.HistogramMergeBudgetMessage,
		"some future shape-fault guard nobody has classified",
	} {
		got := classifyRouteOutcome(routememo.RouteA, throwIfErr(msg))
		if got != routememo.OutcomeNoEvidence {
			t.Errorf("guard %q classified %v, want OutcomeNoEvidence — time-slicing cannot "+
				"relieve it, so escalating to route B would spend a dispatch to fail again",
				msg, got)
		}
	}
}
