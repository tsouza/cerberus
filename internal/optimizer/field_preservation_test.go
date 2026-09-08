package optimizer_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestConstantFold_PreservesAggFuncCombinators pins that folding an
// aggregate's argument does not rewrite the aggregate itself.
//
// The fold rebuilt every AggFunc of an Aggregate from a composite literal
// whenever ANY expression in that Aggregate folded, and the literal did not
// carry Combinators. `argMinIf(a, b, cond)` came out as `argMin(a, b,
// cond)` — a three-argument argMin ClickHouse rejects outright — and
// `groupArrayIf` as `groupArray`, which silently collects the rows the
// condition was there to exclude.
func TestConstantFold_PreservesAggFuncCombinators(t *testing.T) {
	t.Parallel()

	// A foldable argument: the constant-fold rules collapse a literal
	// arithmetic Binary, which is what triggers the AggFunc rebuild.
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
		t.Fatal("ConstantFoldSemantic did not fire; the AggFunc rebuild this test guards never ran")
	}

	agg, ok := got.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("optimized plan = %T, want *chplan.Aggregate", got)
	}
	if len(agg.AggFuncs) != 1 {
		t.Fatalf("AggFuncs = %d, want 1", len(agg.AggFuncs))
	}
	if len(agg.AggFuncs[0].Combinators) != 1 {
		t.Fatalf(
			"AggFuncs[0].Combinators = %v, want the If combinator to survive the fold: "+
				"argMinIf silently became argMin",
			agg.AggFuncs[0].Combinators,
		)
	}
}

// TestFilterRewrites_PreserveHistogramRowShape pins that the three rules
// that rebuild a chplan.Filter carry its Histogram / Mixed flags.
//
// chplan/filter.go makes those flags part of the node's contract: RowShapeOf
// must report a histogram row shape for such a Filter or a wire consumer
// silently drops the nine histogram columns. All three rules rebuilt the
// node from a composite literal that did not carry them, contradicting the
// `newAgg := *a` / `newRW := *r` discipline two lines below in the same
// functions.
func TestFilterRewrites_PreserveHistogramRowShape(t *testing.T) {
	t.Parallel()

	pred := func(name string) chplan.Expr {
		return &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: name},
			Right: &chplan.LitString{V: "x"},
		}
	}

	t.Run("fusion_keeps_the_outer_filters_shape", func(t *testing.T) {
		t.Parallel()

		plan := &chplan.Filter{
			Predicate: pred("outer"),
			Histogram: true,
			Mixed:     true,
			Input: &chplan.Filter{
				Predicate: pred("inner"),
				Input:     &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
			},
		}

		got, ok := optimizer.FilterFusion{}.Apply(plan)
		if !ok {
			t.Fatal("FilterFusion did not fire on nested Filters")
		}
		f, ok := got.(*chplan.Filter)
		if !ok {
			t.Fatalf("fused node = %T, want *chplan.Filter", got)
		}
		if !f.Histogram || !f.Mixed {
			t.Errorf(
				"fused Filter Histogram=%v Mixed=%v, want both true — the outer Filter's row shape "+
					"is what the parent sees and it was dropped",
				f.Histogram, f.Mixed,
			)
		}
	})

	t.Run("transposes_carry_the_shape_down", func(t *testing.T) {
		t.Parallel()

		for name, plan := range map[string]chplan.Node{
			"filter_over_aggregate": &chplan.Filter{
				Predicate: pred("Attributes"),
				Histogram: true,
				Mixed:     true,
				Input: &chplan.Aggregate{
					Input:   &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
					GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				},
			},
			"filter_over_range_window": &chplan.Filter{
				Predicate: pred("Attributes"),
				Histogram: true,
				Mixed:     true,
				Input: &chplan.RangeWindow{
					Input: &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
					Range: 5 * time.Minute,
					// The transpose only fires when the predicate reads
					// nothing but the window's series-identifying GroupBy
					// columns, so the fixture has to supply them.
					GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				},
			},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var got chplan.Node
				switch plan.(*chplan.Filter).Input.(type) {
				case *chplan.Aggregate:
					got, _ = optimizer.FilterAggregateTranspose().Apply(plan)
				default:
					got, _ = optimizer.FilterRangeWindowTranspose().Apply(plan)
				}

				var seen *chplan.Filter
				chplan.Walk(got, func(n chplan.Node) bool {
					if f, ok := n.(*chplan.Filter); ok && seen == nil {
						seen = f
					}
					return true
				})
				if seen == nil {
					t.Fatal("the transpose did not fire, so this case proves nothing: fix the fixture")
				}
				if !seen.Histogram || !seen.Mixed {
					t.Errorf(
						"transposed Filter Histogram=%v Mixed=%v, want both true — the flags did not "+
							"ride down with the predicate",
						seen.Histogram, seen.Mixed,
					)
				}
			})
		}
	})
}
