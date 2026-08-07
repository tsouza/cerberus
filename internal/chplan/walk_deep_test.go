package chplan

import (
	"fmt"
	"reflect"
	"testing"
)

// This file pins the EXHAUSTIVENESS contract of nodeExprs (see its doc
// comment in walk_deep.go) and the reachability contract of WalkDeep.
//
// nodeExprs is the single enumeration of "which Expr slots does this Node
// carry". Everything that must see a plan subtree nested inside an Expr —
// the experimental ts-grid setting gate, the subquery sample budget, the
// compare() memory bound — reads the IR through it. A Node type that grows
// an Expr-bearing field without an arm in nodeExprs silently hides every
// plan subtree inside it again, which is the production 502 this traversal
// exists to prevent.
//
// A hand-maintained field list is exactly the thing that goes stale, so the
// exhaustiveness test does not carry one: it DERIVES the Expr-bearing
// fields of every concrete Node type by reflection, plants a unique
// sentinel Expr in each, and asserts nodeExprs surfaces all of them. The
// reflection lives here and only here — the runtime path runs on every
// query and stays an explicit switch.

var (
	exprInterfaceType = reflect.TypeOf((*Expr)(nil)).Elem()
	nodeInterfaceType = reflect.TypeOf((*Node)(nil)).Elem()
)

// maxExprSlotDepth bounds the reflective descent into a Node's fields. The
// deepest real slot today is two levels (Aggregate.AggFuncs[].Args[]); the
// bound turns a future self-referential struct type into a loud failure
// instead of a hung test.
const maxExprSlotDepth = 8

// typeCarriesExpr reports whether a value of type t can hold an Expr —
// directly, or nested inside a struct / slice / array / map / pointer.
//
// The descent stops at a Node-typed field, interface or concrete: a child
// node's own Expr slots are that node's to report, and nodeExprs is
// deliberately non-recursive, so counting them here would demand a slot the
// enumeration must not surface. Children() is what reaches them.
func typeCarriesExpr(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == exprInterfaceType {
		return true
	}
	if t.Kind() != reflect.Interface && t.Implements(exprInterfaceType) {
		return true
	}
	if t.Implements(nodeInterfaceType) {
		return false
	}
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeCarriesExpr(t.Elem(), seen)
	case reflect.Map:
		return typeCarriesExpr(t.Key(), seen) || typeCarriesExpr(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if typeCarriesExpr(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}

// sentinelExpr mints a uniquely-named Expr. Identity is by pointer, so the
// assertion can tell exactly which planted slot nodeExprs failed to report.
func sentinelExpr(n int) Expr {
	return &ColumnRef{Name: fmt.Sprintf("EXPR_SLOT_SENTINEL_%d", n)}
}

// plantedSlot records one sentinel and the field path it was planted at, so
// a miss names the offending field rather than a bare pointer.
type plantedSlot struct {
	path string
	expr Expr
}

// plantExprSlots fills every Expr-bearing slot reachable from the settable
// value v with a fresh sentinel, allocating the pointers, one-element
// slices and nested structs needed to reach them. It reports each planted
// sentinel with its field path.
//
// Field iteration is by INDEX (reflect.Type.Field(i) / reflect.Value.Field(i)):
// .golangci.yml forbids reflect.Value.FieldByName, and index iteration is
// what makes the derivation total rather than name-driven.
func plantExprSlots(t *testing.T, v reflect.Value, path string, out *[]plantedSlot, depth int) {
	t.Helper()
	if depth > maxExprSlotDepth {
		t.Fatalf("%s: Expr slot nesting exceeded %d levels — a self-referential field type?", path, maxExprSlotDepth)
	}
	ft := v.Type()
	if ft == exprInterfaceType {
		e := sentinelExpr(len(*out))
		v.Set(reflect.ValueOf(e))
		*out = append(*out, plantedSlot{path: path, expr: e})
		return
	}
	if ft.Kind() != reflect.Interface && ft.Implements(exprInterfaceType) {
		t.Fatalf("%s is typed %s, a CONCRETE Expr implementation: decide how nodeExprs surfaces it "+
			"(add an arm in walk_deep.go) and teach this planter to build it", path, ft)
	}
	if !typeCarriesExpr(ft, map[reflect.Type]bool{}) {
		return
	}
	switch ft.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(ft.Elem()))
		plantExprSlots(t, v.Elem(), path, out, depth+1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(ft, 1, 1))
		plantExprSlots(t, v.Index(0), path+"[0]", out, depth+1)
	case reflect.Array:
		for i := range ft.Len() {
			plantExprSlots(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), out, depth+1)
		}
	case reflect.Struct:
		for i := range ft.NumField() {
			f := ft.Field(i)
			if !f.IsExported() {
				t.Fatalf("%s.%s is an unexported Expr-bearing field; nodeExprs cannot be derived for it", path, f.Name)
			}
			plantExprSlots(t, v.Field(i), path+"."+f.Name, out, depth+1)
		}
	default:
		t.Fatalf("%s is an Expr-bearing field of unsupported kind %s — teach the planter to build it", path, ft.Kind())
	}
}

// TestNodeExprs_Exhaustive is the staleness guard. For every concrete Node
// type it derives the Expr-bearing fields by reflection, plants a sentinel
// in each, and asserts nodeExprs reports every one. A Node type that grows
// an Expr-bearing field without an arm in nodeExprs fails here naming the
// exact field path.
func TestNodeExprs_Exhaustive(t *testing.T) {
	t.Parallel()
	for _, c := range allNodeCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			ptr := reflect.New(reflect.TypeOf(c.node).Elem())
			var planted []plantedSlot
			plantExprSlots(t, ptr.Elem(), c.name, &planted, 0)

			node, ok := ptr.Interface().(Node)
			if !ok {
				t.Fatalf("%s: reflect-built value does not implement Node", c.name)
			}

			reported := map[Expr]bool{}
			nodeExprs(node, func(e Expr) { reported[e] = true })

			for _, p := range planted {
				if !reported[p.expr] {
					t.Errorf("nodeExprs did not report the Expr at %s — add it to the arm for %s in "+
						"walk_deep.go, or every plan subtree nested in that slot stays invisible to WalkDeep",
						p.path, c.name)
				}
			}
			if len(reported) != len(planted) {
				t.Errorf("%s: nodeExprs reported %d Exprs, %d were planted — it surfaced an Expr the "+
					"reflective derivation did not plant", c.name, len(reported), len(planted))
			}
		})
	}
}

// TestNodeExprs_FindsEveryNodeTypeWithExprSlots is the other half of the
// guard: it proves the reflective derivation actually plants something, so
// TestNodeExprs_Exhaustive cannot pass vacuously by planting nothing.
func TestNodeExprs_FindsEveryNodeTypeWithExprSlots(t *testing.T) {
	t.Parallel()
	withSlots := 0
	for _, c := range allNodeCases() {
		ptr := reflect.New(reflect.TypeOf(c.node).Elem())
		var planted []plantedSlot
		plantExprSlots(t, ptr.Elem(), c.name, &planted, 0)
		if len(planted) > 0 {
			withSlots++
		}
	}
	// Filter, Project, Aggregate, RangeWindow, RangeWindowNative, OrderBy,
	// TopK, RangeBucketFanout, HistogramQuantile, HistogramQuantileNative,
	// MetricsAggregate, MetricsHistogramOverTime, MetricsCompare.
	const wantNodeTypesWithExprSlots = 13
	if withSlots != wantNodeTypesWithExprSlots {
		t.Fatalf("reflection found Expr slots on %d Node types, expected %d — the IR gained or lost an "+
			"Expr-bearing Node type: update nodeExprs in walk_deep.go and this count",
			withSlots, wantNodeTypesWithExprSlots)
	}
}

// TestWalkDeep_ReachesNodesBehindExprSlots pins the reachability contract
// WalkDeep exists for: a plan subtree whose only path from the root runs
// through an Expr slot is invisible to Walk and visible to WalkDeep.
func TestWalkDeep_ReachesNodesBehindExprSlots(t *testing.T) {
	t.Parallel()

	// The PromQL per-step scalar binding shape: the only ts-grid node in the
	// plan sits inside a ScalarSubquery hanging off a Project projection.
	buried := &RangeWindowResample{Input: &Scan{Table: "otel_metrics_gauge"}}
	root := Node(&Project{
		Input: &Scan{Table: "otel_metrics_gauge"},
		Projections: []Projection{{
			Expr: &Binary{
				Op:    OpMul,
				Left:  &ScalarSubquery{Input: buried},
				Right: &LitInt{V: 2},
			},
			Alias: "Value",
		}},
	})

	contains := func(walk func(Node, func(Node) bool)) bool {
		found := false
		walk(root, func(n Node) bool {
			if n == Node(buried) {
				found = true
			}
			return true
		})
		return found
	}

	if contains(Walk) {
		t.Fatal("Walk reached a node behind an Expr slot; WalkDeep would then be redundant — " +
			"if Walk's contract widened, every caller that relies on the spine-only walk needs revisiting")
	}
	if !contains(WalkDeep) {
		t.Error("WalkDeep did not reach the RangeWindowResample inside the ScalarSubquery")
	}
}

// TestWalkDeep_ReachesInSubqueryPlans pins the second Expr slot that embeds
// a plan: InSubquery.Subquery, the TraceQL cohort shape.
func TestWalkDeep_ReachesInSubqueryPlans(t *testing.T) {
	t.Parallel()

	buried := &Scan{Table: "otel_traces_cohort"}
	root := Node(&Filter{
		Input: &Scan{Table: "otel_traces"},
		Predicate: &InSubquery{
			Left:     &ColumnRef{Name: "TraceId"},
			Subquery: buried,
		},
	})

	found := false
	WalkDeep(root, func(n Node) bool {
		if n == Node(buried) {
			found = true
		}
		return true
	})
	if !found {
		t.Error("WalkDeep did not reach the Scan inside the InSubquery")
	}
}

// TestWalkDeep_SkipStopsExprSubtrees pins that a visit returning false
// prunes the node's Expr-embedded subtrees as well as its children —
// otherwise a caller that stops descending on a match would still pay for
// (and observe) the scalar interiors it asked to skip.
func TestWalkDeep_SkipStopsExprSubtrees(t *testing.T) {
	t.Parallel()

	buried := &Scan{Table: "scalar_interior"}
	root := Node(&Project{
		Input:       &Scan{Table: "spine"},
		Projections: []Projection{{Expr: &ScalarSubquery{Input: buried}, Alias: "Value"}},
	})

	var visited []Node
	WalkDeep(root, func(n Node) bool {
		visited = append(visited, n)
		return false
	})
	if len(visited) != 1 || visited[0] != root {
		t.Errorf("WalkDeep visited %d nodes after the root returned false, want exactly the root", len(visited))
	}
}
