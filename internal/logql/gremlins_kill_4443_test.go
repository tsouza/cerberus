package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file kills LIVED gremlins mutants of the phase4-logql-other-b and
// phase4-logql-aggregation legs (cerberus issue #4443) that a lowered
// query's SQL cannot single out: the decisions sit in a builder or a
// guard whose effect the tests over whole statements never reach.

// TestBytesUnitCascadesAllocateTheirArgumentsOnce pins that each multiIf
// cascade the bytes() unwrap conversion builds over the unit prefixes is
// allocated once, at its final size: a condition and a branch per prefix
// letter, then the fallback branch.
func TestBytesUnitCascadesAllocateTheirArgumentsOnce(t *testing.T) {
	t.Parallel()
	rest := &chplan.ColumnRef{Name: "rest"}
	want := 2*len(bytesUnitPrefixLetters) + 1

	scaled, ok := bytesScaledExpr(&chplan.ColumnRef{Name: "num"}, rest).(*chplan.Binary)
	if !ok {
		t.Fatalf("bytesScaledExpr = %T, want *chplan.Binary", scaled)
	}
	power, ok := scaled.Right.(*chplan.FuncCall)
	if !ok || power.Fn != chplan.FnPow || len(power.Args) != 2 {
		t.Fatalf("bytesScaledExpr's multiplier = %#v, want pow(base, exponent)", scaled.Right)
	}
	multiplier, threshold := bytesExactMultiplierAndOverflowThreshold(rest)
	for _, tc := range []struct {
		name    string
		cascade chplan.Expr
	}{
		{"the scaled value's exponent", power.Args[1]},
		{"the exact multiplier", multiplier},
		{"the overflow threshold", threshold},
	} {
		call, ok := tc.cascade.(*chplan.FuncCall)
		if !ok || call.Fn != chplan.FnMultiIf {
			t.Fatalf("%s = %#v, want a multiIf", tc.name, tc.cascade)
		}
		if len(call.Args) != want || cap(call.Args) != want {
			t.Errorf("%s has %d arguments in a backing array of %d, want %d in one of %d", tc.name, len(call.Args), cap(call.Args), want, want)
		}
	}
}

// TestIsLogRowProjectionNeedsAProjectLeadingWithTheLine pins the guard
// [Lang.ProjectSamples] puts on a log query's plan: only a Project whose
// first projection is the [LogLineColumn] is the log-row projection. Any
// other node, and a Project with no projections, is not, and the guard
// answers so rather than reading a projection that is not there.
func TestIsLogRowProjectionNeedsAProjectLeadingWithTheLine(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_logs"}
	line := chplan.Projection{Expr: &chplan.ColumnRef{Name: "Body"}, Alias: LogLineColumn}
	other := chplan.Projection{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "TimeUnix"}
	for _, tc := range []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{"a Project leading with the line", &chplan.Project{Input: scan, Projections: []chplan.Projection{line, other}}, true},
		{"a Project leading with another column", &chplan.Project{Input: scan, Projections: []chplan.Projection{other, line}}, false},
		{"a Project with no projections", &chplan.Project{Input: scan}, false},
		{"a Limit", &chplan.Limit{Input: &chplan.Project{Input: scan, Projections: []chplan.Projection{line}}, Count: 1}, false},
		{"the scan", scan, false},
	} {
		if got := isLogRowProjection(tc.plan); got != tc.want {
			t.Errorf("isLogRowProjection(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRowLayersHoistWithNoFilteredValueSortsTheFilteredRows pins that when
// no filter reads a value of the stack, [rowLayers.hoist] puts the sort
// directly on the filtered rows, with no Project between them to narrow:
// every value moves above the limit, and the filtered rows are read
// columns of the table alone.
func TestRowLayersHoistWithNoFilteredValueSortsTheFilteredRows(t *testing.T) {
	t.Parallel()
	scanFilter := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Job"}, Right: &chplan.LitString{V: "api"}},
	}
	rows := newRowLayers(scanFilter)
	value := rows.define("_value", lowerOf("X"))
	layer := rows.node
	rows.filter(&chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Y"}, Right: &chplan.LitString{V: "y"}})
	order := &chplan.OrderBy{Keys: []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}}}
	limit := &chplan.Limit{Count: 100}
	rows.hoist(nil, order, limit)
	consumer := &chplan.Project{
		Input:       rows.node,
		Projections: []chplan.Projection{{Expr: value, Alias: "out"}, {Expr: &chplan.ColumnRef{Name: "Timestamp"}}},
	}
	rows.seal(consumer)

	if consumer.Input != layer {
		t.Fatalf("the consumer reads %T, want the _value layer above the limit", consumer.Input)
	}
	if layer.(*chplan.Project).Input != limit {
		t.Fatalf("the _value layer reads %T, want the limit", layer.(*chplan.Project).Input)
	}
	filtered, ok := order.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("the sort reads %T, want the filtered rows directly", order.Input)
	}
	if filtered.Input != scanFilter.Input {
		t.Errorf("the filtered rows read %T, want the stream scan", filtered.Input)
	}
}

// TestRowLayersSealAllocatesEachLayerOnce pins that [rowLayers.seal]
// builds a kept layer's projections once, at their final size: the
// columns it passes through, then its own value.
func TestRowLayersSealAllocatesEachLayerOnce(t *testing.T) {
	t.Parallel()
	rows := newRowLayers(&chplan.Scan{Table: "otel_logs"})
	a := rows.define("_a", lowerOf("X"))
	layerA := rows.node.(*chplan.Project)
	rows.filter(&chplan.Binary{Op: chplan.OpEq, Left: a, Right: &chplan.LitString{V: "x"}})
	b := rows.define("_b", lowerOf("Y"))
	layerB := rows.node.(*chplan.Project)
	consumer := &chplan.Project{
		Input: rows.node,
		Projections: []chplan.Projection{
			{Expr: b, Alias: "out"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			{Expr: &chplan.ColumnRef{Name: "Body"}},
		},
	}
	rows.seal(consumer)
	for _, layer := range []*chplan.Project{layerA, layerB} {
		if n := len(layer.Projections); n < 2 || cap(layer.Projections) != n {
			t.Errorf("layer %s projects %v in a backing array of %d, want it to pass columns through in one of exactly their number", layer.Projections[len(layer.Projections)-1].Alias, projectedNames(layer), cap(layer.Projections))
		}
	}
}

// TestScanScopeExpansionBound pins how a [scanScope] measures itself
// against [maxScanScopeExpansion]: a read of a column outside the scope
// counts as one node and a read of a scope alias as that alias's own
// expanded size; every value or conjunct added counts toward the scope's
// sizes; and the scope admits what keeps its expanded size within the
// bound, at the bound included.
func TestScanScopeExpansionBound(t *testing.T) {
	t.Parallel()
	// nest is a value of the table's column Y of size n.
	nest := func(n int) chplan.Expr {
		e := chplan.Expr(&chplan.ColumnRef{Name: "Y"})
		for range n - 1 {
			e = &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{e}}
		}
		return e
	}
	eq := func(left, right chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: left, Right: right}
	}
	scope := func(defs map[string]chplan.Expr) *scanScope {
		aliases := map[string]bool{}
		for alias := range defs {
			aliases[alias] = true
		}
		return &scanScope{aliases: aliases, defs: defs, expanded: map[string]int{}}
	}
	ref := func(name string) chplan.Expr { return &chplan.ColumnRef{Name: name} }

	t.Run("expanded size", func(t *testing.T) {
		t.Parallel()
		s := scope(map[string]chplan.Expr{"_a": nest(4)})
		// One node for the comparison, two for lower(X), four for _a.
		if got := s.expandedSize(eq(lowerOf("X"), ref("_a"))); got != 7 {
			t.Errorf("expandedSize(lower(X) = _a) = %d, want 7", got)
		}
		if got := s.expandedSize(eq(ref("X"), ref("Z"))); got != 3 {
			t.Errorf("expandedSize(X = Z) = %d, want 3: a read of a column outside the scope is one node", got)
		}
	})

	t.Run("admits up to the bound", func(t *testing.T) {
		t.Parallel()
		// Two reads of a four-node alias in a three-node comparison
		// expand to nine nodes, three times the text.
		at := scope(map[string]chplan.Expr{"_a": nest(4)})
		if !at.admits(eq(ref("_a"), ref("_a"))) {
			t.Errorf("a scope does not admit a conjunct that expands to exactly %d times its text", maxScanScopeExpansion)
		}
		past := scope(map[string]chplan.Expr{"_a": nest(5)})
		if past.admits(eq(ref("_a"), ref("_a"))) {
			t.Errorf("a scope admits a conjunct that expands past %d times its text", maxScanScopeExpansion)
		}
	})

	t.Run("every addition counts", func(t *testing.T) {
		t.Parallel()
		s := scope(map[string]chplan.Expr{"_light": nest(4), "_heavy": nest(10)})
		light := eq(ref("_light"), &chplan.LitString{V: "x"})
		heavy := eq(ref("_heavy"), ref("_heavy"))
		if !s.admits(light) {
			t.Fatal("an empty scope does not admit a conjunct that expands to twice its text")
		}
		// heavy expands to 21 nodes over a 3-node text, light to 6 over
		// 3: after both the scope is 27 over 6, and one more light one
		// would make it 33 over 9.
		s.add(heavy)
		s.add(light)
		if s.admits(light) {
			t.Errorf("after a heavy conjunct and a light one the scope admits another light one: its sizes are %d expanded over %d of text", s.expandedTotal, s.text)
		}
	})
}
