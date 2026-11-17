package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func traceLookupFilter() *chplan.TraceLookupFilter {
	return &chplan.TraceLookupFilter{
		Input:     &chplan.Scan{Table: "otel_traces"},
		Lookups:   []chplan.Node{&chplan.Scan{Table: "roots"}, &chplan.Scan{Table: "durations"}},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "v"}, Right: &chplan.ColumnRef{Name: "SpanName"}},
	}
}

func TestTraceLookupFilter_Equal(t *testing.T) {
	t.Parallel()
	if !traceLookupFilter().Equal(traceLookupFilter()) {
		t.Fatal("identical TraceLookupFilter nodes must be Equal")
	}
	mutations := map[string]func(*chplan.TraceLookupFilter){
		"Input":         func(n *chplan.TraceLookupFilter) { n.Input = &chplan.Scan{Table: "other"} },
		"Lookups order": func(n *chplan.TraceLookupFilter) { n.Lookups[0], n.Lookups[1] = n.Lookups[1], n.Lookups[0] },
		"Lookups count": func(n *chplan.TraceLookupFilter) { n.Lookups = n.Lookups[:1] },
		"Predicate":     func(n *chplan.TraceLookupFilter) { n.Predicate = &chplan.LitBool{V: true} },
		"Predicate nil": func(n *chplan.TraceLookupFilter) { n.Predicate = nil },
		"ExposeLookups": func(n *chplan.TraceLookupFilter) { n.ExposeLookups = true },
	}
	for field, mutate := range mutations {
		other := traceLookupFilter()
		mutate(other)
		if traceLookupFilter().Equal(other) {
			t.Errorf("TraceLookupFilter differing in %s must not be Equal", field)
		}
	}
	if traceLookupFilter().Equal(&chplan.Filter{Input: &chplan.Scan{Table: "otel_traces"}, Predicate: &chplan.LitBool{V: true}}) {
		t.Error("TraceLookupFilter must not equal a different node type")
	}
}

// TestTraceLookupFilter_Children pins the child order RewriteChildren and the
// emitter's lookup aliases (l0, l1, …) both rely on: Input first, then every
// lookup in declaration order.
func TestTraceLookupFilter_Children(t *testing.T) {
	t.Parallel()
	n := traceLookupFilter()
	children := n.Children()
	want := append([]chplan.Node{n.Input}, n.Lookups...)
	if len(children) != len(want) {
		t.Fatalf("Children() = %d nodes, want %d", len(children), len(want))
	}
	for i := range want {
		if children[i] != want[i] {
			t.Errorf("Children()[%d] = %v, want %v", i, children[i], want[i])
		}
	}
}

// TestTraceLookupFilter_RowTypeExposesLookupValues pins the output row type:
// Input's alone, and Input's followed by every lookup's value columns (never
// its key) when the node exposes them.
func TestTraceLookupFilter_RowTypeExposesLookupValues(t *testing.T) {
	t.Parallel()
	lookup := func(value string) chplan.Node {
		return &chplan.Aggregate{
			Input:          &chplan.Scan{Table: "otel_traces"},
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
			GroupByAliases: []string{chplan.TraceLookupKeyColumn},
			AggFuncs:       []chplan.AggFunc{{Fn: chplan.FnMax, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Duration"}}, Alias: value}},
		}
	}
	n := &chplan.TraceLookupFilter{
		Input:   &chplan.Scan{Table: "otel_traces", Columns: []string{"TraceId", "SpanName"}},
		Lookups: []chplan.Node{lookup("a"), lookup("b")},
	}
	names := func(s chplan.Schema) []string {
		out := make([]string, 0, len(s.Columns))
		for _, c := range s.Columns {
			out = append(out, c.Name)
		}
		return out
	}
	if got := names(n.RowType()); len(got) != 2 || got[0] != "TraceId" || got[1] != "SpanName" {
		t.Fatalf("filtering RowType = %v, want Input's [TraceId SpanName]", got)
	}
	n.ExposeLookups = true
	got := names(n.RowType())
	want := []string{"TraceId", "SpanName", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("exposing RowType = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("exposing RowType = %v, want %v", got, want)
		}
	}
}
