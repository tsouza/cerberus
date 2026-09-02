package chsql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// This file pins the exact input-validation behaviour of
// emitRangeBucketGridNative's precondition guards (range_bucket_grid_native.go)
// so gremlins (phase2 scope ./internal/chsql) cannot flip a guard's boundary
// or short-circuit operator without a test going red. Every existing
// coverage of this emitter comes from happy-path TXTAR specs and chDB
// integration tests, none of which ever construct a plan that fails these
// guards — so a mutation that only changes the REJECTED shapes never showed
// up as a test failure. Each test below names the guard line it defends.

// validGridNativePlan returns the minimal chplan.RangeBucketGridNative that
// clears every one of emitRangeBucketGridNative's precondition guards, so
// each test below can flip exactly one field to the value the guard under
// test is supposed to reject.
func validGridNativePlan() *chplan.RangeBucketGridNative {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeBucketGridNative{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{"SeriesID", "TimeUnix", "BucketCounts", "ExplicitBounds"},
		},
		Start:             start,
		End:               start.Add(9 * time.Minute),
		Step:              time.Minute,
		Range:             5 * time.Minute,
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "SeriesID"}},
		GroupByAliases:    []string{"SeriesID"},
		AnchorAlias:       "anchor_ts",
		TimestampCol:      "TimeUnix",
		BucketCountsCol:   "BucketCounts",
		ExplicitBoundsCol: "ExplicitBounds",
	}
}

// requireGridNativeRejected fails t unless emitting node returns an error
// wrapping chsql.ErrUnsupported.
func requireGridNativeRejected(t *testing.T, node *chplan.RangeBucketGridNative) {
	t.Helper()
	_, _, err := chsql.Emit(context.Background(), node)
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if !errors.Is(err, chsql.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

// TestEmitRangeBucketGridNativeRejectsZeroStep defends
// range_bucket_grid_native.go:`if r.Step <= 0`.
//
// Mutation CONDITIONALS_BOUNDARY turns `<=` into `<`, so Step == 0 no
// longer trips the guard: the emitter would proceed to build a grid whose
// anchor spacing is zero, an interval no downstream fold or arrayJoin range
// can express correctly.
func TestEmitRangeBucketGridNativeRejectsZeroStep(t *testing.T) {
	t.Parallel()
	node := validGridNativePlan()
	node.Step = 0
	requireGridNativeRejected(t, node)
}

// TestEmitRangeBucketGridNativeRejectsZeroRange defends
// range_bucket_grid_native.go:`if r.Range <= 0`.
//
// Mutation CONDITIONALS_BOUNDARY turns `<=` into `<`, so Range == 0 no
// longer trips the guard: the emitter would proceed to build a lookback
// window of zero width, which can never see the two samples the rate fold
// requires.
func TestEmitRangeBucketGridNativeRejectsZeroRange(t *testing.T) {
	t.Parallel()
	node := validGridNativePlan()
	node.Range = 0
	requireGridNativeRejected(t, node)
}

// TestEmitRangeBucketGridNativeRejectsPartialZeroStartOrEnd defends
// range_bucket_grid_native.go:`if r.Start.IsZero() || r.End.IsZero()`.
//
// Mutation INVERT_LOGICAL turns `||` into `&&`, so the guard only fires
// when BOTH Start and End are zero. A plan with exactly one endpoint zero
// — the pinned grid missing only its lower or upper bound — would no
// longer be rejected under the mutant. Each subtest zeroes exactly one
// endpoint, which the `&&` mutant lets through.
func TestEmitRangeBucketGridNativeRejectsPartialZeroStartOrEnd(t *testing.T) {
	t.Parallel()
	t.Run("start zero", func(t *testing.T) {
		t.Parallel()
		node := validGridNativePlan()
		node.Start = time.Time{}
		requireGridNativeRejected(t, node)
	})
	t.Run("end zero", func(t *testing.T) {
		t.Parallel()
		node := validGridNativePlan()
		node.End = time.Time{}
		requireGridNativeRejected(t, node)
	})
}

// TestEmitRangeBucketGridNativeRejectsPartialEmptyCols defends
// range_bucket_grid_native.go:`if r.BucketCountsCol == "" || r.ExplicitBoundsCol == ""`
// (`if r.BucketCountsCol == "" || r.ExplicitBoundsCol == ""`).
//
// Mutation INVERT_LOGICAL turns `||` into `&&`, so the guard only fires
// when BOTH columns are unset. A plan naming only one of the two Array
// columns the classic-histogram read needs would no longer be rejected
// under the mutant. Each subtest empties exactly one column name.
func TestEmitRangeBucketGridNativeRejectsPartialEmptyCols(t *testing.T) {
	t.Parallel()
	t.Run("bucket counts empty", func(t *testing.T) {
		t.Parallel()
		node := validGridNativePlan()
		node.BucketCountsCol = ""
		requireGridNativeRejected(t, node)
	})
	t.Run("explicit bounds empty", func(t *testing.T) {
		t.Parallel()
		node := validGridNativePlan()
		node.ExplicitBoundsCol = ""
		requireGridNativeRejected(t, node)
	})
}
