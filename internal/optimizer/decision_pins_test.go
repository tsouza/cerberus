package optimizer_test

// Cost-based decision pins (Layer 4 § D).
//
// The optimizer ships two rules that make non-trivial "should I rewrite"
// decisions based on plan / schema context:
//
//   - MVSubstitution evaluates four safety conditions (step ≥ window,
//     range ≥ window, range%window == 0, outer-fn commutes with rollup
//     AggOp). The default v1 cost model is `firstApplicable`.
//   - The four transpose rules check passthrough columns to decide
//     whether the rewrite is semantics-preserving.
//
// PREWHERE promotion and late-materialisation aren't yet wired as
// distinct named rules (the doc.go reference is forward-looking — see
// roadmap RC3). Their semantic deputies in v1 are the
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
	"github.com/tsouza/cerberus/internal/schema"
)

// mvRule5m is a convenience constructor for the canonical
// MVSubstitution test rule wiring (5-minute sum-rollup against
// otel_metrics_sum).
func mvRule5m() optimizer.Rule {
	return optimizer.MVSubstitution([]schema.Rollup{sumRollupForInteraction}, "Value")
}

// mvRule1hAnd5m: registry with the 1h rollup first (coarsest) +
// 5m fallback. Tests the firstApplicable cost-model picking.
func mvRule1hAnd5m() optimizer.Rule {
	return optimizer.MVSubstitution([]schema.Rollup{
		{
			BaseTable:   "otel_metrics_sum",
			RollupTable: "otel_metrics_sum_1h",
			Window:      time.Hour,
			AggOp:       schema.RollupAggSum,
			ValueColumn: "Sum",
		},
		sumRollupForInteraction,
	}, "Value")
}

// rangeWindow is a typed helper that builds a RangeWindow with the
// supplied func + range + step over otel_metrics_sum.
func rangeWindow(fn string, rng, step time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            fn,
		Range:           rng,
		Step:            step,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// scanTableFromRangeWindow extracts the rewritten Scan's Table from
// the rule output, fatalling if the shape is wrong.
func scanTableFromRangeWindow(t *testing.T, n chplan.Node) string {
	t.Helper()
	rw, ok := n.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected RangeWindow at root, got %T", n)
	}
	scan, ok := rw.Input.(*chplan.Scan)
	if !ok {
		t.Fatalf("expected Scan child, got %T", rw.Input)
	}
	return scan.Table
}

// TestDecision_MVSub_StepEqualsWindowApplies pins the boundary case
// (1): step == window is the minimum step the rollup buckets allow.
// Must apply.
func TestDecision_MVSub_StepEqualsWindowApplies(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("sum_over_time", time.Hour, 5*time.Minute)
	out, _ := mvRule5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum_5m" {
		t.Fatalf("step=5m == window=5m must apply; got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_RangeMultipleApplies pins safety condition (2):
// range = 2 × window. Must apply.
func TestDecision_MVSub_RangeMultipleApplies(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("sum_over_time", 10*time.Minute, 5*time.Minute)
	out, _ := mvRule5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum_5m" {
		t.Fatalf("range=10m = 2×window=5m must apply; got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_RangeOffByOneRejected covers the "almost
// aligned" case: range = window + 1 unit. Mod is non-zero so must
// reject.
func TestDecision_MVSub_RangeOffByOneRejected(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("sum_over_time", 5*time.Minute+time.Second, 5*time.Minute)
	out, _ := mvRule5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum" {
		t.Fatalf("range=5m1s not multiple of 5m must reject; got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_DifferentAggregatorRejected covers safety
// condition (3): query uses `count_over_time` but the only rollup
// is a sum-rollup. Must reject — sum ≠ count.
func TestDecision_MVSub_DifferentAggregatorRejected(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("count_over_time", time.Hour, 5*time.Minute)
	out, _ := mvRule5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum" {
		t.Fatalf("count_over_time over sum-rollup must reject; got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_RateRejected pins the v1 rate exclusion:
// rate(metric[5m]) over a sum-rollup is NOT safe (rate looks at
// per-sample deltas which the sum-rollup loses). Must reject.
func TestDecision_MVSub_RateRejected(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("rate", time.Hour, 5*time.Minute)
	out, _ := mvRule5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum" {
		t.Fatalf("rate over sum-rollup must reject (per-sample deltas not preserved); got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_CoarsestFirst pins the firstApplicable cost
// model: with both 1h and 5m rollups available and the query
// permitting both, pick 1h.
func TestDecision_MVSub_CoarsestFirst(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("sum_over_time", 24*time.Hour, time.Hour)
	out, _ := mvRule1hAnd5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum_1h" {
		t.Fatalf("firstApplicable must pick coarsest (1h); got Scan.Table=%q", got)
	}
}

// TestDecision_MVSub_FallbackWhenCoarsestRejected: 1h is rejected
// (step < window), 5m must be picked.
func TestDecision_MVSub_FallbackWhenCoarsestRejected(t *testing.T) {
	t.Parallel()
	plan := rangeWindow("sum_over_time", time.Hour, 5*time.Minute)
	out, _ := mvRule1hAnd5m().Apply(plan)
	if got := scanTableFromRangeWindow(t, out); got != "otel_metrics_sum_5m" {
		t.Fatalf("expected 1h rejected (step<window), fall through to 5m; got Scan.Table=%q", got)
	}
}

// TestDecision_TransposeFilter_PushedUnderProject_BareColumn pins
// the canonical "predicate references a passthrough column" → rewrite.
// This is the transpose equivalent of "PREWHERE on the sort-prefix
// promotes" — the predicate sits next to the Scan after the rewrite.
func TestDecision_TransposeFilter_PushedUnderProject_BareColumn(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			},
		},
		Predicate: labelFilter("MetricName", "up"),
	}
	out, _ := optimizer.FilterProjectTranspose().Apply(plan)
	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected Project at root, got %T", out)
	}
	if _, ok := proj.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Project (predicate pushed below the projection layer), got %T", proj.Input)
	}
}

// TestDecision_TransposeFilter_NotPushed_ComputedColumn pins the
// inverse decision: predicate references a computed Project column
// → don't push. The Filter stays above the Project.
func TestDecision_TransposeFilter_NotPushed_ComputedColumn(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{
					Expr: &chplan.Binary{
						Op:    chplan.OpMul,
						Left:  &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.LitInt{V: 2},
					},
					Alias: "doubled",
				},
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
			},
		},
		// Predicate references `doubled` — only exists post-Project.
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "doubled"},
			Right: &chplan.LitInt{V: 10},
		},
	}
	out, _ := optimizer.FilterProjectTranspose().Apply(plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (rewrite must be declined for computed column); got %T", out)
	}
}

// TestDecision_TransposeFilter_NotPushed_RenamedColumn: predicate
// touches an aliased name that has no source-side equivalent.
func TestDecision_TransposeFilter_NotPushed_RenamedColumn(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "metric"},
			},
		},
		Predicate: labelFilter("metric", "up"),
	}
	out, _ := optimizer.FilterProjectTranspose().Apply(plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (rewrite must be declined for renamed column); got %T", out)
	}
}

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
