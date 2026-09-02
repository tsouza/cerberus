// Tests in this file kill the LIVED gremlins mutants reported on
// date_fns.go's [readsRangeSampleTimestamp] guard, from the phase4-promql-d
// leg (cerberus issue #2949). See gremlins_kill_test.go for the shared
// file-header convention this file follows.
package promql

import (
	"testing"
	"time"
)

// readsRangeSampleTimestampStep is a step long enough to be unambiguously
// positive; the guard reads only its sign, so the exact value is immaterial.
const readsRangeSampleTimestampStep = 30 * time.Second

// TestReadsRangeSampleTimestamp_OnlyTheTimestampFunction kills the
// INVERT_LOGICAL mutant on the FIRST `||` of
//
//	date_fns.go:`name != "timestamp" || ctx.step <= 0 || ctx.inRangeVector`
//
// All three of this guard's mutants live on that one construct, so each note
// names the operator it rewrites rather than a position within the line.
//
// Call the three disqualifications A (wrong function), B (not a range
// evaluation) and C (inside a range-vector descent). Go binds `&&` tighter
// than `||`, so rewriting the first `||` yields `(A && B) || C` rather than a
// re-association of the original.
//
// The guard is a disjunction of three INDEPENDENT disqualifications, and any
// one of them is enough: this rewrite applies only to `timestamp`, only in
// range mode, and only outside a range-vector descent. `day_of_week` trips A
// alone, with B and C both false — so the original bails and the mutant, which
// now needs B as well, does not. The function then walks on and reports that a
// `day_of_week` call reads the range sample's own timestamp, which is a
// different function's semantics entirely.
//
// (The mutant on the SECOND `||` of the same construct becomes `A || (B && C)`
// for the same precedence reason, so it is A that keeps working and B that stops
// disqualifying alone. That one is killed by
// TestReadsRangeSampleTimestamp_RequiresPositiveStep below, whose input trips
// B alone.)
func TestReadsRangeSampleTimestamp_OnlyTheTimestampFunction(t *testing.T) {
	t.Parallel()

	arg := mustParse(t, `some_metric`)
	ctx := lowerCtx{step: readsRangeSampleTimestampStep}

	if !readsRangeSampleTimestamp("timestamp", arg, ctx) {
		t.Fatal("positive control: readsRangeSampleTimestamp(\"timestamp\", <selector>, step>0) = " +
			"false; the negative assertion below would then hold for the wrong reason")
	}
	if readsRangeSampleTimestamp("day_of_week", arg, ctx) {
		t.Fatal("readsRangeSampleTimestamp(\"day_of_week\", <selector>, step>0) = true; only " +
			"`timestamp` reads the range sample's own timestamp (the `||`->`&&` mutants on " +
			"date_fns.go:`name != \"timestamp\" || ctx.step <= 0 || ctx.inRangeVector` both " +
			"stop the name check disqualifying on its own)")
	}
}

// TestReadsRangeSampleTimestamp_RequiresPositiveStep kills TWO mutants on
//
//	date_fns.go:`name != "timestamp" || ctx.step <= 0 || ctx.inRangeVector`
//
// both of which stop a zero step disqualifying on its own:
//
//   - CONDITIONALS_BOUNDARY, `ctx.step <= 0` -> `ctx.step < 0`.
//   - INVERT_LOGICAL on the SECOND `||`, which under Go's precedence
//     (`&&` binds tighter) yields `A || (B && C)`: the step disqualification B
//     now needs the range-vector descent C alongside it.
//
// A zero step is the instant-query lowering (see [lowerCtx.step]'s own
// documentation: "step == 0 means" this is not a range evaluation), and there
// is no per-step grid for a sample timestamp to be read against. The input
// below trips B alone — `timestamp`, step 0, no range-vector descent — which
// is exactly where each mutant diverges: the boundary shift makes B false
// because a step is never negative, and the `&&` makes B insufficient because
// C is false.
func TestReadsRangeSampleTimestamp_RequiresPositiveStep(t *testing.T) {
	t.Parallel()

	arg := mustParse(t, `some_metric`)

	if !readsRangeSampleTimestamp("timestamp", arg, lowerCtx{step: readsRangeSampleTimestampStep}) {
		t.Fatal("positive control: readsRangeSampleTimestamp(\"timestamp\", <selector>, step>0) = " +
			"false; the negative assertion below would then hold for the wrong reason")
	}
	if readsRangeSampleTimestamp("timestamp", arg, lowerCtx{step: 0}) {
		t.Fatal("readsRangeSampleTimestamp(\"timestamp\", <selector>, step=0) = true; a zero step " +
			"is the instant lowering, which has no per-step grid to read a sample timestamp " +
			"against (the `<=`->`<` mutant on " +
			"date_fns.go:`name != \"timestamp\" || ctx.step <= 0 || ctx.inRangeVector` " +
			"admits it, since a step is never negative)")
	}
}
