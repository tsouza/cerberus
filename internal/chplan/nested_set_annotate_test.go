package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func nsAnnotate() *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              &chplan.Scan{Table: "otel_traces"},
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
	}
}

func TestNestedSetAnnotate_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !nsAnnotate().Equal(nsAnnotate()) {
		t.Fatal("identical NestedSetAnnotate nodes must be Equal")
	}
}

func TestNestedSetAnnotate_Equal_Negative(t *testing.T) {
	t.Parallel()
	base := nsAnnotate()

	mutations := map[string]func(*chplan.NestedSetAnnotate){
		"SpansTable":         func(n *chplan.NestedSetAnnotate) { n.SpansTable = "other" },
		"TraceIDColumn":      func(n *chplan.NestedSetAnnotate) { n.TraceIDColumn = "T2" },
		"SpanIDColumn":       func(n *chplan.NestedSetAnnotate) { n.SpanIDColumn = "S2" },
		"ParentSpanIDColumn": func(n *chplan.NestedSetAnnotate) { n.ParentSpanIDColumn = "P2" },
		"TimestampColumn":    func(n *chplan.NestedSetAnnotate) { n.TimestampColumn = "Ts2" },
		"Input":              func(n *chplan.NestedSetAnnotate) { n.Input = &chplan.Scan{Table: "different"} },
	}
	for field, mutate := range mutations {
		other := nsAnnotate()
		mutate(other)
		if base.Equal(other) {
			t.Errorf("NestedSetAnnotate differing in %s must not be Equal", field)
		}
	}

	if base.Equal(&chplan.Scan{Table: "otel_traces"}) {
		t.Error("NestedSetAnnotate must not equal a different node type")
	}
}

func TestNestedSetAnnotate_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	n := nsAnnotate()
	var visited []chplan.Node
	chplan.Walk(n, func(v chplan.Node) bool {
		visited = append(visited, v)
		return true
	})
	if len(visited) != 2 {
		t.Fatalf("Walk visited %d nodes, want 2 (annotate + scan)", len(visited))
	}
	if visited[0] != chplan.Node(n) || visited[1] != n.Input {
		t.Fatalf("Walk order = %T, %T; want annotate then input", visited[0], visited[1])
	}
}
