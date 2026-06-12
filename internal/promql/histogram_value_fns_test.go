package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HistogramValueFns_InstantLatestSample pins the instant-mode
// plan shape for the six native-histogram value functions over a bare
// exp-hist selector: the filtered scan is aggregated with
// argMax(<col>, TimeUnix) GROUP BY [Attributes] so the value math reads
// the newest sample per series. Before the fix the lowering emitted a
// bare Project(Filter(Scan)) — every historical sample per series, all
// stamped now64(9).
func TestLower_HistogramValueFns_InstantLatestSample(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	for _, q := range []string{
		`histogram_count(my_exp_hist)`,
		`histogram_sum(my_exp_hist)`,
		`histogram_avg(my_exp_hist)`,
		`histogram_stddev(my_exp_hist)`,
		`histogram_stdvar(my_exp_hist)`,
		`histogram_fraction(0.2, 0.8, my_exp_hist)`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", q, err)
			}
			pj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("plan = %T, want *chplan.Project", plan)
			}
			agg, ok := pj.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("Project.Input = %T, want *chplan.Aggregate (per-series latest-sample selection)", pj.Input)
			}
			// GROUP BY Attributes only (no per-anchor key in instant mode).
			if len(agg.GroupBy) != 1 {
				t.Fatalf("instant GroupBy len = %d, want 1 (Attributes)", len(agg.GroupBy))
			}
			if got := colName(t, agg.GroupBy[0]); got != s.AttributesColumn {
				t.Errorf("instant GroupBy[0] = %q, want %q", got, s.AttributesColumn)
			}
			assertArgMaxLatestAggs(t, s, agg.AggFuncs)
		})
	}
}

// TestLower_HistogramValueFns_RangeLatestSample pins the range-mode plan
// shape: a single-pass RangeBucketFanout keyed by [anchor_ts (implicit),
// Attributes] with argMax(<col>, TimeUnix) so each step emits one row
// carrying the newest in-window sample. Before the fix range queries
// emitted N rows all stamped now64(9), so the matrix pivot collapsed to
// empty; before this rework the fan-out was the O(rows × N) StepGrid
// CROSS JOIN, which RangeBucketFanout supersedes.
func TestLower_HistogramValueFns_RangeLatestSample(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second

	for _, q := range []string{
		`histogram_count(my_exp_hist)`,
		`histogram_sum(my_exp_hist)`,
		`histogram_avg(my_exp_hist)`,
		`histogram_stddev(my_exp_hist)`,
		`histogram_stdvar(my_exp_hist)`,
		`histogram_fraction(0.2, 0.8, my_exp_hist)`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", q, err)
			}
			pj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("plan = %T, want *chplan.Project", plan)
			}
			// TimeUnix must surface anchor_ts, not now64(9), so the matrix
			// pivot sees one row per (series, step).
			if got := timeUnixAlias(t, s, pj); got != "anchor_ts" {
				t.Errorf("range TimeUnix projection = %q, want column ref anchor_ts", got)
			}
			fanout, ok := pj.Input.(*chplan.RangeBucketFanout)
			if !ok {
				t.Fatalf("Project.Input = %T, want *chplan.RangeBucketFanout", pj.Input)
			}
			// The anchor key is implicit (AnchorAlias); the user group key
			// is the full Attributes column.
			if fanout.AnchorAlias != "anchor_ts" {
				t.Errorf("AnchorAlias = %q, want anchor_ts", fanout.AnchorAlias)
			}
			if len(fanout.GroupBy) != 1 {
				t.Fatalf("range GroupBy len = %d, want 1 ([Attributes])", len(fanout.GroupBy))
			}
			if got := colName(t, fanout.GroupBy[0]); got != s.AttributesColumn {
				t.Errorf("range GroupBy[0] = %q, want %q", got, s.AttributesColumn)
			}
			assertArgMaxLatestAggs(t, s, fanout.AggFuncs)
			// The single-pass fan-out must NOT contain the old O(rows × N)
			// StepGrid CROSS JOIN scaffold.
			var sawStepGrid, sawCrossJoin bool
			chplan.Walk(fanout.Input, func(n chplan.Node) bool {
				switch n.(type) {
				case *chplan.StepGrid:
					sawStepGrid = true
				case *chplan.CrossJoin:
					sawCrossJoin = true
				}
				return true
			})
			if sawStepGrid {
				t.Errorf("range fan-out must not contain a StepGrid (single-pass invariant)")
			}
			if sawCrossJoin {
				t.Errorf("range fan-out must not contain a CrossJoin (single-pass invariant)")
			}
		})
	}
}

// assertArgMaxLatestAggs verifies the Aggregate's AggFuncs are exactly
// the eight argMax(<col>, TimeUnix) selections, one per exp-hist column
// the value math reads, each aliased back to its source column.
func assertArgMaxLatestAggs(t *testing.T, s schema.Metrics, aggs []chplan.AggFunc) {
	t.Helper()
	want := []string{
		s.CountColumn,
		s.SumColumn,
		s.ScaleColumn,
		s.ZeroCountColumn,
		s.PositiveOffsetColumn,
		s.PositiveBucketCountsColumn,
		s.NegativeOffsetColumn,
		s.NegativeBucketCountsColumn,
	}
	if len(aggs) != len(want) {
		t.Fatalf("AggFuncs len = %d, want %d", len(aggs), len(want))
	}
	for i, col := range want {
		a := aggs[i]
		if a.Name != "argMax" {
			t.Errorf("AggFuncs[%d].Name = %q, want argMax", i, a.Name)
		}
		if a.Alias != col {
			t.Errorf("AggFuncs[%d].Alias = %q, want %q", i, a.Alias, col)
		}
		if len(a.Args) != 2 {
			t.Fatalf("AggFuncs[%d] args len = %d, want 2", i, len(a.Args))
		}
		if got := colName(t, a.Args[0]); got != col {
			t.Errorf("AggFuncs[%d] argMax value col = %q, want %q", i, got, col)
		}
		if got := colName(t, a.Args[1]); got != s.TimestampColumn {
			t.Errorf("AggFuncs[%d] argMax order col = %q, want %q", i, got, s.TimestampColumn)
		}
	}
}

// colName returns the column name of a *chplan.ColumnRef expression, or
// fails the test if the expression is not a column ref.
func colName(t *testing.T, e chplan.Expr) string {
	t.Helper()
	cr, ok := e.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("expr = %T, want *chplan.ColumnRef", e)
	}
	return cr.Name
}

// timeUnixAlias returns the column name backing the projection that the
// top-level Project aliases to TimeUnix, or "" if it is not a plain
// column ref (e.g. now64(9)).
func timeUnixAlias(t *testing.T, s schema.Metrics, pj *chplan.Project) string {
	t.Helper()
	for _, proj := range pj.Projections {
		if proj.Alias == s.TimestampColumn {
			if cr, ok := proj.Expr.(*chplan.ColumnRef); ok {
				return cr.Name
			}
			return ""
		}
	}
	t.Fatalf("no projection aliased to %q", s.TimestampColumn)
	return ""
}
