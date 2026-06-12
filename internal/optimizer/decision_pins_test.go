package optimizer_test

// Cost-based decision pins (Layer 4 § D).
//
// The optimizer's named rules that make non-trivial "should I rewrite"
// decisions based on plan / schema context are the transpose family
// (FilterAggregateTranspose, FilterRangeWindowTranspose) and
// ProjectionPushdown: each checks passthrough columns / column sets to
// decide whether the rewrite is semantics-preserving.
//
// PREWHERE promotion and late-materialisation are not wired as
// distinct named rules. Their semantic deputies in v1 are the
// FilterRangeWindowTranspose + ProjectionPushdown rules; pin those.
//
// Each test asserts on the *plan shape* the rule produces — not on
// raw SQL — so a future emitter tweak (changing inner aliases, for
// example) doesn't make the test brittle.

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestDecision_RangeWindowTranspose_PushedOnSeriesKey: predicate
// references a bare series-key column (Attributes) → pushed under
// the window. This is the load-bearing decision for predicate
// pushdown under PromQL-style windows.
func TestDecision_RangeWindowTranspose_PushedOnSeriesKey(t *testing.T) {
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
		Input:     rw,
		Predicate: labelFilter("Attributes", "v1"),
	}
	out, _ := optimizer.FilterRangeWindowTranspose().Apply(plan)
	got, ok := out.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected RangeWindow at root, got %T", out)
	}
	if _, ok := got.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under RangeWindow (predicate pushed), got %T", got.Input)
	}
}

// TestDecision_RangeWindowTranspose_NotPushed_ValueColumn pins
// the inverse: predicate touches the windowed value → don't push.
func TestDecision_RangeWindowTranspose_NotPushed_ValueColumn(t *testing.T) {
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
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.LitFloat{V: 0},
		},
	}
	out, _ := optimizer.FilterRangeWindowTranspose().Apply(plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (rewrite must be declined for value column); got %T", out)
	}
}

// TestDecision_ProjectionPushdown_NarrowsScan pins the canonical
// projection-pushdown decision: a Project([X, Y], Scan(*)) narrows
// the Scan to Columns=[X, Y].
func TestDecision_ProjectionPushdown_NarrowsScan(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
	}
	out, _ := optimizer.ProjectionPushdown{}.Apply(plan)
	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected Project at root, got %T", out)
	}
	scan, ok := proj.Input.(*chplan.Scan)
	if !ok {
		t.Fatalf("expected Scan child, got %T", proj.Input)
	}
	got := append([]string(nil), scan.Columns...)
	// Order is sorted by referencedColumns().
	want := []string{"MetricName", "Value"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Scan.Columns: got %v, want %v", got, want)
	}
}

// TestDecision_ProjectionPushdown_PreservesAlreadyNarrowed pins the
// inverse: a Scan with pre-existing Columns is left alone (the rule
// doesn't second-guess an upstream pushdown).
func TestDecision_ProjectionPushdown_PreservesAlreadyNarrowed(t *testing.T) {
	t.Parallel()
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge", Columns: []string{"MetricName"}},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
	}
	out, ch := optimizer.ProjectionPushdown{}.Apply(plan)
	if ch {
		t.Fatalf("expected changed=false for pre-narrowed Scan, got changed=true with %#v", out)
	}
}

// TestDecision_AggregateTranspose_NotPushed_EmptyGroupBy pins:
// an Aggregate with no group keys exposes no passthrough columns, so
// the rewrite is declined regardless of predicate shape.
func TestDecision_AggregateTranspose_NotPushed_EmptyGroupBy(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			AggFuncs: []chplan.AggFunc{
				{Name: "count", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: labelFilter("Value", "1"),
	}
	out, _ := optimizer.FilterAggregateTranspose().Apply(plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (empty GroupBy → no passthrough → decline); got %T", out)
	}
}
