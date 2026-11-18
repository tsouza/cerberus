package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestStageAfterGroupOrCoalesceReadsSpans pins that a stage or a metrics
// function after by() or coalesce() reads the spans those stages return, as
// reference Tempo's next stage and metrics evaluator do: by() keeps every
// span and only splits each trace's spanset, and coalesce() merges a trace's
// spansets back into one. Each query must lower exactly as the same query
// with those stages taken out, where nothing after them groups per by()
// spanset.
func TestStageAfterGroupOrCoalesceReadsSpans(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	pairs := [][2]string{
		{`{ } | coalesce() | rate()`, `{ } | rate()`},
		{`{ } | count() > 1 | coalesce() | rate()`, `{ } | count() > 1 | rate()`},
		{`{ } | by(name) | select(name) | rate()`, `{ } | select(name) | rate()`},
		{`{ } | by(name) | select(name)`, `{ } | select(name)`},
		{`{ } | by(name) | coalesce() | count() > 1`, `{ } | count() > 1`},
		{`{ } | by(name) | coalesce() | count() > 1 | rate()`, `{ } | count() > 1 | rate()`},
		{`{ } | count() > 1 | coalesce()`, `{ } | count() > 1 | coalesce() | coalesce()`},
		// A spanset-operation operand reads the spans of the spansets its
		// pipeline returns, as Tempo's && and || combine spans.
		{`({ } | by(name)) && ({ name = "c" })`, `({ }) && ({ name = "c" })`},
		{`({ } | coalesce()) || ({ name = "c" })`, `({ }) || ({ name = "c" })`},
	}
	for _, p := range pairs {
		got := lowerPlain(t, p[0], s)
		want := lowerPlain(t, p[1], s)
		if !got.Equal(want) {
			t.Errorf("%q did not lower equivalently to %q", p[0], p[1])
		}
	}
}

// TestAggregateAfterGroupEvaluatesPerGroup pins that a spanset aggregate
// after by() groups by the by() key as well as the trace, as reference
// Tempo's Aggregate evaluates once per input spanset, and that a stage after
// it reads the spans of each (trace, key) group it passes through a semi-join
// on that pair, the key in a one-element tuple so a NULL key matches its own
// group. Chained by() stages key on the tuple of their keys.
func TestAggregateAfterGroupEvaluatesPerGroup(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	// The key each by() stage lowers to on its own, read off its search
	// aggregate.
	byKey := func(stage string) chplan.Expr {
		agg, ok := lowerPlain(t, `{ } | `+stage, s).(*chplan.Aggregate)
		if !ok || len(agg.GroupBy) != 2 {
			t.Fatalf("%q does not lower to a per-group aggregate", stage)
		}
		return agg.GroupBy[1]
	}
	name, kind := byKey(`by(name)`), byKey(`by(kind)`)
	cases := []struct {
		query string
		key   chplan.Expr
	}{
		{`{ } | by(name) | count() > 1 | rate()`, name},
		{`{ } | by(name) | by(kind) | count() > 1 | rate()`, &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{name, kind}}},
		// A second aggregate after the first still evaluates per group: the
		// cohort read here is the second aggregate's.
		{`{ } | by(name) | count() > 1 | count() > 2 | rate()`, name},
		// A nullable aggregate carries a `Value IS NOT NULL` Filter under the
		// scalar filter's; both are unwrapped to reach the aggregate.
		{`{ } | by(name) | avg(span.x) > 1 | rate()`, name},
	}
	for _, tc := range cases {
		metrics, ok := lowerPlain(t, tc.query, s).(*chplan.MetricsAggregate)
		if !ok {
			t.Fatalf("%q: plan root is not a MetricsAggregate", tc.query)
		}
		f, ok := metrics.Inner.(*chplan.Filter)
		if !ok {
			t.Fatalf("%q: metrics input = %T, want the semi-join Filter", tc.query, metrics.Inner)
		}
		in, ok := f.Predicate.(*chplan.InSubquery)
		if !ok {
			t.Fatalf("%q: metrics input predicate = %T, want an InSubquery", tc.query, f.Predicate)
		}
		wantMember := &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
			&chplan.ColumnRef{Name: s.TraceIDColumn},
			&chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{tc.key}},
		}}
		if !in.Left.Equal(wantMember) {
			t.Errorf("%q: semi-join member does not key on (TraceId, tuple(%v))", tc.query, tc.key)
		}
		agg := spansetAggregate(in.Subquery.(*chplan.Project).Input, s)
		if agg == nil {
			t.Fatalf("%q: cohort holds no spanset aggregate", tc.query)
		}
		if len(agg.GroupBy) != len(spansetGroupByAliases) || !agg.GroupBy[1].Equal(tc.key) || agg.GroupByAliases[1] != groupKeyAlias {
			t.Errorf("%q: cohort aggregate groups by %v %v, want [TraceId, %v] [TraceId, %s]",
				tc.query, agg.GroupBy, agg.GroupByAliases, tc.key, groupKeyAlias)
		}
	}
}

// TestNullableAggregateFilterReadsSpans pins that a nullable aggregate under a
// scalar filter (`Filter(Value > N, Filter(Value IS NOT NULL, Aggregate))`)
// un-collapses to the spans of the traces it passes, so a metrics function
// after it observes spans.
func TestNullableAggregateFilterReadsSpans(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	metrics, ok := lowerPlain(t, `{ } | avg(span.x) > 1 | rate()`, s).(*chplan.MetricsAggregate)
	if !ok {
		t.Fatal("plan root is not a MetricsAggregate")
	}
	f, ok := metrics.Inner.(*chplan.Filter)
	if !ok {
		t.Fatalf("metrics input = %T, want the semi-join Filter", metrics.Inner)
	}
	in, ok := f.Predicate.(*chplan.InSubquery)
	if !ok || !in.Left.Equal(&chplan.ColumnRef{Name: s.TraceIDColumn}) {
		t.Fatalf("metrics input predicate = %v, want TraceId IN (cohort)", f.Predicate)
	}
}
