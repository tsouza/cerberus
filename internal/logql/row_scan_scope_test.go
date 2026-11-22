package logql

import (
	"context"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestRangeAggregationReadsStructuredMetadataKeysAtScan pins that a range
// aggregation whose unwrap post-filter, or whose `by (...)` grouping key,
// falls back to the structured metadata reads that key in the scan's
// scope: nothing above the scope, the range window's own projection
// included, reads the structured-metadata map, so the map is never handed
// up whole, and the reads above read the key columns defined in the
// scope. Read above the scope, a key read reads the whole map from the
// table for every row, including the rows the filter drops in PREWHERE.
func TestRangeAggregationReadsStructuredMetadataKeysAtScan(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	for _, query := range []string{
		`sum_over_time({job="api"} | logfmt | unwrap duration(latency) | __error__ = "" [5m])`,
		`sum_over_time({job="api"} | logfmt | unwrap duration(latency) | __error__ != "" [5m])`,
		`sum by (method) (sum_over_time({job="api"} | logfmt | unwrap bytes(size) | __error__ = "" [5m]))`,
		`sum by (method) (sum_over_time({job="api"} | logfmt | unwrap duration(latency) | __error__ != "" [5m]))`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			above, scope := splitAtScanScope(t, plan)
			whole := map[string]bool{s.AttributesColumn: true}
			for _, n := range above {
				for _, e := range nodeExprs(n) {
					if chplan.ReadsAnyColumn(e, whole) {
						t.Errorf("%T above the scan's scope reads %s: %v", n, s.AttributesColumn, e)
					}
				}
			}
			var lifted []string
			for _, p := range scope {
				for _, projection := range p.Projections {
					if m, ok := projection.Expr.(*chplan.MapAccess); ok && projection.Alias != "" {
						if ref, isColumn := m.Map.(*chplan.ColumnRef); isColumn && ref.Name == s.AttributesColumn {
							lifted = append(lifted, projection.Alias)
						}
					}
				}
			}
			if len(lifted) == 0 {
				t.Fatalf("the scan's scope defines no %s key read", s.AttributesColumn)
			}
			readsLifted := false
			names := map[string]bool{}
			for _, alias := range lifted {
				names[alias] = true
			}
			for _, n := range above {
				for _, e := range nodeExprs(n) {
					readsLifted = readsLifted || chplan.ReadsAnyColumn(e, names)
				}
			}
			if !readsLifted {
				t.Errorf("nothing above the scan's scope reads the key columns %v the scope defines", lifted)
			}
		})
	}
}

// TestLiftKeysLeavesMapsReadWhole pins that [rowLayers.liftKeys] leaves a
// key read alone when the stage reads its map as a whole, since the scan
// reads that map whole anyway, and lifts each other map's key once however
// many times the stack and the consumer read it.
func TestLiftKeysLeavesMapsReadWhole(t *testing.T) {
	t.Parallel()

	key := func(m, k string) chplan.Expr {
		return &chplan.MapAccess{Map: &chplan.ColumnRef{Name: m}, Key: &chplan.LitString{V: k}}
	}
	eq := func(left chplan.Expr, v string) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: left, Right: &chplan.LitString{V: v}}
	}
	layer := &chplan.Project{Projections: []chplan.Projection{{
		Expr:  &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{key("A", "k"), key("W", "k")}},
		Alias: "_value",
	}}}
	conjunct := &chplan.Binary{Op: chplan.OpAnd, Left: eq(key("A", "k"), "x"), Right: eq(key("A", "j"), "y")}
	above := []scanRest{{at: 0, layer: layer}, {at: 1, conjuncts: []chplan.Expr{conjunct}}}

	consumer := []chplan.Projection{{Expr: &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{key("A", "c"), key("A", "k")}}, Alias: "out"}}

	r := &rowLayers{}
	lifted := r.liftKeys(stageMaps{maps: []string{"A", "W"}, whole: []string{"W"}}, above, consumer)

	var got []string
	for _, p := range lifted {
		got = append(got, p.Projections[0].Alias+"="+p.Projections[0].Expr.(*chplan.MapAccess).Key.(*chplan.LitString).V)
	}
	if want := []string{"_logql_scan_key_0=k", "_logql_scan_key_1=j", "_logql_scan_key_2=c"}; !slices.Equal(got, want) {
		t.Fatalf("lifted = %v, want %v", got, want)
	}
	wantValue := &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_logql_scan_key_0"}, key("W", "k")}}
	if !layer.Projections[0].Expr.Equal(wantValue) {
		t.Errorf("layer value = %v, want %v", layer.Projections[0].Expr, wantValue)
	}
	wantConjunct := &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  eq(&chplan.ColumnRef{Name: "_logql_scan_key_0"}, "x"),
		Right: eq(&chplan.ColumnRef{Name: "_logql_scan_key_1"}, "y"),
	}
	if !above[1].conjuncts[0].Equal(wantConjunct) {
		t.Errorf("conjunct = %v, want %v", above[1].conjuncts[0], wantConjunct)
	}
	wantConsumer := &chplan.FuncCall{Fn: chplan.FnConcat, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_logql_scan_key_2"}, &chplan.ColumnRef{Name: "_logql_scan_key_0"}}}
	if !consumer[0].Expr.Equal(wantConsumer) {
		t.Errorf("consumer = %v, want %v", consumer[0].Expr, wantConsumer)
	}
	if !conjunct.Left.Equal(eq(key("A", "k"), "x")) {
		t.Errorf("liftKeys changed the conjunct it was handed in place: %v", conjunct)
	}
}

// splitAtScanScope finds plan's range window and walks its input down to
// the Project marked [chplan.Project.OneScopeRows]; it returns the nodes
// above that Project, from the range window's input down, and the
// Projects of the scope under it, down to the stream scan.
func splitAtScanScope(t *testing.T, plan chplan.Node) (above []chplan.Node, scope []*chplan.Project) {
	t.Helper()
	var rw *chplan.RangeWindow
	chplan.Walk(plan, func(n chplan.Node) bool {
		if v, ok := n.(*chplan.RangeWindow); ok && rw == nil {
			rw = v
		}
		return rw == nil
	})
	if rw == nil {
		t.Fatalf("%T holds no range window", plan)
	}
	for n := rw.Input; n != nil; n = inputOf(n) {
		if p, ok := n.(*chplan.Project); ok && p.OneScopeRows {
			for m := p.Input; m != nil; m = inputOf(m) {
				if q, ok := m.(*chplan.Project); ok {
					scope = append(scope, q)
				}
			}
			return above, scope
		}
		above = append(above, n)
	}
	t.Fatalf("no Project of %T is marked OneScopeRows", plan)
	return nil, nil
}

// nodeExprs returns the expressions a Project or a Filter reads.
func nodeExprs(n chplan.Node) []chplan.Expr {
	switch v := n.(type) {
	case *chplan.Project:
		return projectionExprs(v.Projections)
	case *chplan.Filter:
		return []chplan.Expr{v.Predicate}
	}
	return nil
}
