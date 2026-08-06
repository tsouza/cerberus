package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// bindGate is a BoundedTraceScope carrying the structure-tab search's scope
// parameters; the option functions below perturb exactly one of them so a test
// can assert what "the sites disagree" costs.
func bindGate(mut ...func(*chplan.BoundedTraceScope)) *chplan.BoundedTraceScope {
	g := &chplan.BoundedTraceScope{
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
		TraceLimit:         20,
		WindowStartNano:    1699996400000000000,
		WindowEndNano:      1700000000000000000,
	}
	for _, m := range mut {
		m(g)
	}
	return g
}

func bindWalk(over chplan.Node) *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              over,
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
		TraceLimit:         20,
		WindowStartNano:    1699996400000000000,
		WindowEndNano:      1700000000000000000,
	}
}

// gatedLeaf is one row-source leaf scoped by gate — the shape
// traceql.pushLeafPredicate produces at every leaf of a bounded search plan.
func gatedLeaf(gate *chplan.BoundedTraceScope) chplan.Node {
	return &chplan.Filter{Input: &chplan.Scan{Table: "otel_traces"}, Predicate: gate}
}

// TestBindBoundedTraceScope_StampsAgreeingSites pins the collapse: when every
// gate and the numbering walk describe the SAME trace set, all of them are
// stamped with the shared alias and the returned scope carries it too, so the
// emitter can bind the top-N once and have every site test that binding.
func TestBindBoundedTraceScope_StampsAgreeingSites(t *testing.T) {
	t.Parallel()
	g1, g2 := bindGate(), bindGate()
	walk := bindWalk(&chplan.SetOperation{
		Op:    chplan.SetUnion,
		Left:  gatedLeaf(g1),
		Right: gatedLeaf(g2),
	})

	bound := chplan.BindBoundedTraceScope(walk)
	if bound == nil {
		t.Fatal("agreeing sites must bind, got nil")
	}
	if bound.BindingAlias != chplan.BoundedTraceScopeAlias {
		t.Errorf("returned scope alias = %q, want %q", bound.BindingAlias, chplan.BoundedTraceScopeAlias)
	}
	// The returned scope must still describe the same trace set — it is what
	// the emitter renders the single top-N subquery from.
	if !bound.SameScope(g1) {
		t.Errorf("returned scope diverged from the gates it binds: %+v vs %+v", bound, g1)
	}
	for i, g := range []*chplan.BoundedTraceScope{g1, g2} {
		if g.BindingAlias != chplan.BoundedTraceScopeAlias {
			t.Errorf("gate %d not stamped: alias = %q", i, g.BindingAlias)
		}
	}
	if walk.ScopeBindingAlias != chplan.BoundedTraceScopeAlias {
		t.Errorf("numbering walk not stamped: alias = %q", walk.ScopeBindingAlias)
	}
}

// TestBindBoundedTraceScope_Idempotent pins what makes stamping in place safe:
// the alias is a constant, not a counter, so re-binding an already-stamped
// tree — which the sharded-pushdown solver does, emitting K re-anchored deep
// copies — converges on the same alias rather than accumulating suffixes.
func TestBindBoundedTraceScope_Idempotent(t *testing.T) {
	t.Parallel()
	g := bindGate()
	root := gatedLeaf(g)

	first := chplan.BindBoundedTraceScope(root)
	second := chplan.BindBoundedTraceScope(root)
	if first == nil || second == nil {
		t.Fatalf("both binds must succeed, got %v / %v", first, second)
	}
	if *first != *second {
		t.Errorf("re-binding changed the bound scope: %+v then %+v", first, second)
	}
	if g.BindingAlias != chplan.BoundedTraceScopeAlias {
		t.Errorf("gate alias after two binds = %q, want %q", g.BindingAlias, chplan.BoundedTraceScopeAlias)
	}
}

// TestBindBoundedTraceScope_DisagreeingSitesDoNotBind pins the safety rule:
// one binding can serve only sites that select the SAME traces, so a tree
// whose sites disagree on any scope parameter binds nothing and every site
// keeps its own subquery. Each subtest perturbs exactly one parameter, so a
// SameScope comparison that silently stopped checking that field is caught.
func TestBindBoundedTraceScope_DisagreeingSitesDoNotBind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(*chplan.BoundedTraceScope)
	}{
		{"table", func(g *chplan.BoundedTraceScope) { g.SpansTable = "otel_traces_other" }},
		{"trace id column", func(g *chplan.BoundedTraceScope) { g.TraceIDColumn = "Tid" }},
		{"parent span id column", func(g *chplan.BoundedTraceScope) { g.ParentSpanIDColumn = "Parent" }},
		{"timestamp column", func(g *chplan.BoundedTraceScope) { g.TimestampColumn = "Ts" }},
		{"trace limit", func(g *chplan.BoundedTraceScope) { g.TraceLimit = 21 }},
		{"window start", func(g *chplan.BoundedTraceScope) { g.WindowStartNano = 1 }},
		{"window end", func(g *chplan.BoundedTraceScope) { g.WindowEndNano = 2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agreeing, odd := bindGate(), bindGate(tc.mut)
			root := &chplan.SetOperation{
				Op:    chplan.SetUnion,
				Left:  gatedLeaf(agreeing),
				Right: gatedLeaf(odd),
			}
			if bound := chplan.BindBoundedTraceScope(root); bound != nil {
				t.Errorf("sites disagreeing on %s must not bind, got %+v", tc.name, bound)
			}
			if agreeing.BindingAlias != "" || odd.BindingAlias != "" {
				t.Errorf("a refused bind must stamp nothing, got %q / %q", agreeing.BindingAlias, odd.BindingAlias)
			}
		})
	}
}

// TestBindBoundedTraceScope_WalkDisagreeingWithGateDoesNotBind pins the
// cross-kind half of the same rule. A numbering walk scoped to a different
// trace set than the row-source gates must not share their binding: numbering
// and row source ranging over different traces is exactly what strands kept
// rows at the 0/0/0 LEFT-JOIN default.
func TestBindBoundedTraceScope_WalkDisagreeingWithGateDoesNotBind(t *testing.T) {
	t.Parallel()
	g := bindGate()
	walk := bindWalk(gatedLeaf(g))
	walk.TraceLimit = 200

	if bound := chplan.BindBoundedTraceScope(walk); bound != nil {
		t.Errorf("walk disagreeing with the gate must not bind, got %+v", bound)
	}
	if g.BindingAlias != "" || walk.ScopeBindingAlias != "" {
		t.Errorf("a refused bind must stamp nothing, got %q / %q", g.BindingAlias, walk.ScopeBindingAlias)
	}
}

// TestBindBoundedTraceScope_NoSites pins the no-op path every PromQL / LogQL
// plan and every unbounded TraceQL one takes: nothing renders a top-N, so
// there is nothing to bind and the tree is returned untouched.
func TestBindBoundedTraceScope_NoSites(t *testing.T) {
	t.Parallel()
	unbounded := bindWalk(&chplan.Scan{Table: "otel_traces"})
	unbounded.TraceLimit = 0

	if bound := chplan.BindBoundedTraceScope(unbounded); bound != nil {
		t.Errorf("a plan with no top-N site must not bind, got %+v", bound)
	}
	if unbounded.ScopeBindingAlias != "" {
		t.Errorf("unbounded walk stamped: %q", unbounded.ScopeBindingAlias)
	}
	if bound := chplan.BindBoundedTraceScope(&chplan.Scan{Table: "otel_traces"}); bound != nil {
		t.Errorf("a bare scan must not bind, got %+v", bound)
	}
}

// TestBindBoundedTraceScope_GateInsideCohortSubquery pins the reach of the
// collector. The spanset-aggregate cohort hangs a whole subplan off an Expr
// slot (InSubquery.Subquery), which RewriteChildren cannot see by
// construction — a gate down there still renders a top-N, so it must be
// collected, both to be stamped and to be able to veto the bind.
func TestBindBoundedTraceScope_GateInsideCohortSubquery(t *testing.T) {
	t.Parallel()
	buried := bindGate()
	root := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.InSubquery{
			Left:     &chplan.ColumnRef{Name: "TraceId"},
			Subquery: gatedLeaf(buried),
		},
	}
	if bound := chplan.BindBoundedTraceScope(root); bound == nil {
		t.Fatal("a gate inside a cohort subquery must bind")
	}
	if buried.BindingAlias != chplan.BoundedTraceScopeAlias {
		t.Errorf("gate inside cohort subquery not stamped: %q", buried.BindingAlias)
	}

	// ... and it must be able to veto, which only holds if it is collected.
	odd := bindGate(func(g *chplan.BoundedTraceScope) { g.TraceLimit = 21 })
	vetoRoot := &chplan.Filter{
		Input: gatedLeaf(bindGate()),
		Predicate: &chplan.InSubquery{
			Left:     &chplan.ColumnRef{Name: "TraceId"},
			Subquery: gatedLeaf(odd),
		},
	}
	if bound := chplan.BindBoundedTraceScope(vetoRoot); bound != nil {
		t.Errorf("a disagreeing gate inside a cohort subquery must veto the bind, got %+v", bound)
	}
}
