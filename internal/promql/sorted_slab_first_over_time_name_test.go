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

// TestSortedSlabFirstOverTime_PreservesName is the load-bearing regression
// pin for cerberus issue #2804's widening of SortedSlabOverTimeLowerer to
// first_over_time. Unlike sum_over_time/avg_over_time (which drop
// `__name__`), first_over_time keeps it — rangeFnPreservesName — and
// SortedSlabOverTimeLowerer.LowerOverTime returns a shallow COPY of the
// fan-out RangeWindow (a DIFFERENT pointer, but still a plain
// *chplan.RangeWindow with the identical derived output schema), not the
// RangeWindow it was handed and not a different, already-canonical node
// type the way NativeLastOverTimeLowerer's RangeWindowStaleResample is.
//
// lowerRangeVectorCall's name-preservation seam has to recognise that copy
// as STILL needing the synthesis wrap. An earlier version of that check
// compared `node != chplan.Node(rw)` by POINTER, which this copy would have
// satisfied despite being just as name-less as rw — silently dropping
// `__name__` from every sorted-slab first_over_time query_range result (see
// lowerRangeVectorCall's own doc for the full argument). This test lowers
// the query with SortedSlabOverTimeLowerer actually wired and asserts BOTH
// halves: the sorted-slab flag survived onto the wrapped node, and the
// canonical MetricName projection wraps it.
func TestSortedSlabFirstOverTime_PreservesName(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr("first_over_time(cpu_temp_celsius[5m])")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	lowerers := promql.RangeLowerers{
		OverTime: promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}},
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 30*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatal(err)
	}

	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project (the __name__-preservation wrap) — sorted-slab "+
			"first_over_time lost __name__ preservation", plan)
	}
	cols := chplan.SampleColumns{
		MetricName: s.MetricNameColumn, Attributes: s.AttributesColumn,
		Timestamp: s.TimestampColumn, Value: s.ValueColumn,
	}
	if !chplan.ProjectExposesCanonical(proj, cols) {
		t.Fatalf("Project does not expose the canonical 4-column (MetricName, Attributes, Timestamp, "+
			"Value) shape: %#v", proj.Projections)
	}
	lit, ok := proj.Projections[0].Expr.(*chplan.LitString)
	if !ok || lit.V != "cpu_temp_celsius" {
		t.Fatalf("MetricName projection = %#v, want LitString(%q)", proj.Projections[0].Expr, "cpu_temp_celsius")
	}

	rw, ok := proj.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("Project.Input = %T, want *chplan.RangeWindow", proj.Input)
	}
	if !rw.SortedSlabOverTime {
		t.Fatalf("the wrapped RangeWindow lost SortedSlabOverTime=true — the name-preservation wrap must " +
			"wrap the STRATEGY's returned node, not the stale original fan-out rw")
	}
}

// TestSortedSlabFirstOverTime_RegexNamePreservesPerSeries is the regex-name
// sibling of TestSortedSlabFirstOverTime_PreservesName: a `__name__=~"a|b"`
// matcher spans several metrics whose names differ per series, so the name
// must ride through the sorted-slab RangeWindow's OWN widened grouping key
// (appendNameGroupKey) rather than a single literal — proving the fix
// mutates the ACTUAL strategy-returned node, not a copy the emitter never
// sees.
func TestSortedSlabFirstOverTime_RegexNamePreservesPerSeries(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(`first_over_time({__name__=~"cpu_temp_celsius|gpu_temp_celsius"}[5m])`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	lowerers := promql.RangeLowerers{
		OverTime: promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}},
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 30*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatal(err)
	}

	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project", plan)
	}
	rw, ok := proj.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("Project.Input = %T, want *chplan.RangeWindow", proj.Input)
	}
	if !rw.SortedSlabOverTime {
		t.Fatalf("the wrapped RangeWindow lost SortedSlabOverTime=true")
	}
	var sawMetricNameKey bool
	for _, g := range rw.GroupBy {
		if ref, ok := g.(*chplan.ColumnRef); ok && ref.Name == s.MetricNameColumn {
			sawMetricNameKey = true
		}
	}
	if !sawMetricNameKey {
		t.Fatalf("RangeWindow.GroupBy = %#v, want it widened with MetricName for the regex-name case", rw.GroupBy)
	}
}
