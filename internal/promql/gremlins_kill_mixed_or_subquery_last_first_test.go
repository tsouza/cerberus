// Tests in this file kill the LIVED gremlins mutants assigned to
// histogram_native_mixed_or_subquery_last_first.go from a phase4-promql-h
// mutation run (mutation.yml, PR #2727). See gremlins_kill_test.go for the
// shared file-header convention this file follows.
package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerMixedOrSubqueryLastFirstInput_RangeModePinnedTakesBroadcast
// kills the INVERT_LOGICAL mutant (`&&` -> `||`) at
// histogram_native_mixed_or_subquery_last_first.go:`if ctx.rangeMode() && !subqueryPinned(sub)`,
// inside lowerMixedOrSubqueryLastFirstInput:
//
//	if ctx.rangeMode() && !subqueryPinned(sub) {
//	    return lowerMixedOrSubqueryLastFirstRange(...)
//	}
//	... single-window path, which CrossJoins over a StepGrid when
//	    ctx.rangeMode() is also true (the pinned-broadcast case) ...
//
// A range-mode query over an `@`-pinned mixed-or subquery must take the
// single-window broadcast path (which builds a *chplan.CrossJoin), not
// the true per-anchor fan-out (lowerMixedOrSubqueryLastFirstRange, which
// never builds one). Flipping `&&` to `||` routes ANY range-mode query
// — pinned or not — to the fan-out lowering instead.
func TestLowerMixedOrSubqueryLastFirstInput_RangeModePinnedTakesBroadcast(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	pin := int64(1700000000)
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute, Timestamp: &pin}
	mixedRel := &chplan.Scan{Table: "dummy"}
	ctx := lowerCtx{start: start, end: end, step: time.Minute}

	plan, err := lowerMixedOrSubqueryLastFirstInput(mixedRel, sub, lastOverTimeWindowFn, s, ctx)
	if err != nil {
		t.Fatalf("lowerMixedOrSubqueryLastFirstInput: %v", err)
	}
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.CrossJoin); ok {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("expected a CrossJoin (broadcast path) for a range-mode query over an " +
			"@-pinned mixed-or subquery; mutant `&&`->`||` at " +
			"histogram_native_mixed_or_subquery_last_first.go:`if ctx.rangeMode() && !subqueryPinned(sub)` would route to the " +
			"true fan-out lowering instead")
	}
}

// TestMixedLastFirstAggs_PickDirection kills the CONDITIONALS_NEGATION
// mutant (`==` -> `!=`) at
// histogram_native_mixed_or_subquery_last_first.go:`if windowFn == firstOverTimeWindowFn`, inside mixedLastFirstAggs:
//
//	pick := latestArgMax
//	if windowFn == firstOverTimeWindowFn {
//	    pick = earliestArgMin
//	}
//
// last_over_time must pick via argMax (latest); first_over_time must
// pick via argMin (earliest) — the negation swaps both.
func TestMixedLastFirstAggs_PickDirection(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)

	last := mixedLastFirstAggs(lastOverTimeWindowFn, histSchema)
	if last[0].Fn != chplan.FnArgMax {
		t.Fatalf("last_over_time first agg Fn = %v, want FnArgMax (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_last_first.go:`if windowFn == firstOverTimeWindowFn`)", last[0].Fn)
	}
	first := mixedLastFirstAggs(firstOverTimeWindowFn, histSchema)
	if first[0].Fn != chplan.FnArgMin {
		t.Fatalf("first_over_time first agg Fn = %v, want FnArgMin (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_last_first.go:`if windowFn == firstOverTimeWindowFn`)", first[0].Fn)
	}
}

// TestMixedLastFirstProjection_PublishesZeroThresholdColumn kills the
// CONDITIONALS_NEGATION mutant (`!=` -> `==`) at
// histogram_native_mixed_or_subquery_last_first.go:`if histSchema.ZeroThresholdColumn != ""`,
// inside mixedLastFirstProjection — the last_first sibling of
// gremlins_kill_mixed_or_subquery_further_setop_test.go's identical
// splitMixedRelByDiscriminator kill: histSchema always comes from
// histogramProjectionSchema(s), which unconditionally sets
// ZeroThresholdColumn to a non-empty canonical alias, so under any real
// caller's histSchema the projection is always present and the negation
// would always skip it.
func TestMixedLastFirstProjection_PublishesZeroThresholdColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)
	input := &chplan.Scan{Table: "dummy"}

	plan := mixedLastFirstProjection(input, &chplan.LitInt{V: 1}, histSchema, s)
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	found := false
	for _, p := range proj.Projections {
		if p.Alias == histSchema.ZeroThresholdColumn {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %q projection; got %#v (mutant `!=`->`==` at "+
			"histogram_native_mixed_or_subquery_last_first.go:`if histSchema.ZeroThresholdColumn != \"\"`)",
			histSchema.ZeroThresholdColumn, proj.Projections)
	}
}
