package optimizer_test

// Termination / fixpoint bounds (Layer 4 § C).
//
// Every rule must satisfy two related properties:
//
//  1. **Idempotence on its own output.** Applying the rule to its
//     own previous output produces `changed=false`. Without this,
//     a FixedPoint batch would never converge.
//  2. **Convergence under a bounded iteration cap.** The default
//     cap is 100. For pathological inputs that could nominally
//     cycle (a Filter that transposes back and forth), the driver
//     must still terminate inside the cap.
//
// We pin both properties per rule. Most rules are trivially
// idempotent because their match condition is consumed on the first
// rewrite (e.g. FilterFusion fires on `Filter(Filter)` and produces
// `Filter(Scan)`, which doesn't match). The interesting cases are
// the transposes: they can swap two nodes and theoretically swap
// them back. Each transpose's `Apply` is intentionally one-directional
// — it pushes Filter UNDER the named operator, never above. The
// inverse direction never matches.

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// applyTwice runs rule.Apply against the plan; if it changed, runs
// Apply against the result. Returns (final, firstChanged, secondChanged).
// secondChanged must be false for the rule to qualify as idempotent.
func applyTwice(rule optimizer.Rule, plan chplan.Node) (chplan.Node, bool, bool) {
	out1, ch1 := rule.Apply(plan)
	out2, ch2 := rule.Apply(out1)
	return out2, ch1, ch2
}

// TestTermination_FilterFusion_Idempotent verifies fusion runs once
// and then reports no change on its own output. Filter(Filter(scan))
// → Filter(scan) on the first pass; the second pass sees no further
// Filter-over-Filter and reports false.
func TestTermination_FilterFusion_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: labelFilter("MetricName", "up"),
		},
		Predicate: labelFilter("job", "api"),
	}
	_, ch1, ch2 := applyTwice(optimizer.FilterFusion{}, plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true (Filter(Filter) is a fusion target)")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false (rule must be idempotent on its own output)")
	}
}

// TestTermination_FilterFusion_DeepStackConverges builds a deep
// stack of N Filters and verifies the driver collapses them all into
// a single Filter inside the default iteration cap.
func TestTermination_FilterFusion_DeepStackConverges(t *testing.T) {
	t.Parallel()
	const depth = 50
	plan := chplan.Node(&chplan.Scan{Table: "otel_metrics_gauge"})
	for i := 0; i < depth; i++ {
		plan = &chplan.Filter{
			Input:     plan,
			Predicate: labelFilter("MetricName", "up"),
		}
	}
	out := optimizer.New(optimizer.FilterFusion{}).Run(context.Background(), plan)
	// After fixpoint, the stack should collapse to a single Filter(Scan).
	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected Filter at root after fusion, got %T", out)
	}
	if _, ok := f.Input.(*chplan.Scan); !ok {
		t.Fatalf("expected Filter(Scan) after fixpoint, got Filter(%T)", f.Input)
	}
}

// TestTermination_ConstantFoldSemantic_Idempotent runs the rule
// twice; the second pass must report no change. By design semantic
// fold produces a canonical literal form that doesn't trigger
// further folds.
func TestTermination_ConstantFoldSemantic_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
			Right: &chplan.LitInt{V: 3},
		},
	}
	_, ch1, ch2 := applyTwice(optimizer.ConstantFoldSemantic{}, plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on `1+2 = 3`")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false; semantic fold must be idempotent")
	}
}

// TestTermination_ConstantFoldHeuristic_Idempotent.
func TestTermination_ConstantFoldHeuristic_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("MetricName", "up"),
		},
	}
	_, ch1, ch2 := applyTwice(optimizer.ConstantFoldHeuristic{}, plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on `true AND X`")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false; heuristic fold must be idempotent")
	}
}

// TestTermination_FilterAggregateTranspose_Idempotent.
func TestTermination_FilterAggregateTranspose_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: labelFilter("job", "api"),
	}
	_, ch1, ch2 := applyTwice(optimizer.FilterAggregateTranspose(), plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on Filter(Aggregate)")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false; transpose is one-directional and must be idempotent")
	}
}

// TestTermination_FilterRangeWindowTranspose_Idempotent.
func TestTermination_FilterRangeWindowTranspose_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: labelFilter("Attributes", "v1"),
	}
	_, ch1, ch2 := applyTwice(optimizer.FilterRangeWindowTranspose(), plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on Filter(RangeWindow)")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false; transpose is one-directional and must be idempotent")
	}
}

// TestTermination_ProjectionPushdown_Idempotent. After narrowing the
// Scan's column list, a second pass sees `len(Scan.Columns) > 0` and
// declines (the rule's guard exits early in that case).
func TestTermination_ProjectionPushdown_Idempotent(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
	}
	_, ch1, ch2 := applyTwice(optimizer.ProjectionPushdown{}, plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on Project(Scan with empty Columns)")
	}
	if ch2 {
		t.Fatalf("second Apply: expected changed=false; ProjectionPushdown must be idempotent")
	}
}

// TestTermination_DefaultDriverConvergesOnPathologicalStack rebuilds
// the existing TestDriver_FixpointTerminates case but with a deeper
// stack (100 levels) and a non-trivial predicate. The default driver
// must converge within its iteration cap.
func TestTermination_DefaultDriverConvergesOnPathologicalStack(t *testing.T) {
	t.Parallel()
	const depth = 100
	plan := chplan.Node(&chplan.Scan{Table: "otel_metrics_gauge"})
	for i := 0; i < depth; i++ {
		plan = &chplan.Filter{
			Input:     plan,
			Predicate: labelFilter("MetricName", "up"),
		}
	}

	out := optimizer.Default().Run(context.Background(), plan)
	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected Filter at root after fixpoint, got %T", out)
	}
	if _, ok := f.Input.(*chplan.Scan); !ok {
		t.Fatalf("expected Filter(Scan) after fixpoint, got Filter(%T)", f.Input)
	}
}
