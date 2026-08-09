package tempo

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestWrapWithSampleProjection_ArithmeticScalarFilter pins the
// /api/search wrap-projection side of #1708 (arithmetic between
// aggregates in a scalar filter). internal/traceql's
// lowerArithmeticScalarFilter inserts a Project between the outer
// Filter and the shared Aggregate — a shape isSpansetAggregateShape
// did not recognise before this fix (it only recursed through Filter,
// not Project). Without the fix, wrapWithSampleProjection would fall
// through to the Scan/Filter(Scan) default branch and reference raw
// span columns (SpanName, ResourceAttributes, Timestamp, Duration)
// that don't exist in the Aggregate's output scope, producing SQL
// that fails against a real ClickHouse ("unknown identifier"). This
// test catches that class of break locally, without a real
// ClickHouse: the wrapped plan must still be recognised as the
// spanset-aggregate search envelope and must emit successfully.
func TestWrapWithSampleProjection_ArithmeticScalarFilter(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	query := `{ resource.service.name = "frontend" } | max(duration) - min(duration) >= 0`
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}

	// Sanity: confirm the lowered shape actually has the Project layer
	// this test exists to cover (Filter -> Project -> Aggregate), so a
	// future change to lowerArithmeticScalarFilter that drops the
	// Project doesn't make this test vacuous.
	filter, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Filter", plan)
	}
	if _, ok := filter.Input.(*chplan.Project); !ok {
		t.Fatalf("Filter.Input = %T, want *chplan.Project", filter.Input)
	}

	if !isSpansetAggregateShape(plan) {
		t.Fatalf("isSpansetAggregateShape(plan) = false, want true — the Filter->Project->Aggregate shape must still be recognised as the spanset-aggregate search envelope")
	}

	wrapped := wrapWithSampleProjection(plan, s, engine.Meta{})
	outer, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("wrapped = %T, want *chplan.Project", wrapped)
	}

	// The spanset-aggregate branch's first projection reads the
	// ALREADY-ALIASED "MetricName" column the inner plan produced
	// (spansetAggregateSampleProjections); the Scan/Filter(Scan)
	// default branch would instead read the raw s.SpanNameColumn
	// ("SpanName") directly, which does not exist on this plan's
	// output. Checking the concrete Expr distinguishes the two
	// branches without a real ClickHouse.
	if len(outer.Projections) == 0 {
		t.Fatalf("wrapped Project has no Projections")
	}
	col, ok := outer.Projections[0].Expr.(*chplan.ColumnRef)
	if !ok || col.Name != "MetricName" {
		t.Fatalf("outer.Projections[0].Expr = %v, want ColumnRef(MetricName) (the spanset-aggregate branch's own re-read of its inner Project's alias, not the raw span-level %s column)", outer.Projections[0].Expr, s.SpanNameColumn)
	}

	if _, _, err := chsql.Emit(context.Background(), wrapped); err != nil {
		t.Fatalf("Emit(wrapped): %v", err)
	}
}
