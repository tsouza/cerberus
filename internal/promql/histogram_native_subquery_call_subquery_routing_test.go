package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Routing pins for cerberus issue #3253's third finding: WHICH inner
// expression actually reaches [lowerSubqueryOverCallSubquery]'s
// doubly-nested composition, and with which row shape.
//
// The question is not academic. [lowerHistogramOrMixedCallSubqueryInput]
// splits on `shape == chplan.HistogramRowShape`, and
// histogram_native_subquery_call_subquery_chdb_test.go's own header used to
// assert that a bare `and`/`or` set-op inner is what reaches it with a
// HistogramRowShape wideInner, on the premise that
// [isExpHistogramValuedShape] does not recognise a set-op. That premise was
// true when #2726 was written and is not true now: cerberus issue #2324
// added [expHistogramSetOp] to that predicate, so
// [rangeFnOverExpHistogramSubquery] / [selectFnOverExpHistogramSubquery]
// claim every pure-histogram set-op inner at the SINGLE-LEVEL recognizer
// and [lowerSubqueryOverCallSubquery] is never reached for one. Every
// `HistAnd` case in that file therefore proves the single-level
// continuations, not the doubly-nested ones its doc comments named.
//
// The doubly-nested HistogramRowShape arms are still live — a
// NON-default-matching exp-histogram binop reaches them, because
// [isExpHistogramValuedShape] deliberately withholds recognition from
// `on()` / `ignoring()` matching while [lowerExpHistogramHistogramBinop]
// lowers it to a HistogramRowShape relation regardless. These three tests
// pin all three routes so that a change to either recognizer fails here
// rather than silently turning an arm into dead code or an unrecognised
// shape into a ClickHouse error.
const (
	// callSubqRoutePureSetOpInner and callSubqRouteOrSetOpInner are the two
	// pure-histogram set-op inners #2726 was written for and #2324 now
	// intercepts.
	callSubqRoutePureSetOpInner = "((route_a_exp_hist) and (route_b_exp_hist))"
	callSubqRouteOrSetOpInner   = "((route_a_exp_hist) or (route_b_exp_hist))"
	// callSubqRouteHistBinopInner is the shape that DOES reach the
	// doubly-nested composition with a HistogramRowShape wideInner.
	callSubqRouteHistBinopInner = "(route_a_exp_hist + on(x) route_b_exp_hist)"
	// callSubqRouteMixedInner reaches it with a MixedRowShape wideInner.
	callSubqRouteMixedInner = "((route_a_exp_hist) or (route_a_float))"
)

// callSubqRouteQuery brackets inner as the doubly-nested
// `<fn>(<inner>[3m:1m])[4m:1m]` composition.
func callSubqRouteQuery(fn, inner string) string {
	return fn + "(" + inner + "[3m:1m])[4m:1m]"
}

// callSubqRoutePlan lowers one composition at a fixed instant anchor.
func callSubqRoutePlan(t *testing.T, fn, inner string) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).
		ParseExpr(callSubqRouteQuery(fn, inner))
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", callSubqRouteQuery(fn, inner), err)
	}
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	plan, err := LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", callSubqRouteQuery(fn, inner), err)
	}
	return plan
}

// callSubqRouteMarkers counts the two nodes that tell the three routes
// apart: an OuterRange-mode [chplan.RangeBucketFanout] is built ONLY by
// [buildOuterRangeSubqueryFanout] (every ambient-grid continuation leaves
// OuterRange zero), and a Mixed [chplan.VectorSetOp] above it is built only
// by the Mixed arms' own recombination and projections.
func callSubqRouteMarkers(plan chplan.Node) (outerFanouts, mixedUnions int) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.RangeBucketFanout:
			if v.OuterRange > 0 {
				outerFanouts++
			}
		case *chplan.VectorSetOp:
			if v.Mixed {
				mixedUnions++
			}
		}
		return true
	})
	return outerFanouts, mixedUnions
}

// TestCallSubqueryRouting_PureSetOpInnerIsInterceptedEarlier pins that a
// pure-histogram `and`/`or` set-op inner never reaches the doubly-nested
// composition at all: [isExpHistogramValuedShape] recognises it (cerberus
// issue #2324), so the single-level recognizer claims `<fn>(<inner-sub>)`
// as a histogram-valued shape and the ambient-grid continuation answers it.
//
// This is the assertion that makes the correction to
// histogram_native_subquery_call_subquery_chdb_test.go's header checkable:
// if #2324's set-op arm is ever removed from that predicate, these queries
// start reaching [lowerSubqueryOverCallSubquery] and this test fails,
// pointing at the doc that would then be right again.
func TestCallSubqueryRouting_PureSetOpInnerIsInterceptedEarlier(t *testing.T) {
	t.Parallel()

	for _, inner := range []string{callSubqRoutePureSetOpInner, callSubqRouteOrSetOpInner} {
		for _, fn := range outerFn2AllNames() {
			plan := callSubqRoutePlan(t, fn, inner)
			outerFanouts, _ := callSubqRouteMarkers(plan)
			if outerFanouts != 0 {
				t.Errorf("%s: built %d OuterRange-mode fan-out(s), want 0 — a pure-histogram set-op inner is claimed by the single-level recognizer, not by lowerSubqueryOverCallSubquery",
					callSubqRouteQuery(fn, inner), outerFanouts)
			}
		}
	}
}

// TestCallSubqueryRouting_HistBinopInnerReachesTheHistogramArms pins the
// shape that DOES exercise [lowerHistogramOrMixedCallSubqueryInput]'s
// `shape == chplan.HistogramRowShape` arms — and therefore
// [lowerSelectFnOverCallSubqueryInput]'s last/first and resets/changes
// branches and [lowerExpHistogramFoldOverCallSubqueryInput]'s direct,
// non-Mixed use.
//
// Two assertions, and both are load-bearing. The OuterRange-mode fan-out
// proves the doubly-nested composition ran at all. The ABSENCE of a Mixed
// VectorSetOp proves it took the HistogramRowShape arm rather than the
// Mixed one: every Mixed arm either recombines two branches through
// [combineMixedAggregateBranches] or projects through
// [mixedLastFirstProjection], and both put a Mixed VectorSetOp in the plan.
// Deleting an arm and letting the Mixed sibling answer a HistogramRowShape
// wideInner would satisfy the first assertion and fail the second.
func TestCallSubqueryRouting_HistBinopInnerReachesTheHistogramArms(t *testing.T) {
	t.Parallel()

	for _, fn := range outerFn2AllNames() {
		plan := callSubqRoutePlan(t, fn, callSubqRouteHistBinopInner)
		outerFanouts, mixedUnions := callSubqRouteMarkers(plan)
		if outerFanouts == 0 {
			t.Errorf("%s: built no OuterRange-mode fan-out — the doubly-nested composition was not reached",
				callSubqRouteQuery(fn, callSubqRouteHistBinopInner))
		}
		if mixedUnions != 0 {
			t.Errorf("%s: built %d Mixed VectorSetOp(s), want 0 — a HistogramRowShape wideInner must take the histogram arm, not the Mixed sibling",
				callSubqRouteQuery(fn, callSubqRouteHistBinopInner), mixedUnions)
		}
		// The arms are only as reachable as the SQL they produce: a
		// continuation that lowers but cannot emit is still dead in
		// production.
		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Errorf("%s: Emit: %v", callSubqRouteQuery(fn, callSubqRouteHistBinopInner), err)
		}
	}
}

// TestCallSubqueryRouting_MixedInnerReachesTheMixedArms is the negative
// control for the test above: the SAME fifteen names over a mixed
// float/histogram `or` inner must reach the composition too, and must take
// the Mixed arm. Without it, "no Mixed VectorSetOp" above would also pass
// if the Mixed arms had quietly stopped being reachable.
func TestCallSubqueryRouting_MixedInnerReachesTheMixedArms(t *testing.T) {
	t.Parallel()

	for _, fn := range outerFn2AllNames() {
		plan := callSubqRoutePlan(t, fn, callSubqRouteMixedInner)
		outerFanouts, mixedUnions := callSubqRouteMarkers(plan)
		if outerFanouts == 0 {
			t.Errorf("%s: built no OuterRange-mode fan-out — the doubly-nested composition was not reached",
				callSubqRouteQuery(fn, callSubqRouteMixedInner))
		}
		if mixedUnions == 0 {
			t.Errorf("%s: built no Mixed VectorSetOp — a MixedRowShape wideInner must take a Mixed arm",
				callSubqRouteQuery(fn, callSubqRouteMixedInner))
		}
	}
}
