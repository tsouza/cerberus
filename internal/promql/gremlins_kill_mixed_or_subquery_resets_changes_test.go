// Tests in this file kill the LIVED gremlins mutants assigned to
// histogram_native_mixed_or_subquery_resets_changes.go from a
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

// TestMixedPairCountStage_ProjectionsCapacityIsTight kills the
// ARITHMETIC_BASE mutant (`+` -> `-`) on mixedPairCountStage's
// slice-capacity hint,
// histogram_native_mixed_or_subquery_resets_changes.go:`projs := make([]chplan.Projection, 0, len(keyAliases)+1)`.
//
// The function appends exactly one projection per keyAliases entry plus
// one more for the value column — len(keyAliases)+1 appends total — so
// the original capacity is an exact fit; flipping `+` to `-`
// under-allocates, forcing a reallocation that (for this fixture's size)
// lands on a different cap() than the original — mirrors this package's
// established slice-capacity mutation-kill pattern (gremlins_kill_test.go).
// Two keyAliases (rather than one) are used because Go's growth schedule
// for this element size happens to coincide at exactly one entry,
// masking the difference — verified directly by trying both sizes.
func TestMixedPairCountStage_ProjectionsCapacityIsTight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)
	input := &chplan.Scan{Table: "dummy"}
	keyAliases := []string{"step_anchor", "attrs_alias"}

	node := mixedPairCountStage(input, resetsWindowFn, keyAliases, histSchema)
	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("node = %T, want *chplan.Project", node)
	}
	const want = 3 // 2 keyAliases + 1 value projection
	if got := len(proj.Projections); got != want {
		t.Fatalf("len(Projections) = %d, want %d", got, want)
	}
	if got := cap(proj.Projections); got != want {
		t.Fatalf("cap(Projections) = %d, want %d (mutant `+`->`-` at "+
			"histogram_native_mixed_or_subquery_resets_changes.go:`projs := make([]chplan.Projection, 0, len(keyAliases)+1)` would force a "+
			"reallocation, leaving cap != %d)", got, want, want)
	}
}

// innerPairLambdaParams navigates mixedPairVerdictExpr's nested hqLet
// shape: Subscript{Container: FuncCall(FnArrayMap, [Lambda{Params:[outer]},
// ...])}, where that outer Lambda's Body is ANOTHER
// FuncCall(FnArrayMap, [Lambda{Params:[prevParam, currParam]}, ...]) — the
// inner lambda whose Params this function returns.
func innerPairLambdaParams(t *testing.T, e chplan.Expr) []string {
	t.Helper()
	sub, ok := e.(*chplan.Subscript)
	if !ok {
		t.Fatalf("expected *chplan.Subscript, got %T", e)
	}
	outerCall, ok := sub.Container.(*chplan.FuncCall)
	if !ok || outerCall.Fn != chplan.FnArrayMap {
		t.Fatalf("expected outer arrayMap FuncCall, got %#v", sub.Container)
	}
	outerLambda, ok := outerCall.Args[0].(*chplan.Lambda)
	if !ok {
		t.Fatalf("expected outer Lambda, got %T", outerCall.Args[0])
	}
	innerCall, ok := outerLambda.Body.(*chplan.FuncCall)
	if !ok || innerCall.Fn != chplan.FnArrayMap {
		t.Fatalf("expected inner arrayMap FuncCall, got %#v", outerLambda.Body)
	}
	innerLambda, ok := innerCall.Args[0].(*chplan.Lambda)
	if !ok {
		t.Fatalf("expected inner Lambda, got %T", innerCall.Args[0])
	}
	return innerLambda.Params
}

// TestMixedPairVerdictExpr_WindowFnSelectsBranch kills the
// CONDITIONALS_NEGATION mutant (`==` -> `!=`) at
// histogram_native_mixed_or_subquery_resets_changes.go:mixedPairVerdictExpr:`if windowFn == changesWindowFn`:
//
//	prevParam, currParam := paramResetPrevRow, paramResetCurrRow
//	histVerdict := expHistogramResetVerdictExpr()
//	if windowFn == changesWindowFn {
//	    prevParam, currParam = paramChangePrevRow, paramChangeCurrRow
//	    histVerdict = expHistogramChangeVerdictExpr(histSchema)
//	}
//
// resetsWindowFn must keep the reset-shaped params ("ra"/"rb");
// changesWindowFn must switch to the change-shaped ones ("ca"/"cb") — the
// negation swaps both. The lambda's own Params (rather than histVerdict's
// full body) is the lightest-weight signal that distinguishes the two.
func TestMixedPairVerdictExpr_WindowFnSelectsBranch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histSchema := histogramProjectionSchema(s)

	resetsParams := innerPairLambdaParams(t, mixedPairVerdictExpr(resetsWindowFn, histSchema))
	changesParams := innerPairLambdaParams(t, mixedPairVerdictExpr(changesWindowFn, histSchema))

	if resetsParams[0] != paramResetPrevRow || resetsParams[1] != paramResetCurrRow {
		t.Fatalf("resets params = %v, want [%q %q] (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_resets_changes.go:mixedPairVerdictExpr:`if windowFn == changesWindowFn`)",
			resetsParams, paramResetPrevRow, paramResetCurrRow)
	}
	if changesParams[0] != paramChangePrevRow || changesParams[1] != paramChangeCurrRow {
		t.Fatalf("changes params = %v, want [%q %q] (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_resets_changes.go:mixedPairVerdictExpr:`if windowFn == changesWindowFn`)",
			changesParams, paramChangePrevRow, paramChangeCurrRow)
	}
}

// TestMixedFloatPairVerdictExpr_WindowFnSelectsShape kills the
// CONDITIONALS_NEGATION mutant (`==` -> `!=`) at
// histogram_native_mixed_or_subquery_resets_changes.go:mixedFloatPairVerdictExpr:`if windowFn == changesWindowFn`:
// resets renders a bare `curr < prev` (a
// top-level OpLt Binary); changes renders the NaN-aware `curr != prev &&
// !bothNaN` (a top-level OpAnd Binary) — the negation swaps both.
func TestMixedFloatPairVerdictExpr_WindowFnSelectsShape(t *testing.T) {
	t.Parallel()

	prev := &chplan.LitInt{V: 1}
	curr := &chplan.LitInt{V: 2}

	resetsExpr := mixedFloatPairVerdictExpr(resetsWindowFn, prev, curr)
	if bin, ok := resetsExpr.(*chplan.Binary); !ok || bin.Op != chplan.OpLt {
		t.Fatalf("resets shape = %#v, want top-level OpLt Binary (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_resets_changes.go:mixedFloatPairVerdictExpr:`if windowFn == changesWindowFn`)", resetsExpr)
	}

	changesExpr := mixedFloatPairVerdictExpr(changesWindowFn, prev, curr)
	if bin, ok := changesExpr.(*chplan.Binary); !ok || bin.Op != chplan.OpAnd {
		t.Fatalf("changes shape = %#v, want top-level OpAnd Binary (mutant `==`->`!=` at "+
			"histogram_native_mixed_or_subquery_resets_changes.go:mixedFloatPairVerdictExpr:`if windowFn == changesWindowFn`)", changesExpr)
	}
}

// TestLowerMixedOrSubqueryResetsOrChangesInput_RangeModePinnedTakesBroadcast
// kills the INVERT_LOGICAL mutant (`&&` -> `||`) at
// histogram_native_mixed_or_subquery_resets_changes.go:`if ctx.rangeMode() && !subqueryPinned(sub)` — the
// resets/changes sibling of
// gremlins_kill_mixed_or_subquery_last_first_test.go's identical
// last_first dispatch kill.
func TestLowerMixedOrSubqueryResetsOrChangesInput_RangeModePinnedTakesBroadcast(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	pin := int64(1700000000)
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute, Timestamp: &pin}
	mixedRel := &chplan.Scan{Table: "dummy"}
	ctx := lowerCtx{start: start, end: end, step: time.Minute}

	plan, err := lowerMixedOrSubqueryResetsOrChangesInput(mixedRel, sub, resetsWindowFn, s, ctx)
	if err != nil {
		t.Fatalf("lowerMixedOrSubqueryResetsOrChangesInput: %v", err)
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
			"histogram_native_mixed_or_subquery_resets_changes.go:`if ctx.rangeMode() && !subqueryPinned(sub)` would route to the " +
			"true fan-out lowering instead")
	}
}

// TestLowerMixedOrSubqueryResetsOrChangesInput_InstantPinnedNoCrossJoin
// kills the INVERT_LOGICAL mutant (`&&` -> `||`) at
// histogram_native_mixed_or_subquery_resets_changes.go:`if ctx.rangeMode() && subqueryPinned(sub)`,
// inside lowerMixedOrSubqueryResetsOrChangesInput:
//
//	if ctx.rangeMode() && subqueryPinned(sub) {
//	    grid := &chplan.StepGrid{...}
//	    return expHistogramPairCountProjection(&chplan.CrossJoin{Left: grid, Right: windowed}, ...)
//	}
//
// An instant query (ctx.rangeMode()==false) over an `@`-pinned subquery
// must NOT build a CrossJoin — there is no grid to fan across. Flipping
// `&&` to `||` makes subqueryPinned(sub) alone satisfy the condition
// regardless of mode.
func TestLowerMixedOrSubqueryResetsOrChangesInput_InstantPinnedNoCrossJoin(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pin := int64(1700000000)
	sub := &parser.SubqueryExpr{Range: 5 * time.Minute, Step: time.Minute, Timestamp: &pin}
	mixedRel := &chplan.Scan{Table: "dummy"}
	ctx := lowerCtx{start: at, end: at} // instant

	plan, err := lowerMixedOrSubqueryResetsOrChangesInput(mixedRel, sub, resetsWindowFn, s, ctx)
	if err != nil {
		t.Fatalf("lowerMixedOrSubqueryResetsOrChangesInput: %v", err)
	}
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.CrossJoin); ok {
			found = true
		}
		return true
	})
	if found {
		t.Fatalf("instant-mode pinned subquery built a CrossJoin (mutant `&&`->`||` at " +
			"histogram_native_mixed_or_subquery_resets_changes.go:`if ctx.rangeMode() && subqueryPinned(sub)`)")
	}
}
