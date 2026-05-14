package chplan_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// chplan Equal + Walk micro-benchmarks (Layer 12). Equal is the
// optimizer's primary "did the rewrite change anything" oracle and is
// called on every node touched by every rule fire; Walk is the
// traversal primitive used by analyzers (rule-pattern matching,
// idempotence checks, late-mat). Both must stay zero-allocation in
// the steady state.

// buildDeepTree constructs a plan tree with depth ≥6 to give the
// benchmarks a realistic structural recursion cost. Mirrors what
// `sum by (job)(rate(http_requests_total[5m]))` lowers to:
// Project(Aggregate(RangeWindow(Filter(Filter(Scan))))).
func buildDeepTree() chplan.Node {
	scan := &chplan.Scan{
		Table:   "otel_metrics_sum",
		Columns: []string{"MetricName", "Attributes", "TimeUnix", "Value"},
	}
	inner := &chplan.Filter{
		Input: scan,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
	outer := &chplan.Filter{
		Input: inner,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "TimeUnix"},
			Right: &chplan.LitInt{V: 1700000000},
		},
	}
	rw := &chplan.RangeWindow{
		Input: outer,
		Func:  "rate",
		Range: 5 * time.Minute,
		Step:  time.Minute,
	}
	agg := &chplan.Aggregate{
		Input:   rw,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
		},
	}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "job"}},
			{Expr: &chplan.ColumnRef{Name: "sum_value"}, Alias: "result"},
		},
	}
}

func BenchmarkEqual(b *testing.B) {
	left := buildDeepTree()
	right := buildDeepTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !left.Equal(right) {
			b.Fatal("Equal returned false on identical trees")
		}
	}
}

func BenchmarkWalk(b *testing.B) {
	tree := buildDeepTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		chplan.Walk(tree, func(_ chplan.Node) bool {
			count++
			return true
		})
		if count == 0 {
			b.Fatal("Walk visited zero nodes")
		}
	}
}

// TestAllocs_Equal pins Equal at zero alloc. The Equal implementations
// dispatch on type and compare fields in place — none of them should
// box, slice-copy, or otherwise allocate. A regression here would
// usually mean someone introduced an interface conversion that
// escapes to heap.
func TestAllocs_Equal(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	left := buildDeepTree()
	right := buildDeepTree()
	got := testing.AllocsPerRun(100, func() {
		_ = left.Equal(right)
	})
	const ceiling = 0.0
	if got > ceiling {
		t.Errorf("Equal avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("Equal avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}

// TestAllocs_Walk pins Walk at a low ceiling. The visit closure itself
// escapes (it captures `count`); the Walk traversal itself only
// indirect-calls through Children() which returns the cached
// children slice on most nodes.
func TestAllocs_Walk(t *testing.T) {
	// AllocsPerRun forbids parallel execution.
	tree := buildDeepTree()
	got := testing.AllocsPerRun(100, func() {
		count := 0
		chplan.Walk(tree, func(_ chplan.Node) bool {
			count++
			return true
		})
	})
	// Baseline 5 allocs (Children() calls allocate a fresh []Node on
	// some nodes — Aggregate / Filter / Project). Ceiling = 2×.
	const ceiling = 12.0
	if got > ceiling {
		t.Errorf("Walk avg allocs = %.1f; want <= %.1f", got, ceiling)
	}
	t.Logf("Walk avg allocs = %.1f (ceiling %.1f)", got, ceiling)
}
