package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerPointerFormDispatch pins the pointer-form arms in the
// lowering dispatchers (lowerPipelineElement / lowerFollowingElement /
// lowerSpansetExpr). Tempo's parser emits these AST nodes by value, so
// the pointer arms are defensive — they only fire when a caller hand-
// builds the AST. This test reaches them by reparsing each shape,
// extracting the parser-produced value-form element, wrapping it in a
// pointer, and re-running Lower on the rewritten RootExpr.
//
// Without this test, the pointer arms are dead code from coverage's
// perspective; with it, a future change that drops one of those arms
// (or breaks pointer dispatch) shows up as a test failure rather than
// a coverage regression that audit tooling alone has to surface.
func TestLowerPointerFormDispatch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	t.Run("pipeline_element_spanset_op_pointer", func(t *testing.T) {
		t.Parallel()
		// `{ a } && { b }` parses with Pipeline[0] = traceql.SpansetOperation (value).
		// Wrapping it in a pointer hits lowerPipelineElement's
		// `*traceql.SpansetOperation` arm.
		expr := mustParse(t, `{ resource.service.name = "a" } && { resource.service.name = "b" }`)
		op, ok := expr.Pipeline.Elements[0].(tempo.SpansetOperation)
		if !ok {
			t.Fatalf("Pipeline[0] = %T; want SpansetOperation (value)", expr.Pipeline.Elements[0])
		}
		expr.Pipeline.Elements[0] = &op
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*SpansetOperation): %v", err)
		}
	})

	t.Run("spanset_expr_spanset_op_pointer", func(t *testing.T) {
		t.Parallel()
		// `({ a } && { b }) >> { c }` parses with the LHS of the outer
		// SpansetOperation being a value-form SpansetOperation. Wrapping
		// the inner LHS in a pointer hits lowerSpansetExpr's
		// `*traceql.SpansetOperation` arm.
		expr := mustParse(t, `({ resource.service.name = "a" } && { resource.service.name = "b" }) >> { resource.service.name = "c" }`)
		outer, ok := expr.Pipeline.Elements[0].(tempo.SpansetOperation)
		if !ok {
			t.Fatalf("Pipeline[0] = %T; want SpansetOperation (value)", expr.Pipeline.Elements[0])
		}
		inner, ok := outer.LHS.(tempo.SpansetOperation)
		if !ok {
			t.Fatalf("outer.LHS = %T; want SpansetOperation (value)", outer.LHS)
		}
		outer.LHS = &inner
		expr.Pipeline.Elements[0] = outer
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(SpansetOperation with *SpansetOperation LHS): %v", err)
		}
	})

	t.Run("following_aggregate_pointer", func(t *testing.T) {
		t.Parallel()
		// `{ a } | count() > 0` parses as Pipeline = [SpansetFilter,
		// ScalarFilter]. The ScalarFilter's LHS is an Aggregate (value).
		// Wrap that Aggregate in a pointer and stuff it in the Pipeline
		// directly: the result is Pipeline = [SpansetFilter, *Aggregate]
		// which exercises lowerFollowingElement's `*traceql.Aggregate`
		// arm. (The grammar doesn't write a bare aggregate to a pipeline
		// element, but the dispatcher allows for it.)
		expr := mustParse(t, `{ resource.service.name = "a" } | count() > 0`)
		sf, ok := expr.Pipeline.Elements[1].(tempo.ScalarFilter)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want ScalarFilter", expr.Pipeline.Elements[1])
		}
		agg, ok := sf.LHS.(tempo.Aggregate)
		if !ok {
			t.Fatalf("ScalarFilter.LHS = %T; want Aggregate", sf.LHS)
		}
		expr.Pipeline.Elements[1] = &agg
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*Aggregate as pipeline tail): %v", err)
		}
	})

	t.Run("following_scalar_filter_pointer", func(t *testing.T) {
		t.Parallel()
		// Parsed ScalarFilter is value-form; wrap in pointer to hit the
		// pointer arm.
		expr := mustParse(t, `{ resource.service.name = "a" } | count() > 0`)
		sf, ok := expr.Pipeline.Elements[1].(tempo.ScalarFilter)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want ScalarFilter (value)", expr.Pipeline.Elements[1])
		}
		expr.Pipeline.Elements[1] = &sf
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*ScalarFilter): %v", err)
		}
	})

	t.Run("following_select_pointer", func(t *testing.T) {
		t.Parallel()
		expr := mustParse(t, `{ resource.service.name = "a" } | select(.foo, .bar)`)
		sel, ok := expr.Pipeline.Elements[1].(tempo.SelectOperation)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want SelectOperation (value)", expr.Pipeline.Elements[1])
		}
		expr.Pipeline.Elements[1] = &sel
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*SelectOperation): %v", err)
		}
	})

	t.Run("following_group_pointer", func(t *testing.T) {
		t.Parallel()
		expr := mustParse(t, `{ resource.service.name = "a" } | by(resource.service.name)`)
		grp, ok := expr.Pipeline.Elements[1].(tempo.GroupOperation)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want GroupOperation (value)", expr.Pipeline.Elements[1])
		}
		expr.Pipeline.Elements[1] = &grp
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*GroupOperation): %v", err)
		}
	})

	t.Run("following_coalesce_pointer", func(t *testing.T) {
		t.Parallel()
		expr := mustParse(t, `{ resource.service.name = "a" } | coalesce()`)
		co, ok := expr.Pipeline.Elements[1].(tempo.CoalesceOperation)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want CoalesceOperation (value)", expr.Pipeline.Elements[1])
		}
		expr.Pipeline.Elements[1] = &co
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(*CoalesceOperation): %v", err)
		}
	})

	t.Run("scalar_expr_aggregate_pointer", func(t *testing.T) {
		t.Parallel()
		// ScalarFilter LHS is an Aggregate (value). Wrap as pointer to
		// hit lowerScalarExpr's `*traceql.Aggregate` arm.
		expr := mustParse(t, `{ resource.service.name = "a" } | count() > 0`)
		sf, ok := expr.Pipeline.Elements[1].(tempo.ScalarFilter)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want ScalarFilter", expr.Pipeline.Elements[1])
		}
		agg, ok := sf.LHS.(tempo.Aggregate)
		if !ok {
			t.Fatalf("ScalarFilter.LHS = %T; want Aggregate (value)", sf.LHS)
		}
		sf.LHS = &agg
		expr.Pipeline.Elements[1] = sf
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(ScalarFilter with *Aggregate LHS): %v", err)
		}
	})

	t.Run("scalar_expr_static_pointer", func(t *testing.T) {
		t.Parallel()
		// ScalarFilter RHS is a Static (value). Wrap as pointer to hit
		// lowerScalarExpr's `*traceql.Static` arm.
		expr := mustParse(t, `{ resource.service.name = "a" } | count() > 0`)
		sf, ok := expr.Pipeline.Elements[1].(tempo.ScalarFilter)
		if !ok {
			t.Fatalf("Pipeline[1] = %T; want ScalarFilter", expr.Pipeline.Elements[1])
		}
		st, ok := sf.RHS.(tempo.Static)
		if !ok {
			t.Fatalf("ScalarFilter.RHS = %T; want Static (value)", sf.RHS)
		}
		sf.RHS = &st
		expr.Pipeline.Elements[1] = sf
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(ScalarFilter with *Static RHS): %v", err)
		}
	})
}

func mustParse(t *testing.T, q string) *tempo.RootExpr {
	t.Helper()
	expr, err := tempo.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return expr
}
