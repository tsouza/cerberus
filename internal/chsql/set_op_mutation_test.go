package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// This file pins the control-flow and boundary behaviour of the fused `&&`
// single-pass shape in set_op.go (fusableIntersect / emitFusedIntersect /
// splitCommonConjuncts) so the gremlins mutation lane cannot flip a
// loop-control token or a comparison there without a test going red. Each
// test names the set_op.go line it defends.

// setOpWindowEpoch is the shared request-window bound both arms of the
// factoring test carry — the same conjunct on both sides is exactly the
// windowed shape Grafana sends, and it is what splitCommonConjuncts exists
// to factor out.
const setOpWindowEpoch = 1700000000

// fusedIntersectScan returns the one spans Scan every fusable arm reads.
func fusedIntersectScan() *chplan.Scan { return &chplan.Scan{Table: "otel_traces"} }

// fusedIntersect wraps two arms in the `&&` set op the single-pass gate
// serves.
func fusedIntersect(l, r chplan.Node) *chplan.SetOperation {
	return &chplan.SetOperation{
		Left:          l,
		Right:         r,
		Op:            chplan.SetIntersect,
		TraceIDColumn: "TraceId",
		SpanIDColumn:  "SpanId",
	}
}

// fusedIntersectArm returns `Filter(<col> = <val>) → Scan`, the bare-selector
// arm shape fusableIntersect accepts.
func fusedIntersectArm(col, val string) chplan.Node {
	return &chplan.Filter{
		Input:     fusedIntersectScan(),
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: col}, Right: &chplan.LitString{V: val}},
	}
}

// fusedIntersectWindowedArm returns the same arm under a shared request-window
// Filter: `Filter(<col> = <val>) → Filter(Timestamp >= <epoch>) → Scan`. Both
// arms built this way carry a byte-identical window conjunct, which is the
// input splitCommonConjuncts factors.
func fusedIntersectWindowedArm(col, val string) chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Filter{
			Input:     fusedIntersectScan(),
			Predicate: &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: setOpWindowEpoch}},
		},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: col}, Right: &chplan.LitString{V: val}},
	}
}

// TestMutation_FusedIntersect_UnfilteredArmDoesNotEndTheGateLoop defends
// set_op.go:436 (the `continue` that skips an arm whose residual predicate is
// nil while emitting the QUALIFY gate for every other arm).
//
// Mutation INVERT_LOOPCTRL turns that `continue` into `break`, which abandons
// the whole gate loop at the FIRST nil residual instead of skipping just that
// arm. `{} && {SpanName="x"}` puts the nil residual FIRST (the unfiltered arm
// matches every span, so it contributes no restriction and no gate), so the
// mutant emits no QUALIFY at all — turning a trace-gated intersect into a bare
// `SELECT *` over the spans table that returns every span of every trace,
// including traces the right arm never matched.
func TestMutation_FusedIntersect_UnfilteredArmDoesNotEndTheGateLoop(t *testing.T) {
	t.Parallel()

	plan := fusedIntersect(fusedIntersectScan(), fusedIntersectArm("SpanName", "x"))
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	const gate = "QUALIFY max((`SpanName` = ?)) OVER (PARTITION BY `TraceId`)"
	if !strings.Contains(sql, gate) {
		t.Fatalf("filtered arm lost its trace gate after the unfiltered arm was skipped; want %q in:\n%s", gate, sql)
	}
}

// TestMutation_SplitCommonConjuncts_FactorsTheSharedConjunct defends
// set_op.go:`if len(common) == 0` (`if len(common) == 0 { return nil, arms }`, the "nothing is
// shared, keep every arm whole" bail-out).
//
// Mutation CONDITIONALS_NEGATION turns `== 0` into `!= 0`, inverting the
// bail-out: a set of arms that DOES share a conjunct is returned unfactored,
// and a set that shares nothing falls through the residual loop instead. The
// unfactored shape is the regression splitCommonConjuncts' godoc describes —
// the restriction becomes ONE opaque `(<window> AND <p1>) OR (<window> AND
// <p2>)` conjunct from which no window bound can be promoted to PREWHERE, and
// each gate re-tests the window every surviving row has already passed.
//
// Two assertions distinguish the shapes. The gates must carry the RESIDUAL
// alone (the mutant renders `max(((`Timestamp` >= ?) AND (`SpanName` = ?)))`),
// and the shared conjunct must appear exactly ONCE in the statement (the
// mutant repeats it four times: twice in the disjunction, once per gate).
func TestMutation_SplitCommonConjuncts_FactorsTheSharedConjunct(t *testing.T) {
	t.Parallel()

	plan := fusedIntersect(
		fusedIntersectWindowedArm("SpanName", "x"),
		fusedIntersectWindowedArm("StatusCode", "Error"),
	)
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	for _, gate := range []string{
		"QUALIFY max((`SpanName` = ?)) OVER (PARTITION BY `TraceId`)",
		"AND max((`StatusCode` = ?)) OVER (PARTITION BY `TraceId`)",
	} {
		if !strings.Contains(sql, gate) {
			t.Errorf("trace gate must carry the arm's residual alone; want %q in:\n%s", gate, sql)
		}
	}

	const shared = "`Timestamp` >= ?"
	if got := strings.Count(sql, shared); got != 1 {
		t.Errorf("shared conjunct %q rendered %d times, want 1 (factored out of both the disjunction and the gates):\n%s", shared, got, sql)
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// set_op.go:`i < j` (CONDITIONALS_BOUNDARY, `i < j` -> `i <= j` in
// filterChainOverScan's in-place reversal of the collected Filter
// predicates). The extra iteration the mutant runs is the one where
// `i == j`, whose body is `preds[i], preds[j] = preds[j], preds[i]` with
// both indices equal. Go evaluates the whole right-hand side before
// assigning, so that swap reads one element and writes the same value back
// to it; the slice is unchanged, and the next update makes `i > j` under
// either comparison, so the loop then ends. For every input length the
// mutant produces a byte-identical `preds` and therefore an identical
// conjunction — only the iteration count differs.
//
// set_op.go:`break` (INVERT_LOOPCTRL, `break` -> `continue` in
// splitCommonConjuncts' shared-conjunct scan). The break guards a boolean
// latch: `shared[i]` is set true once before the inner loop and the loop
// body's only write sets it false. Continuing instead of breaking keeps
// scanning the remaining arms, but no arm can set the flag back to true, so
// the post-loop value of `shared[i]` — and hence `common`, the residuals and
// the emitted SQL — is identical. Only the number of containsExpr calls
// differs.
