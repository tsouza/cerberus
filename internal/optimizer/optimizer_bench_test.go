package optimizer_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// buildComplexPlan returns a plan that the default Driver should rewrite
// across multiple batches: a Filter / Aggregate / Project nest where
// constant-fold collapses a `true AND <predicate>`, filter-fusion
// merges the result with an outer Filter, filter-aggregate-transpose
// pushes the fused Filter under the Aggregate, and projection-pushdown
// trims the Scan's column list at the bottom. ≥3 rule fires per Run so
// fixpoint iteration is exercised end-to-end.
func buildComplexPlan() chplan.Node {
	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	agg := &chplan.Aggregate{
		Input:   scan,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
		},
	}
	// Outer Filter over Aggregate with a constant-true conjunction —
	// drives constant-fold + filter-aggregate-transpose.
	innerFilter := &chplan.Filter{
		Input: agg,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	}
	outerFilter := &chplan.Filter{
		Input: innerFilter,
		Predicate: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: &chplan.LitBool{V: true},
			Right: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "sum_value"},
				Right: &chplan.LitFloat{V: 0},
			},
		},
	}
	return &chplan.Project{
		Input: outerFilter,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "job"}},
			{Expr: &chplan.ColumnRef{Name: "sum_value"}, Alias: "result"},
		},
	}
}

func BenchmarkDriver_Run(b *testing.B) {
	d := optimizer.Default()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Build a fresh plan each iteration — Run is non-destructive
		// (rules construct new nodes) but timer cleanliness means the
		// allocation profile of the input must not be amortised across
		// iterations.
		b.StopTimer()
		plan := buildComplexPlan()
		b.StartTimer()
		_ = d.Run(context.Background(), plan)
	}
}

// TestAllocs_DriverRun pins the per-run alloc count. The optimizer is
// expected to allocate — every rule fire builds a fresh node — but the
// ceiling caps catastrophic regressions (e.g. introducing reflect.Copy
// somewhere in the rewrite path).
func TestAllocs_DriverRun(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	d := optimizer.Default()
	got := testing.AllocsPerRun(50, func() {
		_ = d.Run(context.Background(), buildComplexPlan())
	})
	// Baseline 88 allocs — Driver.Run builds fresh nodes on every
	// rule fire. Ceiling = baseline × ~3.
	const ceiling = 270.0
	if got > ceiling {
		t.Errorf("Driver.Run avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("Driver.Run avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}
