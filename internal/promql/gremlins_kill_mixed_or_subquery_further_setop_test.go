// Tests in this file kill the LIVED gremlins mutants assigned to
// histogram_native_mixed_or_subquery_further_setop_range_fn.go from a
// phase4-promql-h mutation run (mutation.yml, PR #2727). See
// gremlins_kill_test.go for the shared file-header convention this file
// follows.
package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// mixedPairAggAlias walks n looking for a *chplan.Aggregate whose
// AggFuncs carries alias — used below to tell whether a plan took the
// mixed-aware resets/changes path ([mixedPairCountAggs], which is the
// all-histogram [expHistogramPairCountAggs] widened by two more
// groupArrays keyed by mixedPairValueArrayAlias / mixedPairDiscrArrayAlias)
// or the plain all-histogram path (which never collects either).
func mixedPairAggAlias(n chplan.Node, alias string) bool {
	found := false
	chplan.Walk(n, func(node chplan.Node) bool {
		if a, ok := node.(*chplan.Aggregate); ok {
			for _, agg := range a.AggFuncs {
				if agg.Alias == alias {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// mixedDiscriminatorProjected walks n looking for a *chplan.Project
// publishing chplan.MixedDiscriminatorColumn — the Mixed-row-shape
// signature ([mixedLastFirstProjection] publishes it; the plain
// all-histogram [lowerSelectFnOverExpHistogramSubqueryInput] path never
// does).
func mixedDiscriminatorProjected(n chplan.Node) bool {
	found := false
	chplan.Walk(n, func(node chplan.Node) bool {
		if p, ok := node.(*chplan.Project); ok {
			for _, proj := range p.Projections {
				if proj.Alias == chplan.MixedDiscriminatorColumn {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// TestLowerHistogramOrMixedSubqueryOuterFnInput_LastFirstShapeDispatch
// kills the CONDITIONALS_NEGATION mutant (`==` -> `!=`) on the
// `if shape == chplan.HistogramRowShape` guard of
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case lastOverTimeWindowFn, firstOverTimeWindowFn:`,
// inside lowerHistogramOrMixedSubqueryOuterFnInput:
//
//	if shape == chplan.HistogramRowShape {
//	    node, err = lowerSelectFnOverExpHistogramSubqueryInput(...)
//	    ...
//	}
//	node, err = lowerMixedOrSubqueryLastFirstInput(...)
//
// A HistogramRowShape input must route to the plain histogram
// continuation (never publishes chplan.MixedDiscriminatorColumn); a
// MixedRowShape input must route to the Mixed-aware continuation (always
// does, via [mixedLastFirstProjection]). The negation swaps both.
func TestLowerHistogramOrMixedSubqueryOuterFnInput_LastFirstShapeDispatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := lowerCtx{start: at, end: at}
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute}
	inner := &chplan.Scan{Table: "dummy"}

	histPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.HistogramRowShape, lastOverTimeWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("histogram-shape: %v", err)
	}
	if mixedDiscriminatorProjected(histPlan) {
		t.Fatalf("histogram-shape last_over_time took the Mixed path (mutant `==`->`!=` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case lastOverTimeWindowFn, firstOverTimeWindowFn:`)")
	}

	mixedPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.MixedRowShape, lastOverTimeWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("mixed-shape: %v", err)
	}
	if !mixedDiscriminatorProjected(mixedPlan) {
		t.Fatalf("mixed-shape last_over_time took the Histogram path (mutant `==`->`!=` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case lastOverTimeWindowFn, firstOverTimeWindowFn:`)")
	}
}

// TestLowerHistogramOrMixedSubqueryOuterFnInput_ResetsChangesShapeDispatch
// kills the CONDITIONALS_NEGATION mutant (`==` -> `!=`) on
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case resetsWindowFn, changesWindowFn:`, the
// resets/changes sibling of the last_over_time/first_over_time dispatch
// above. Both shapes ultimately produce the same four-column canonical
// Project, so the two paths are told apart by whether the underlying
// Aggregate collected the Mixed-only groupArrays
// [mixedPairCountAggs] adds.
func TestLowerHistogramOrMixedSubqueryOuterFnInput_ResetsChangesShapeDispatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := lowerCtx{start: at, end: at}
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute}
	inner := &chplan.Scan{Table: "dummy"}

	histPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.HistogramRowShape, resetsWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("histogram-shape: %v", err)
	}
	if mixedPairAggAlias(histPlan, mixedPairValueArrayAlias) {
		t.Fatalf("histogram-shape resets took the Mixed path (mutant `==`->`!=` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case resetsWindowFn, changesWindowFn:`)")
	}

	mixedPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.MixedRowShape, resetsWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("mixed-shape: %v", err)
	}
	if !mixedPairAggAlias(mixedPlan, mixedPairValueArrayAlias) {
		t.Fatalf("mixed-shape resets took the Histogram path (mutant `==`->`!=` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case resetsWindowFn, changesWindowFn:`)")
	}
}

// TestLowerHistogramOrMixedSubqueryOuterFnInput_FoldShapeDispatch kills
// the CONDITIONALS_NEGATION mutant (`==` -> `!=`) on
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:`, the
// FOLD-family (rate/increase/delta/...) sibling of the two dispatches
// above. Over a MixedRowShape input this case routes to
// [lowerFurtherWrapMixedOrSubqueryFoldFn], whose own
// [combineMixedAggregateBranches] always returns a *chplan.VectorSetOp at
// the root — a shape the plain-histogram continuation
// ([lowerExpHistogramRangeFnOverSubqueryInput]) never returns.
func TestLowerHistogramOrMixedSubqueryOuterFnInput_FoldShapeDispatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := lowerCtx{start: at, end: at}
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute}
	inner := &chplan.Scan{Table: "dummy"}

	histPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.HistogramRowShape, rateWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("histogram-shape: %v", err)
	}
	if _, ok := histPlan.(*chplan.VectorSetOp); ok {
		t.Fatalf("histogram-shape rate took the Mixed path (mutant `==`->`!=` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:`)")
	}

	mixedPlan, _, err := lowerHistogramOrMixedSubqueryOuterFnInput(inner, chplan.MixedRowShape, rateWindowFn, sub, s, ctx)
	if err != nil {
		t.Fatalf("mixed-shape: %v", err)
	}
	if _, ok := mixedPlan.(*chplan.VectorSetOp); !ok {
		t.Fatalf("mixed-shape rate = %T, want *chplan.VectorSetOp (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:`)", mixedPlan)
	}
}

// TestLowerFurtherWrapMixedOrSubqueryFoldFn_StepAligned kills both
// mutants on the `ctx.step > 0` argument (CONDITIONALS_BOUNDARY `>`->`>=`
// and CONDITIONALS_NEGATION `>`->`<=`) of
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx.step > 0), nil`,
// inside lowerFurtherWrapMixedOrSubqueryFoldFn:
//
// stepAligned threads directly into the returned *chplan.VectorSetOp's own
// StepAligned field with no other logic in between — see this file's
// sibling gremlins_kill_mixed_or_subquery_aggregate_range_fn_test.go for
// the identical shape. An instant ctx (step==0) must publish
// StepAligned=false; a real query_range ctx (step>0) must publish
// StepAligned=true — both `>=` and `<=` disagree with the original `>`
// only at step==0, so the instant case alone kills both.
func TestLowerFurtherWrapMixedOrSubqueryFoldFn_StepAligned(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute}
	mixedRel := &chplan.Scan{Table: "dummy_mixed_rel"}

	instantCtx := lowerCtx{start: at, end: at}
	plan, err := lowerFurtherWrapMixedOrSubqueryFoldFn(mixedRel, sub, rateWindowFn, s, instantCtx)
	if err != nil {
		t.Fatalf("instant: %v", err)
	}
	vso, ok := plan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.VectorSetOp", plan)
	}
	if vso.StepAligned {
		t.Fatalf("instant-mode (step==0) StepAligned = true, want false (mutants on the `ctx.step > 0` argument of " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx.step > 0), nil`)")
	}

	rangeCtx := lowerCtx{start: at, end: at.Add(10 * time.Minute), step: time.Minute}
	plan, err = lowerFurtherWrapMixedOrSubqueryFoldFn(mixedRel, sub, rateWindowFn, s, rangeCtx)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	vso, ok = plan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.VectorSetOp", plan)
	}
	if !vso.StepAligned {
		t.Fatalf("range-mode (step>0) StepAligned = false, want true")
	}
}

// TestSplitMixedRelByDiscriminator_PublishesZeroThresholdColumn kills the
// CONDITIONALS_NEGATION mutant (`!=` -> `==`) at
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`if histSchema.ZeroThresholdColumn != ""`,
// inside splitMixedRelByDiscriminator:
//
//	if histSchema.ZeroThresholdColumn != "" {
//	    histProjs = append(histProjs, ...)
//	}
//
// histSchema always comes from histogramProjectionSchema(s)
// (histogram_native_sum.go), which unconditionally sets ZeroThresholdColumn
// to a non-empty canonical alias — the same "defensive guard, always true
// in practice" shape histogram_native_binop.go's own
// histogramBinopMergeProjections documents for its identical check — so
// under any real caller's histSchema, the projection is always present,
// and the negation would always skip it.
func TestSplitMixedRelByDiscriminator_PublishesZeroThresholdColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)
	mixedRel := &chplan.Scan{Table: "dummy_mixed_rel"}

	histBranch, _ := splitMixedRelByDiscriminator(mixedRel, histSchema, s)
	proj, ok := histBranch.(*chplan.Project)
	if !ok {
		t.Fatalf("histBranch = %T, want *chplan.Project", histBranch)
	}
	found := false
	for _, p := range proj.Projections {
		if p.Alias == histSchema.ZeroThresholdColumn {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %q projection in histBranch; got %#v (mutant `!=`->`==` at "+
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`if histSchema.ZeroThresholdColumn != \"\"`)",
			histSchema.ZeroThresholdColumn, proj.Projections)
	}
}

// TestLowerFloatFoldOverSubqueryInput_InstantPinnedNoCrossJoin kills the
// INVERT_LOGICAL mutant (`&&` -> `||`) at
// histogram_native_mixed_or_subquery_further_setop_range_fn.go:`if ctx.rangeMode() && subqueryPinned(sub)`,
// inside lowerFloatFoldOverSubqueryInput:
//
//	if ctx.rangeMode() && subqueryPinned(sub) {
//	    return wrapRangeWindowAtBroadcast(...) // builds a CrossJoin over a StepGrid
//	}
//
// An instant query (ctx.rangeMode()==false) over an `@`-pinned subquery
// must NOT build a CrossJoin — there is no grid to fan across. Flipping
// `&&` to `||` makes subqueryPinned(sub) alone satisfy the condition
// regardless of mode.
func TestLowerFloatFoldOverSubqueryInput_InstantPinnedNoCrossJoin(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pin := int64(1700000000)
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute, Timestamp: &pin}
	input := &chplan.Scan{Table: "dummy"}
	ctx := lowerCtx{start: at, end: at} // instant: rangeMode() == false.
	anchor := evalAnchor{End: at}

	plan := lowerFloatFoldOverSubqueryInput(input, sub, rateWindowFn, anchor, s, ctx)
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.CrossJoin); ok {
			found = true
		}
		return true
	})
	if found {
		t.Fatalf("instant-mode pinned subquery built a CrossJoin (mutant `&&`->`||` at " +
			"histogram_native_mixed_or_subquery_further_setop_range_fn.go:`if ctx.rangeMode() && subqueryPinned(sub)`)")
	}
}
