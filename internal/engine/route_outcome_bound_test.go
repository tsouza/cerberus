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
// Tightening a guard to the measured OOM cliff converts a query that used to
// die on ClickHouse's own memory limit — which the memo learns from — into one
// rejected pre-flight. Unless that rejection is ALSO evidence, the honest bound
// would be strictly worse than the loose one: terminal for exactly the queries
// route B can answer.
//
// That argument holds for exactly the guards whose CEILING route B leaves
// alone. routeBExecCtx threads these three fan-out ceilings to the shards
// un-apportioned while each shard's window shrinks, so a shard can genuinely
// pass a bound the whole query failed. The two RangeBucketGridNative budgets
// used to be in this list and are NOT any more — #2705 made their ceilings
// K-apportioned, which makes their verdict K-invariant; see the sibling test
// below and timeSliceableResourceBoundMessages' own doc.
func TestClassifyRouteOutcome_TimeSliceableBoundsAreResourceFailures(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
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
// which is the half that can silently rot: adding a bound sharding cannot
// relieve to the sliceable set spends a scarce route-B dispatch to reproduce a
// verdict route A just produced.
//
// Three families qualify. A merge budget is driven by series cardinality and
// bucket width, which slicing splits anchors rather than series and so cannot
// touch. A shape fault is a user error no execution strategy resolves. And the
// two RangeBucketGridNative budgets are K-INVARIANT since #2705 apportioned
// their ceilings: a shard is judged by maxRows/K against a cost of
// groups x anchors/K, which is the identical inequality route A already
// failed (cerberus issue #3184).
func TestClassifyRouteOutcome_CardinalityBoundsAreNotEvidence(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		chplan.HistogramMergeBudgetMessage,
		chsql.RangeBucketGridNativeBudgetMessage,
		chsql.RangeBucketGridNativeDensityBudgetMessage,
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
