package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerScalarFilter_ArithmeticBetweenAggregates pins the #1708 fix:
// arithmetic between two (or more) aggregates in a scalar-filter
// pipeline stage — `| max(duration) - min(duration) >= 0` — used to
// fail lowering with "traceql: scalar expression ast.ScalarOperation
// is unsupported" because lowerScalarExpr's type switch had no case
// for ast.ScalarOperation. lowerArithmeticScalarFilter now folds every
// aggregate leaf across LHS and RHS into ONE chplan.Aggregate node
// (shared TraceId grouping, same envelope columns lowerAggregate uses
// for the single-aggregate shape) and computes the arithmetic in a
// Project on top, so the Filter compares two plain columns instead of
// two independently-grouped Aggregate nodes with no shared row.
func TestLowerScalarFilter_ArithmeticBetweenAggregates(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	t.Run("flat_two_aggregates", func(t *testing.T) {
		t.Parallel()
		query := `{ resource.service.name = "frontend" } | max(duration) - min(duration) >= 0`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}

		filter, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("plan = %T, want *chplan.Filter", plan)
		}
		project, ok := filter.Input.(*chplan.Project)
		if !ok {
			t.Fatalf("Filter.Input = %T, want *chplan.Project", filter.Input)
		}
		agg, ok := project.Input.(*chplan.Aggregate)
		if !ok {
			t.Fatalf("Project.Input = %T, want *chplan.Aggregate", project.Input)
		}

		// Two distinct per-leaf AggFuncs (max(Duration), min(Duration))
		// feed the arithmetic, aliased __cerberus_scalar_agg0/1 — under
		// the aggValueAlias-style leaf-alias prefix, not the envelope's
		// own "min"/"max" (TraceStartNs / TraceEndNs also use min/max
		// internally, so counting by Name alone would double-count).
		aliasFuncs := map[string]string{}
		aliases := map[string]bool{}
		for _, af := range agg.AggFuncs {
			aliases[af.Alias] = true
			aliasFuncs[af.Alias] = af.Name
		}
		if aliasFuncs["__cerberus_scalar_agg0"] != "max" {
			t.Errorf("agg leaf 0 = %q, want max", aliasFuncs["__cerberus_scalar_agg0"])
		}
		if aliasFuncs["__cerberus_scalar_agg1"] != "min" {
			t.Errorf("agg leaf 1 = %q, want min", aliasFuncs["__cerberus_scalar_agg1"])
		}
		for _, want := range []string{"MetricName", "ResourceAttrs", "TimeUnix", "TraceStartNs", "TraceEndNs"} {
			if !aliases[want] {
				t.Errorf("Aggregate.AggFuncs missing envelope alias %q", want)
			}
		}
		if len(agg.GroupBy) != 1 {
			t.Fatalf("len(GroupBy) = %d, want 1 (TraceId)", len(agg.GroupBy))
		}

		// The Project must republish the arithmetic under aggValueAlias
		// ("Value") and pass every envelope column through unchanged, so
		// internal/api/tempo's isSpansetAggregateShape keeps recognising
		// this as the spanset-aggregate search envelope through the
		// extra Project layer.
		gotAliases := map[string]bool{}
		for _, p := range project.Projections {
			gotAliases[p.Alias] = true
		}
		for _, want := range []string{"Value", "MetricName", "ResourceAttrs", "TimeUnix", "TraceStartNs", "TraceEndNs", "TraceId"} {
			if !gotAliases[want] {
				t.Errorf("Project.Projections missing alias %q", want)
			}
		}

		// The Filter must compare Value against the projected RHS
		// column, not embed a literal directly (RHS may itself carry
		// aggregate leaves in the general path).
		pred, ok := filter.Predicate.(*chplan.Binary)
		if !ok {
			t.Fatalf("Filter.Predicate = %T, want *chplan.Binary", filter.Predicate)
		}
		if pred.Op != chplan.OpGe {
			t.Errorf("Predicate.Op = %v, want OpGe", pred.Op)
		}
		left, ok := pred.Left.(*chplan.ColumnRef)
		if !ok || left.Name != "Value" {
			t.Errorf("Predicate.Left = %v, want ColumnRef(Value)", pred.Left)
		}

		// The plan must actually emit — invariant 10 (typed chsql Frags
		// only) and a sanity check that the shape chsql understands.
		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})

	t.Run("nested_scalar_operation", func(t *testing.T) {
		t.Parallel()
		// Mixed precedence nests a ScalarOperation inside a
		// ScalarOperation: `/` binds tighter than `-`, so this parses as
		// max(duration) - (min(duration) / avg(duration)) — three
		// aggregate leaves feeding two levels of arithmetic, pinning
		// that lowerScalarOperand's recursion composes arbitrarily deep
		// chplan.Binary trees rather than just one level.
		query := `{ resource.service.name = "frontend" } | max(duration) - min(duration) / avg(duration) >= 0.5`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}

		filter, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("plan = %T, want *chplan.Filter", plan)
		}
		project, ok := filter.Input.(*chplan.Project)
		if !ok {
			t.Fatalf("Filter.Input = %T, want *chplan.Project", filter.Input)
		}
		agg, ok := project.Input.(*chplan.Aggregate)
		if !ok {
			t.Fatalf("Project.Input = %T, want *chplan.Aggregate", project.Input)
		}

		// Three per-leaf AggFuncs, one per aggregate in the expression
		// (max, min, avg over Duration), aliased __cerberus_scalar_agg0-2
		// — checked by alias rather than by Name, for the same reason as
		// the flat case above (the envelope columns also use min/max).
		aliasFuncs := map[string]string{}
		for _, af := range agg.AggFuncs {
			aliasFuncs[af.Alias] = af.Name
		}
		wantLeaves := map[string]string{
			"__cerberus_scalar_agg0": "max",
			"__cerberus_scalar_agg1": "min",
			"__cerberus_scalar_agg2": "avg",
		}
		for alias, wantFn := range wantLeaves {
			if got := aliasFuncs[alias]; got != wantFn {
				t.Errorf("agg leaf %s = %q, want %q", alias, got, wantFn)
			}
		}

		// The Value projection must be a Binary(Sub) whose right operand
		// is itself a Binary(Div) — the nested composition.
		var valueExpr chplan.Expr
		for _, p := range project.Projections {
			if p.Alias == "Value" {
				valueExpr = p.Expr
			}
		}
		outer, ok := valueExpr.(*chplan.Binary)
		if !ok {
			t.Fatalf("Value projection = %T, want *chplan.Binary", valueExpr)
		}
		if outer.Op != chplan.OpSub {
			t.Errorf("outer op = %v, want OpSub", outer.Op)
		}
		if _, ok := outer.Right.(*chplan.Binary); !ok {
			t.Errorf("outer.Right = %T, want *chplan.Binary (nested Div)", outer.Right)
		}

		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})

	t.Run("aggregate_directly_on_rhs", func(t *testing.T) {
		t.Parallel()
		// Not arithmetic at all, but a shape lowerSimpleScalarFilter
		// never supported either: an aggregate on BOTH sides of the
		// comparison (`max(duration) > avg(duration)`). Falls out of the
		// same general mechanism for free, since both sides feed the
		// shared Aggregate node symmetrically.
		query := `{ resource.service.name = "frontend" } | max(duration) > avg(duration)`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	})

	t.Run("count_vs_count", func(t *testing.T) {
		t.Parallel()
		// `count() > count()` used to fail with "scalar-filter RHS must
		// be a literal" (lowerScalarFilter assumed only the LHS could
		// aggregate). Now both sides fold into the shared Aggregate.
		query := `{ resource.service.name = "frontend" } | count() > count()`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
	})

	t.Run("no_aggregate_either_side_still_rejects", func(t *testing.T) {
		t.Parallel()
		// The pathological `{} | 1 > 2` shape (mirrors the #324 `{}|0>0`
		// regression pin in lower_panic_regression_test.go) must still
		// error cleanly rather than silently accept a filter with no
		// series to aggregate.
		query := `{} | 1 > 2`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err == nil {
			t.Fatalf("Lower(%q): want error, got nil", query)
		}
	})

	t.Run("simple_shape_unchanged", func(t *testing.T) {
		t.Parallel()
		// The single-aggregate-vs-literal fast path must stay exactly
		// the pre-#1708 plan shape: Filter directly wrapping the lone
		// Aggregate node, no Project in between.
		query := `{ resource.service.name = "frontend" } | count() > 0`
		expr, err := tempo.Parse(query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		filter, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("plan = %T, want *chplan.Filter", plan)
		}
		if _, ok := filter.Input.(*chplan.Aggregate); !ok {
			t.Fatalf("Filter.Input = %T, want *chplan.Aggregate (no Project)", filter.Input)
		}
	})
}
