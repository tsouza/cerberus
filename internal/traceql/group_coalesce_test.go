package traceql_test

import (
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerGroupCoalesce covers the `| by(...)` (TraceQL's
// GroupOperation, conventionally spelled `group()` in user-facing docs
// but `by()` in the grammar — see Tempo expr.y) and `| coalesce()`
// pipeline elements. Each lowers to an Aggregate whose Input is the
// previous stage's plan; the GroupBy / AggFuncs differ.
func TestLowerGroupCoalesce(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	t.Run("group_by_attr", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{} | by(resource.service.name)`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		agg, ok := plan.(*chplan.Aggregate)
		if !ok {
			t.Fatalf("expected *chplan.Aggregate, got %T", plan)
		}
		if len(agg.GroupBy) != 1 {
			t.Errorf("len(GroupBy) = %d, want 1", len(agg.GroupBy))
		}
		// Three rep AggFuncs: any(TraceId), any(SpanId), min(Timestamp).
		if len(agg.AggFuncs) != 3 {
			t.Errorf("len(AggFuncs) = %d, want 3", len(agg.AggFuncs))
		}
		seen := map[string]int{}
		for _, af := range agg.AggFuncs {
			seen[af.Name]++
		}
		if seen["any"] != 2 || seen["min"] != 1 {
			t.Errorf("AggFunc kind counts = %v, want any=2 min=1", seen)
		}
	})

	t.Run("coalesce", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{} | coalesce()`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		agg, ok := plan.(*chplan.Aggregate)
		if !ok {
			t.Fatalf("expected *chplan.Aggregate, got %T", plan)
		}
		// coalesce() groups by (TraceId, SpanId).
		if len(agg.GroupBy) != 2 {
			t.Errorf("len(GroupBy) = %d, want 2", len(agg.GroupBy))
		}
		if len(agg.GroupByAliases) != 2 {
			t.Errorf("len(GroupByAliases) = %d, want 2", len(agg.GroupByAliases))
		}
		// Aliases should be the trace + span identity columns.
		if agg.GroupByAliases[0] != s.TraceIDColumn || agg.GroupByAliases[1] != s.SpanIDColumn {
			t.Errorf("GroupByAliases = %v, want [%q, %q]",
				agg.GroupByAliases, s.TraceIDColumn, s.SpanIDColumn)
		}
	})

	t.Run("group_missing_expr", func(t *testing.T) {
		t.Parallel()
		// `by()` with no expression should fail at parse time, but if a
		// caller hand-builds a GroupOperation with a nil Expression the
		// lowering surfaces a clean error rather than panicking.
		_, err := traceql.Lower(&tempo.RootExpr{
			Pipeline: tempo.Pipeline{Elements: []tempo.PipelineElement{
				&tempo.SpansetFilter{},
				tempo.GroupOperation{Expression: nil},
			}},
		}, s)
		if err == nil {
			t.Fatal("Lower: expected error from nil GroupOperation.Expression, got nil")
		}
	})
}
