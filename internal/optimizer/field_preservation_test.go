package optimizer_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestConstantFold_PreservesAggFuncCombinators pins that folding an
// aggregate's argument does not rewrite the aggregate function itself.
//
// The fold once rebuilt an Aggregate's AggFunc from a composite literal
// whenever any expression folded, but failed to carry Combinators.
// `argMinIf(a, b, cond)` became invalid three-argument `argMin(a, b, cond)`,
// while `groupArrayIf` became `groupArray` and silently retained excluded rows.
func TestConstantFold_PreservesAggFuncCombinators(t *testing.T) {
	t.Parallel()

	foldable := &chplan.Binary{
		Op:    chplan.OpAdd,
		Left:  &chplan.LitInt{V: 2},
		Right: &chplan.LitInt{V: 3},
	}
	plan := &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_traces"},
		AggFuncs: []chplan.AggFunc{{
			Fn:          chplan.FnArgMin,
			Args:        []chplan.Expr{&chplan.ColumnRef{Name: "Value"}, foldable},
			Alias:       "am",
			Combinators: []chplan.AggCombinator{chplan.CombIf},
		}},
	}

	got, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if !changed {
		t.Fatal("ConstantFoldSemantic did not fire; AggFunc rebuild test guards never ran")
	}
	agg, ok := got.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("optimized plan = %T, want *chplan.Aggregate", got)
	}
	if len(agg.AggFuncs) != 1 {
		t.Fatalf("AggFuncs length = %d, want 1", len(agg.AggFuncs))
	}
	if len(agg.AggFuncs[0].Combinators) != 1 || agg.AggFuncs[0].Combinators[0] != chplan.CombIf {
		t.Fatalf("AggFuncs[0].Combinators = %v, want [CombIf]", agg.AggFuncs[0].Combinators)
	}
}
