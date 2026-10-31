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
