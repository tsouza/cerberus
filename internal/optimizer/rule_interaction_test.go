package optimizer_test

// Rule × rule interaction matrix (Layer 4 § A).
//
// The optimizer ships 7 named rules across 4 batches:
//
//   - ConstantFoldSemantic (analyzer batch)
//   - ConstantFoldHeuristic (Once batch)
//   - FilterFusion, FilterProjectTranspose, FilterAggregateTranspose,
//     FilterRangeWindowTranspose (predicate-pushdown FixedPoint batch)
//   - ProjectionPushdown (projection FixedPoint batch)
//   - MVSubstitution (mv-substitution FixedPoint batch)
//
// C(7,2) = 21 unordered pairs. For each pair the goal is the same:
// construct a plan where BOTH rules are applicable, run a Driver
// that registers exactly those two rules (in either order), and
// assert the final tree is identical regardless of registration
// order. This catches commutation bugs: a pair where the rule
// firing order changes the converged tree shape would be a hazard
// for the Catalyst-style Batch driver (each FixedPoint batch picks
// up rules in declared order and iterates).
//
// Strategy. Rather than reach for `RunWithRuleOrder` (which the
// driver doesn't expose), each test wires two NewWithBatches
// drivers — one with `[r1, r2]` and one with `[r2, r1]`. Both run
// inside a single FixedPoint(100) batch so the iteration order
// converges. The pair is "interaction-stable" iff
// Driver(r1, r2).Run(plan).Equal(Driver(r2, r1).Run(plan)).
//
// Note. Some pairs are conceptually trivial (e.g. ConstantFoldSemantic
// + ProjectionPushdown — they touch disjoint plan slots). We still
// pin every pair so future rule additions can't quietly break a
// commutation property the codebase implicitly relies on.

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// twoRuleConverge runs the plan through two drivers — one ordered
// (a, b), one ordered (b, a) — and asserts the final trees are
// `Equal`. Both drivers iterate to fixpoint (100 iterations), so any
// commutation property of the pair surfaces as a tree-shape diff.
func twoRuleConverge(t *testing.T, label string, plan chplan.Node, a, b optimizer.Rule) {
	t.Helper()
	dAB := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "ab",
		Strategy: optimizer.FixedPoint(100),
		Rules:    []optimizer.Rule{a, b},
	})
	dBA := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "ba",
		Strategy: optimizer.FixedPoint(100),
		Rules:    []optimizer.Rule{b, a},
	})
	outAB := dAB.Run(context.Background(), plan)
	outBA := dBA.Run(context.Background(), plan)
	if !outAB.Equal(outBA) {
		t.Fatalf("%s: order-dependent fixpoint\n--- (a, b) ---\n%#v\n--- (b, a) ---\n%#v", label, outAB, outBA)
	}
}

// labelFilter builds a `<col> = <v>` predicate. Used pervasively.
func labelFilter(col, v string) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: col},
		Right: &chplan.LitString{V: v},
	}
}

// sumRollupForInteraction is a local copy of sumRollup5m, redeclared
// here to avoid coupling test ordering across files.
var sumRollupForInteraction = schema.Rollup{
	BaseTable:   "otel_metrics_sum",
	RollupTable: "otel_metrics_sum_5m",
	Window:      5 * time.Minute,
	AggOp:       schema.RollupAggSum,
	ValueColumn: "Sum",
}

// --- Pair 1: ConstantFoldSemantic × ConstantFoldHeuristic.
// `(1+2=3) AND X`: semantic folds the arithmetic to `true`, heuristic
// then collapses `true AND X → X`. Order matters across the two but
// in the same FixedPoint batch they converge.
func TestRuleInteraction_ConstantFoldSemantic_x_Heuristic(t *testing.T) {
	t.Parallel()
	inner := labelFilter("MetricName", "up")
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
				Right: &chplan.LitInt{V: 3},
			},
			Right: inner,
		},
	}
	twoRuleConverge(t, "semantic×heuristic", plan, optimizer.ConstantFoldSemantic{}, optimizer.ConstantFoldHeuristic{})
}

// --- Pair 2: ConstantFoldSemantic × FilterFusion.
// Filter(Filter(scan, p1), 1=1) — semantic collapses `1=1 → true`;
// fusion combines into a single Filter.
func TestRuleInteraction_ConstantFoldSemantic_x_FilterFusion(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: labelFilter("MetricName", "up"),
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.LitInt{V: 1},
			Right: &chplan.LitInt{V: 1},
		},
	}
	twoRuleConverge(t, "semantic×fusion", plan, optimizer.ConstantFoldSemantic{}, optimizer.FilterFusion{})
}

// --- Pair 3: ConstantFoldSemantic × FilterProjectTranspose.
// Filter(Project([X], Scan), 0=0): semantic folds; transpose pushes
// the (now-true) Filter under the Project.
func TestRuleInteraction_ConstantFoldSemantic_x_FilterProjectTranspose(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		// `0 = 0 AND MetricName = "up"` — semantic reduces left to
		// LitBool(true); FilterProjectTranspose still moves the Filter
		// even if its predicate is partly-folded.
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 0}, Right: &chplan.LitInt{V: 0}},
			Right: labelFilter("MetricName", "up"),
		},
	}
	twoRuleConverge(t, "semantic×proj-transpose", plan, optimizer.ConstantFoldSemantic{}, optimizer.FilterProjectTranspose())
}

// --- Pair 4: ConstantFoldSemantic × FilterAggregateTranspose.
func TestRuleInteraction_ConstantFoldSemantic_x_FilterAggregateTranspose(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 1}},
			Right: labelFilter("job", "api"),
		},
	}
	twoRuleConverge(t, "semantic×agg-transpose", plan, optimizer.ConstantFoldSemantic{}, optimizer.FilterAggregateTranspose())
}

// --- Pair 5: ConstantFoldSemantic × FilterRangeWindowTranspose.
func TestRuleInteraction_ConstantFoldSemantic_x_FilterRangeWindowTranspose(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	rw := &chplan.RangeWindow{
		Input:           scan,
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: rw,
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: 2}, Right: &chplan.LitInt{V: 2}},
			Right: labelFilter("Attributes", "irrelevant"), // bare column ref over passthrough
		},
	}
	twoRuleConverge(t, "semantic×rw-transpose", plan, optimizer.ConstantFoldSemantic{}, optimizer.FilterRangeWindowTranspose())
}

// --- Pair 6: ConstantFoldSemantic × ProjectionPushdown.
// Disjoint targets (expression slots vs Scan.Columns) but pin commutation.
func TestRuleInteraction_ConstantFoldSemantic_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{
				// `Value + 0` — semantic does NOT fold this (the right
				// is a literal but the left is a ColumnRef); ProjectionPushdown
				// still narrows the Scan to [Value].
				Expr: &chplan.Binary{
					Op:    chplan.OpAdd,
					Left:  &chplan.ColumnRef{Name: "Value"},
					Right: &chplan.LitFloat{V: 0},
				},
				Alias: "v_plus_zero",
			},
		},
	}
	twoRuleConverge(t, "semantic×proj-pushdown", plan, optimizer.ConstantFoldSemantic{}, optimizer.ProjectionPushdown{})
}

// --- Pair 7: ConstantFoldSemantic × MVSubstitution.
func TestRuleInteraction_ConstantFoldSemantic_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	twoRuleConverge(t, "semantic×mv-sub", plan,
		optimizer.ConstantFoldSemantic{},
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 8: ConstantFoldHeuristic × FilterFusion.
// Filter(Filter(scan, p1), true AND p2) — heuristic collapses `true AND p2 → p2`,
// fusion merges. Either order should produce identical results.
func TestRuleInteraction_ConstantFoldHeuristic_x_FilterFusion(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: labelFilter("MetricName", "up"),
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("job", "api"),
		},
	}
	twoRuleConverge(t, "heuristic×fusion", plan, optimizer.ConstantFoldHeuristic{}, optimizer.FilterFusion{})
}

// --- Pair 9: ConstantFoldHeuristic × FilterProjectTranspose.
func TestRuleInteraction_ConstantFoldHeuristic_x_FilterProjectTranspose(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("MetricName", "up"),
		},
	}
	twoRuleConverge(t, "heuristic×proj-transpose", plan, optimizer.ConstantFoldHeuristic{}, optimizer.FilterProjectTranspose())
}

// --- Pair 10: ConstantFoldHeuristic × FilterAggregateTranspose.
func TestRuleInteraction_ConstantFoldHeuristic_x_FilterAggregateTranspose(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("job", "api"),
		},
	}
	twoRuleConverge(t, "heuristic×agg-transpose", plan, optimizer.ConstantFoldHeuristic{}, optimizer.FilterAggregateTranspose())
}

// --- Pair 11: ConstantFoldHeuristic × FilterRangeWindowTranspose.
func TestRuleInteraction_ConstantFoldHeuristic_x_FilterRangeWindowTranspose(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: rw,
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("Attributes", "irrelevant"),
		},
	}
	twoRuleConverge(t, "heuristic×rw-transpose", plan, optimizer.ConstantFoldHeuristic{}, optimizer.FilterRangeWindowTranspose())
}

// --- Pair 12: ConstantFoldHeuristic × ProjectionPushdown.
func TestRuleInteraction_ConstantFoldHeuristic_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
			{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "m"},
		},
	}
	twoRuleConverge(t, "heuristic×proj-pushdown", plan, optimizer.ConstantFoldHeuristic{}, optimizer.ProjectionPushdown{})
}

// --- Pair 13: ConstantFoldHeuristic × MVSubstitution.
func TestRuleInteraction_ConstantFoldHeuristic_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	twoRuleConverge(t, "heuristic×mv-sub", plan,
		optimizer.ConstantFoldHeuristic{},
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 14: FilterFusion × FilterProjectTranspose.
// Filter(Filter(Project([X, Y], Scan), p1), p2) — fusion merges
// outer filters first, then transpose pushes the merged filter
// under the project. Or, transpose can fire on the outer + project
// pair first, leaving inner-fusion to a follow-up iteration.
func TestRuleInteraction_FilterFusion_x_FilterProjectTranspose(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Project{
				Input: scan,
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "MetricName"}},
					{Expr: &chplan.ColumnRef{Name: "Value"}},
				},
			},
			Predicate: labelFilter("MetricName", "up"),
		},
		Predicate: labelFilter("Value", "1.0"),
	}
	twoRuleConverge(t, "fusion×proj-transpose", plan, optimizer.FilterFusion{}, optimizer.FilterProjectTranspose())
}

// --- Pair 15: FilterFusion × FilterAggregateTranspose.
func TestRuleInteraction_FilterFusion_x_FilterAggregateTranspose(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Aggregate{
				Input:   scan,
				GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
				AggFuncs: []chplan.AggFunc{
					{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
				},
			},
			Predicate: labelFilter("job", "api"),
		},
		Predicate: labelFilter("job", "web"),
	}
	twoRuleConverge(t, "fusion×agg-transpose", plan, optimizer.FilterFusion{}, optimizer.FilterAggregateTranspose())
}

// --- Pair 16: FilterFusion × FilterRangeWindowTranspose.
func TestRuleInteraction_FilterFusion_x_FilterRangeWindowTranspose(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     rw,
			Predicate: labelFilter("Attributes", "v1"),
		},
		Predicate: labelFilter("Attributes", "v2"),
	}
	twoRuleConverge(t, "fusion×rw-transpose", plan, optimizer.FilterFusion{}, optimizer.FilterRangeWindowTranspose())
}

// --- Pair 17: FilterFusion × ProjectionPushdown.
// Disjoint shapes today (fusion fires on Filter(Filter), pushdown on
// Project(Scan)). Pin commutation as a forward guarantee.
func TestRuleInteraction_FilterFusion_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Project{
				Input: &chplan.Scan{Table: "otel_metrics_gauge"},
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "MetricName"}},
					{Expr: &chplan.ColumnRef{Name: "Value"}},
				},
			},
			Predicate: labelFilter("MetricName", "up"),
		},
		Predicate: labelFilter("MetricName", "down"),
	}
	twoRuleConverge(t, "fusion×proj-pushdown", plan, optimizer.FilterFusion{}, optimizer.ProjectionPushdown{})
}

// --- Pair 18: FilterFusion × MVSubstitution.
func TestRuleInteraction_FilterFusion_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	// Build Filter(Filter(RangeWindow(Scan))) — fusion target — and let
	// MVSubstitution see the inner RangeWindow(Scan) shape after
	// nothing else changes.
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     rw,
			Predicate: labelFilter("Attributes", "v1"),
		},
		Predicate: labelFilter("Attributes", "v2"),
	}
	twoRuleConverge(t, "fusion×mv-sub", plan,
		optimizer.FilterFusion{},
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 19: FilterProjectTranspose × FilterAggregateTranspose.
// Disjoint shapes (one needs Project, one needs Aggregate). Pin
// anyway: a Filter(Project(Filter(Aggregate(Scan)))) tree would
// exercise both rules sequentially.
func TestRuleInteraction_FilterProjectTranspose_x_FilterAggregateTranspose(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: &chplan.Filter{
				Input: &chplan.Aggregate{
					Input:   scan,
					GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
					AggFuncs: []chplan.AggFunc{
						{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
					},
				},
				Predicate: labelFilter("job", "api"),
			},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "job"}},
			},
		},
		Predicate: labelFilter("job", "api"),
	}
	twoRuleConverge(t, "proj-transpose×agg-transpose", plan, optimizer.FilterProjectTranspose(), optimizer.FilterAggregateTranspose())
}

// --- Pair 20: FilterProjectTranspose × FilterRangeWindowTranspose.
func TestRuleInteraction_FilterProjectTranspose_x_FilterRangeWindowTranspose(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: rw,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Attributes"}},
			},
		},
		Predicate: labelFilter("Attributes", "v1"),
	}
	twoRuleConverge(t, "proj-transpose×rw-transpose", plan, optimizer.FilterProjectTranspose(), optimizer.FilterRangeWindowTranspose())
}

// --- Pair 21: FilterProjectTranspose × ProjectionPushdown.
//
// Commutativity unlocked by widening `ProjectionPushdown` to match
// `Project(Filter(Scan))` in addition to `Project(Scan)` (see
// projection_pushdown.go). With that widening:
//
//   - (Pushdown, TransposeFilter): pushdown narrows the inner Scan
//     before transpose moves the Filter; final shape is
//     `Project([cols], Filter(Scan(narrowed=projCols∪predCols), pred))`.
//   - (TransposeFilter, Pushdown): transpose first leaves
//     `Project(Filter(Scan))`; the widened pushdown then narrows the
//     inner Scan against `projCols ∪ predCols`. Final shape:
//     `Project([cols], Filter(Scan(narrowed=projCols∪predCols), pred))`.
//
// The two final trees are now structurally identical because both
// orderings narrow the Scan to the same union set; the predicate stays
// in the Filter slot above the Scan in both.
func TestRuleInteraction_FilterProjectTranspose_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		},
		Predicate: labelFilter("MetricName", "up"),
	}
	twoRuleConverge(t, "proj-transpose×proj-pushdown", plan,
		optimizer.FilterProjectTranspose(),
		optimizer.ProjectionPushdown{},
	)
}

// --- Pair 22: FilterProjectTranspose × MVSubstitution.
func TestRuleInteraction_FilterProjectTranspose_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: rw,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Attributes"}},
			},
		},
		Predicate: labelFilter("Attributes", "v1"),
	}
	twoRuleConverge(t, "proj-transpose×mv-sub", plan,
		optimizer.FilterProjectTranspose(),
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 23: FilterAggregateTranspose × FilterRangeWindowTranspose.
func TestRuleInteraction_FilterAggregateTranspose_x_FilterRangeWindowTranspose(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_sum"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input:     rw,
		Predicate: labelFilter("Attributes", "v1"),
	}
	twoRuleConverge(t, "agg-transpose×rw-transpose", plan, optimizer.FilterAggregateTranspose(), optimizer.FilterRangeWindowTranspose())
}

// --- Pair 24: FilterAggregateTranspose × ProjectionPushdown.
func TestRuleInteraction_FilterAggregateTranspose_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: &chplan.Project{
				Input: scan,
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "job"}},
					{Expr: &chplan.ColumnRef{Name: "Value"}},
				},
			},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: labelFilter("job", "api"),
	}
	twoRuleConverge(t, "agg-transpose×proj-pushdown", plan, optimizer.FilterAggregateTranspose(), optimizer.ProjectionPushdown{})
}

// --- Pair 25: FilterAggregateTranspose × MVSubstitution.
func TestRuleInteraction_FilterAggregateTranspose_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	// FilterAggregateTranspose doesn't have an Aggregate in this plan
	// — the test verifies the rule is a no-op alongside MV-sub.
	twoRuleConverge(t, "agg-transpose×mv-sub", rw,
		optimizer.FilterAggregateTranspose(),
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 26: FilterRangeWindowTranspose × ProjectionPushdown.
func TestRuleInteraction_FilterRangeWindowTranspose_x_ProjectionPushdown(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_sum"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Attributes"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
				{Expr: &chplan.ColumnRef{Name: "TimeUnix"}},
			},
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input:     rw,
		Predicate: labelFilter("Attributes", "v1"),
	}
	twoRuleConverge(t, "rw-transpose×proj-pushdown", plan, optimizer.FilterRangeWindowTranspose(), optimizer.ProjectionPushdown{})
}

// --- Pair 27: FilterRangeWindowTranspose × MVSubstitution.
//
// Commutativity unlocked by widening `MVSubstitution` to match
// `RangeWindow(Filter(Scan))` in addition to `RangeWindow(Scan)`
// (see mv_substitution.go). The Filter is required to reference only
// series-identity columns on the RangeWindow's `GroupBy` slot —
// columns the rollup table preserves with the same names — so the
// rewrite keeps the Filter alive around the substituted Scan.
//
// On a `Filter(RangeWindow(Scan), pred=Attributes=v1)` plan:
//   - (MV, RWTranspose): MV substitutes Scan.Table → rollup (and
//     RangeWindow.ValueColumn → "Sum"), then RWTranspose pushes the
//     Filter under. Final shape:
//     `RW(Filter(Scan(rollup), pred))` with ValueColumn="Sum".
//   - (RWTranspose, MV): RWTranspose pushes Filter under first,
//     leaving `RW(Filter(Scan(base)), pred)`. MV's widened pattern
//     matches — `pred=Attributes=v1` is rollup-safe (Attributes is
//     a series-identity GroupBy column) — and substitutes the inner
//     Scan + RangeWindow.ValueColumn. Final shape:
//     `RW(Filter(Scan(rollup), pred))` with ValueColumn="Sum".
//
// Both orderings converge to the same tree.
func TestRuleInteraction_FilterRangeWindowTranspose_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input:     rw,
		Predicate: labelFilter("Attributes", "v1"),
	}
	twoRuleConverge(t, "rw-transpose×mv-sub", plan,
		optimizer.FilterRangeWindowTranspose(),
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}

// --- Pair 28: ProjectionPushdown × MVSubstitution.
// MV-sub clears Scan.Columns when it fires. ProjectionPushdown can
// re-narrow against the rollup. Pin convergence.
func TestRuleInteraction_ProjectionPushdown_x_MVSubstitution(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_sum"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Attributes"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
				{Expr: &chplan.ColumnRef{Name: "TimeUnix"}},
			},
		},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	twoRuleConverge(t, "proj-pushdown×mv-sub", rw,
		optimizer.ProjectionPushdown{},
		optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value"),
	)
}
