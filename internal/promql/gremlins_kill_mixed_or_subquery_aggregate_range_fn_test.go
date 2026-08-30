// Tests in this file kill the LIVED gremlins mutants assigned to
// histogram_native_mixed_or_subquery_aggregate_range_fn.go from a
// phase4-promql-h mutation run (mutation.yml, PR #2727). See
// gremlins_kill_test.go for the shared file-header convention this file
// follows.
//
// Two mutants on this file are NOT addressed with a dedicated test here —
// both are provably EQUIVALENT, not coverage gaps:
//
//   - :119:31, INVERT_LOGICAL (`||` -> `&&`) inside
//     sumOrAvgMixedOrSubqueryOuterFnRecognized's
//     `s.ExpHistogramTable == "" || ctx.metadataFullRange` guard. Under
//     metadataFullRange=true this function's own later call chain —
//     sumOrAvgOverMixedExpHistogramSetOp(sub.Expr, s, ctx), which calls
//     mixedExpHistogramSetOp(agg.Expr, s, ctx) directly — independently
//     rejects regardless of the mutation: mixedExpHistogramSetOp's own
//     lhsHist/rhsHist are each `isExpHistogramValuedShape(...) ||
//     isExpHistogramForwardedThroughSetOp(...)`, and every leaf
//     isExpHistogramValuedShape dispatches to (bareExpHistogramSelector
//     and friends) carries the identical metadataFullRange guard, so both
//     collapse to `false` whenever metadataFullRange is true — making
//     `lhsHist == rhsHist` (false == false) unconditionally true and
//     mixedExpHistogramSetOp reject regardless of what this function's own
//     early guard did. Verified directly: manually applying the mutation
//     and running `go test ./internal/promql/...` (this package) stays
//     green.
//
//   - :181:10, CONDITIONALS_BOUNDARY (`<` -> `<=`) inside
//     lowerSumOrAvgMixedOrSubqueryOuterFn:
//
//     step := sub.Step
//     if step == 0 {
//     step = defaultSubqueryStep
//     }
//     if step < 0 {
//
//     defaultSubqueryStep is the fixed positive constant time.Minute
//     (subquery.go), so step is NEVER literally 0 by the time the `< 0`
//     check runs — it is either defaultSubqueryStep (always > 0, when the
//     original sub.Step was exactly 0) or sub.Step unchanged (which the
//     real PromQL grammar can only produce as positive; a negative value
//     requires hand-building the AST, same as the parser bypass this
//     file's own sibling tests use elsewhere). `< 0` and `<= 0` agree on
//     every value except exactly 0, which this check can never observe —
//     verified directly: manually applying the mutation and running
//     `go test ./internal/promql/...` stays green.
package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSumOrAvgMixedOrSubqueryOuterFnRecognized_PositiveMatch kills two
// mutants on this file's line 119/122 at once:
//   - :119:25, CONDITIONALS_NEGATION (`==` -> `!=`) on
//     `s.ExpHistogramTable == ""`: with the negation, ANY non-empty
//     ExpHistogramTable (the normal, default schema) makes the guard
//     bail, so this test's ordinary DefaultOTelMetrics() schema alone
//     differentiates.
//   - :122:17, CONDITIONALS_NEGATION (`!=` -> `==`) on
//     `len(c.Args) != 1`: the real call always carries exactly 1
//     argument (the subquery), so the negation would reject it.
//
// Unlike the :119:31 INVERT_LOGICAL guard this file's own header documents
// as equivalent, neither of these two mutants is masked by the downstream
// mixedExpHistogramSetOp call: they fire BEFORE that call ever runs, on a
// perfectly ordinary, otherwise-recognisable positive-path query — so a
// plain ok=true assertion, with no metadataFullRange or arity trickery
// involved, cleanly kills both.
func TestSumOrAvgMixedOrSubqueryOuterFnRecognized_PositiveMatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	expr := mustParse(t, gremlinsMixedOrPinnedQuery("count_over_time"))
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("parsed %T, want *parser.Call", expr)
	}
	if _, ok := sumOrAvgMixedOrSubqueryOuterFnRecognized(call, s, lowerCtx{}); !ok {
		t.Fatalf("expected a well-formed 1-arg mixed-or subquery outer call to be recognised; "+
			"got ok=false (mutants at %s:119:25 and :122:17)", "histogram_native_mixed_or_subquery_aggregate_range_fn.go")
	}
}

// TestLowerSumOrAvgMixedOrSubqueryOuterFn_RangeModePinnedTakesBroadcastNotFanout
// kills the INVERT_LOGICAL mutant at :203:22, where
//
//	if ctx.rangeMode() && !subqueryPinned(sub) {
//	    return lowerSumOrAvgMixedOrSubqueryFoldFnRange(...)
//	}
//	return lowerSumOrAvgMixedOrSubqueryFoldFn(...)
//
// must route a range-mode query over an `@`-pinned mixed-or subquery to
// the single-window broadcast lowering (which builds a *chplan.CrossJoin
// over a StepGrid for the histogram side, see
// lowerHistFoldOverPureSubqueryBranch), not the true per-anchor fan-out.
// Flipping `&&` to `||` makes ctx.rangeMode() ALONE (true for any
// query_range call, pinned or not) route to the fan-out lowering instead —
// which never builds a CrossJoin.
//
// Must use a FOLD-family name (sum_over_time here) reaching this file's
// own `default` switch case — count_over_time and friends dispatch
// through lowerSumOrAvgMixedOrSubquerySelectFn instead and never reach
// this guard at all.
func TestLowerSumOrAvgMixedOrSubqueryOuterFn_RangeModePinnedTakesBroadcastNotFanout(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	expr := mustParse(t, gremlinsMixedOrPinnedQuery("sum_over_time"))

	plan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.CrossJoin); ok {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("expected a CrossJoin (the pinned single-window broadcast path) for a " +
			"range-mode query over an @-pinned mixed-or subquery; got none — mutant `&&`->`||` " +
			"at :203:22 would route to the true fan-out lowering instead")
	}
}

// TestLowerSumOrAvgMixedOrSubqueryFoldFn_StepAlignedTracksCtxStep kills
// both mutants at :283:76 (`ctx.step > 0`, CONDITIONALS_BOUNDARY `>`->`>=`
// and CONDITIONALS_NEGATION `>`->`<=`) inside lowerSumOrAvgMixedOrSubqueryFoldFn:
//
//	return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx.step > 0), nil
//
// stepAligned threads straight into the returned *chplan.VectorSetOp's own
// StepAligned field (combineMixedAggregateBranches / mixedOrShadowUnless,
// histogram_native_mixed_or_aggregate.go) with no other logic in between,
// so the root plan's StepAligned field is a direct, unmasked readout of
// this expression. An instant query (ctx.step == 0) must publish
// StepAligned=false; a range-mode `@`-pinned broadcast (ctx.step > 0, this
// function's OTHER reachable caller state — see :203:22's own guard just
// above) must publish StepAligned=true. Both CONDITIONALS_BOUNDARY
// (`>=`, true at step==0) and CONDITIONALS_NEGATION (`<=`, also true at
// step==0) disagree with the original ONLY at the instant-mode case, so a
// single differentiator (asserting false there) kills both; the
// range-mode case is included for symmetry and as a regression backstop.
func TestLowerSumOrAvgMixedOrSubqueryFoldFn_StepAlignedTracksCtxStep(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, gremlinsMixedOrPinnedQuery("sum_over_time"))

	instantPlan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt (instant): %v", err)
	}
	vso, ok := instantPlan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("instant plan = %T, want *chplan.VectorSetOp", instantPlan)
	}
	if vso.StepAligned {
		t.Fatalf("instant-mode plan StepAligned = true, want false (mutants at :283:76)")
	}

	end := at.Add(10 * time.Minute)
	rangePlan, err := LowerAtRange(context.Background(), expr, s, at, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange (pinned broadcast): %v", err)
	}
	vso, ok = rangePlan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("range plan = %T, want *chplan.VectorSetOp", rangePlan)
	}
	if !vso.StepAligned {
		t.Fatalf("range-mode pinned-broadcast plan StepAligned = false, want true")
	}
}

// TestLowerSumOrAvgMixedOrSubqueryFoldFnRange_StepAlignedFalseAtZeroStep
// kills all six mutants across :351:105, :352:107 and :360:72 (each a
// CONDITIONALS_BOUNDARY/CONDITIONALS_NEGATION pair on a `ctx.step > 0`
// expression) inside lowerSumOrAvgMixedOrSubqueryFoldFnRange:
//
//	histPure := mixedOrShadowUnless(histFoldedFanout, floatExists, true, chplan.VectorMatch{}, s, ctx.step > 0)   // :351
//	floatPure := mixedOrShadowUnless(floatFoldedFanout, histExists, false, chplan.VectorMatch{}, s, ctx.step > 0) // :352
//	return combineMixedAggregateBranches(histPure, floatPure, s, ctx.step > 0), nil                               // :360
//
// This function is only reached, in production, via
// lowerSumOrAvgMixedOrSubqueryOuterFn's own `ctx.rangeMode() &&
// !subqueryPinned(sub)` dispatch (:203:22 above), which guarantees
// ctx.step > 0 whenever this function runs — so BOTH mutant families
// (`>=` and `<=`) agree with the original `>` on every value this
// function's callER can ever supply, UNLESS this function is driven
// directly with ctx.step == 0 (bypassing the caller's own guard) — which
// is exactly the scenario `>=`/`<=` disagree with `>` on (0>=0 and 0<=0
// are both true; 0>0 is false). Reaching this function directly with a
// crafted ctx is the same "drive the unexported continuation directly"
// technique this package's own established convention uses throughout
// (e.g. the *_test.go files calling lowerXxxInput functions directly).
//
// stepAligned is threaded verbatim into each of the three
// *chplan.VectorSetOp nodes' own StepAligned field with no other logic —
// mixedOrShadowUnless / combineMixedAggregateBranches
// (histogram_native_mixed_or_aggregate.go) — so each of the three
// resulting nodes' StepAligned field is a direct, unmasked readout of its
// own call site's expression. The three nodes nest structurally:
// combineMixedAggregateBranches returns
// &VectorSetOp{Left: histOnly, Right: floatOnly, StepAligned: <:360>},
// where histOnly = &VectorSetOp{Left: histPure, Right: floatPure,
// StepAligned: <:360, reused>} — so :351/:352's own StepAligned values
// live one level further down, at histOnly.Left / histOnly.Right.
func TestLowerSumOrAvgMixedOrSubqueryFoldFnRange_StepAlignedFalseAtZeroStep(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	// A true fan-out shape: NOT pinned, so the outer dispatcher would
	// normally route here itself under a real query_range ctx.
	expr := mustParse(t, `sum_over_time((sum by (service) ((latency_exp_hist) or (other_metric)))[5m:1m])`)
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("parsed %T, want *parser.Call", expr)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	realCtx := lowerCtx{
		start: start, end: end, step: time.Minute,
		lowerers:       RangeLowerers{}.withDefaults(),
		resourceBounds: ResourceBounds{}.withDefaults(),
	}

	shape, ok := sumOrAvgMixedOrSubqueryOuterFnRecognized(call, s, realCtx)
	if !ok {
		t.Fatalf("expected recognition; got ok=false")
	}
	effStep := shape.sub.Step
	if effStep == 0 {
		effStep = defaultSubqueryStep
	}
	gridCtx, ok, err := subqueryGridCtx(shape.sub, effStep, realCtx)
	if err != nil || !ok {
		t.Fatalf("subqueryGridCtx: ok=%v err=%v", ok, err)
	}

	// The distinguishing input: drive the function directly with
	// ctx.step == 0 — unreachable through the normal dispatcher (which
	// only calls this function when ctx.rangeMode() is true), but exactly
	// the value the three mutant families disagree with the original on.
	zeroStepCtx := realCtx
	zeroStepCtx.step = 0

	plan, err := lowerSumOrAvgMixedOrSubqueryFoldFnRange(shape, gridCtx, s, zeroStepCtx)
	if err != nil {
		t.Fatalf("lowerSumOrAvgMixedOrSubqueryFoldFnRange: %v", err)
	}
	top, ok := plan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.VectorSetOp", plan)
	}
	if top.StepAligned {
		t.Fatalf("combine (mutants at :360:72) StepAligned = true, want false")
	}
	histOnly, ok := top.Left.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("top.Left = %T, want *chplan.VectorSetOp", top.Left)
	}
	histPure, ok := histOnly.Left.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("histOnly.Left = %T, want *chplan.VectorSetOp", histOnly.Left)
	}
	if histPure.StepAligned {
		t.Fatalf("histPure (mutants at :351:105) StepAligned = true, want false")
	}
	floatPure, ok := histOnly.Right.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("histOnly.Right = %T, want *chplan.VectorSetOp", histOnly.Right)
	}
	if floatPure.StepAligned {
		t.Fatalf("floatPure (mutants at :352:107) StepAligned = true, want false")
	}
}
