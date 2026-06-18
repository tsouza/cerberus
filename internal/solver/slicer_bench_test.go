package solver

import (
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// BenchmarkSlice measures the plan-slicing hot path — one unpinSpine plus K
// ReanchorRange calls — at K=2/4/8/16. The copy-on-write rewrite shares the
// immutable off-spine subtree instead of CloneNode-ing it K+1 times, so this
// benchmark is dominated by the O(K x spine-depth) spine clone rather than the
// O(K x plan-size) full deep copy the pre-COW path paid.
//
// The plan mirrors `sum by (job)(rate(http_requests_total[5m]))`: a matrix
// RangeWindow spine over a Filter-over-Scan off-spine subtree, wrapped in an
// Aggregate + Project. The off-spine subtree carries a wide projection /
// predicate so the shared-vs-copied difference is material.
//
// It is picked up by the weekly perf-benchmark lane (`go test -bench=.` +
// benchstat over internal/solver) exactly like the existing chplan
// BenchmarkEqual / BenchmarkWalk micro-benchmarks; no new doc convention is
// introduced.
func BenchmarkSlice(b *testing.B) {
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour) // 241 anchors
	meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

	plan := benchSlicePlan(start, end, step, 5*time.Minute)
	p := &Planner{Cfg: autoCfg()}

	for _, k := range []int{2, 4, 8, 16} {
		k := k
		b.Run(fmt.Sprintf("K=%d", k), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				slices, err := p.slice(plan, meta, k)
				if err != nil {
					b.Fatalf("slice: %v", err)
				}
				if len(slices) < 2 {
					b.Fatalf("expected >= 2 slices, got %d", len(slices))
				}
			}
		})
	}
}

// benchSlicePlan builds a realistic single-spine plan with a non-trivial
// off-spine subtree (a Filter-over-Scan with a wide projection above) so the
// COW off-spine sharing has a meaningful subtree to NOT copy.
func benchSlicePlan(start, end time.Time, step, rang time.Duration) chplan.Node {
	scan := &chplan.Scan{
		Table:   "otel_metrics_sum",
		Columns: []string{"MetricName", "Attributes", "TimeUnix", "Value", "ResourceAttributes", "ScopeName"},
	}
	filter := &chplan.Filter{
		Input: scan,
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	}
	rw := &chplan.RangeWindow{
		Input:           filter,
		Func:            "rate",
		Range:           rang,
		Step:            step,
		OuterRange:      end.Sub(start),
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
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
