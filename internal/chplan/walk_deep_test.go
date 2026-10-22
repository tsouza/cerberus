package chplan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	exprInterfaceType = reflect.TypeFor[Expr]()
	nodeInterfaceType = reflect.TypeFor[Node]()
)

// maxExprSlotDepth bounds the reflective descent into a Node's fields. The
// deepest real slot today is two levels (Aggregate.AggFuncs[].Args[]); the
// bound turns a future self-referential struct type into a loud failure
// instead of a hung test.
const maxExprSlotDepth = 8

// typeCarriesExpr reports whether a value of type t can hold an Expr —
// directly, or nested inside a struct / slice / array / map / pointer.
//
// The descent stops at a Node-typed field, interface or concrete. Whether
// that field's Expr slots are the enclosing node's to report depends on
// whether Children() returns it, which only a built value can answer:
// [plantNodeFields] resolves it per field.
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
		for field := range t.Fields() {
			if typeCarriesExpr(field.Type, seen) {
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

// nodeField is one Node-typed struct field plantNodeFields populated: the
// value it planted there, the Expr sentinels planted inside that value, and
// the Node-typed fields nested in it.
type nodeField struct {
	path   string
	node   Node
	iface  bool
	slots  []plantedSlot
	nested []nodeField
}

// plantNodeFields populates every direct Node-typed field of the struct v.
// An interface field gets a fresh Expr-free node; a concrete node field gets
// a fresh value with its own Expr slots and Node fields planted in turn.
//
// A Node field is a child exactly when Children() returns it, and a child's
// Expr slots are the child's to report. A concrete node field Children() does
// NOT return — an embedded node, such as [HistogramQuantiles].Histogram — is
// part of the enclosing node, so its Expr slots are the enclosing node's.
// [resolveNodeFields] applies that rule once the node is built.
func plantNodeFields(t *testing.T, v reflect.Value, path string, depth int) []nodeField {
	t.Helper()
	if depth > maxExprSlotDepth {
		t.Fatalf("%s: Node field nesting exceeded %d levels — a self-referential field type?", path, maxExprSlotDepth)
	}
	var out []nodeField
	vt := v.Type()
	for i := range vt.NumField() {
		f := vt.Field(i)
		if !f.Type.Implements(nodeInterfaceType) {
			continue
		}
		fpath := path + "." + f.Name
		nf := nodeField{path: fpath}
		switch f.Type.Kind() {
		case reflect.Interface:
			nf.node, nf.iface = &OneRow{}, true
			v.Field(i).Set(reflect.ValueOf(nf.node))
		case reflect.Pointer:
			v.Field(i).Set(reflect.New(f.Type.Elem()))
			plantExprSlots(t, v.Field(i).Elem(), fpath, &nf.slots, depth+1)
			nf.nested = plantNodeFields(t, v.Field(i).Elem(), fpath, depth+1)
			nf.node, _ = v.Field(i).Interface().(Node)
		default:
			t.Fatalf("%s is a Node field of unsupported kind %s — teach the planter to build it", fpath, f.Type.Kind())
		}
		out = append(out, nf)
	}
	return out
}

// resolveNodeFields applies plantNodeFields' rule to the built node: a field
// Children() returns is left to its own row of the test; a concrete one it
// does not return contributes its Expr slots to planted; an interface one it
// does not return is a plan subtree nothing reaches, and fails the test.
func resolveNodeFields(t *testing.T, fields []nodeField, children map[Node]bool, planted *[]plantedSlot) {
	t.Helper()
	for _, f := range fields {
		if children[f.node] {
			continue
		}
		if f.iface {
			t.Errorf("%s is a Node-typed field Children() does not return, so the subtree it holds "+
				"is reachable neither as a child nor through nodeExprs", f.path)
			continue
		}
		*planted = append(*planted, f.slots...)
		resolveNodeFields(t, f.nested, children, planted)
	}
}

// plantNodeCase builds a zero value of c's node type with every Expr slot the
// node owns planted — its own fields' and those of every embedded node field
// Children() does not return — and reports the planted sentinels.
func plantNodeCase(t *testing.T, c nodeExhaustivenessCase) (Node, []plantedSlot) {
	t.Helper()
	ptr := reflect.New(reflect.TypeOf(c.node).Elem())
	var planted []plantedSlot
	plantExprSlots(t, ptr.Elem(), c.name, &planted, 0)
	fields := plantNodeFields(t, ptr.Elem(), c.name, 0)

	node, ok := ptr.Interface().(Node)
	if !ok {
		t.Fatalf("%s: reflect-built value does not implement Node", c.name)
	}
	children := map[Node]bool{}
	for _, ch := range node.Children() {
		children[ch] = true
	}
	resolveNodeFields(t, fields, children, &planted)
	return node, planted
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

			node, planted := plantNodeCase(t, c)

			reported := map[Expr]bool{}
			nodeExprs(node, func(e Expr) { reported[e] = true })

			for _, p := range planted {
				if !reported[p.expr] {
					t.Errorf("nodeExprs did not report the Expr at %s — add it to the arm for %s in "+
						"walk_deep.go, or every plan subtree nested in that slot stays invisible to WalkDeep",
						p.path, c.name)
				}
			}
			if len(reported) > len(planted) {
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
	withSlots := map[string]bool{}
	for _, c := range allNodeCases() {
		elem := reflect.TypeOf(c.node).Elem()
		if _, planted := plantNodeCase(t, c); len(planted) > 0 {
			// Keyed by the CONCRETE TYPE name, which is what the other
			// side of this comparison reads out of the switch. The
			// table's row name is a label and nothing pins the two
			// together, so keying on it would let a descriptive row name
			// invent a two-sided failure.
			withSlots[elem.Name()] = true
		}
	}
	// Both sides of this comparison are derived, and they are derived
	// from different things — that is what makes it an assertion rather
	// than a restatement. The left side is reflection over the struct
	// fields: which Node types actually HAVE an Expr-typed slot. The
	// right side is nodeExprs' own dispatch, read out of walk_deep.go:
	// which Node types it treats as having one. A pinned number in the
	// middle could only ever agree with whichever of the two its author
	// last looked at.
	//
	// The failure this catches is a Node type that gains an Expr field
	// while staying in nodeExprs' no-op arm, which leaves every plan
	// subtree nested in that slot invisible to WalkDeep.
	handled := nodeExprsHandledTypes(t)
	for name := range handled {
		if !withSlots[name] {
			t.Errorf("nodeExprs has an Expr-visiting arm for %s, but reflection finds no "+
				"Expr-typed field on it — the arm is dead, drop it from walk_deep.go", name)
		}
	}
	for name := range withSlots {
		if !handled[name] {
			t.Errorf("%s has an Expr-typed field but nodeExprs does not visit it — add an arm "+
				"for %s in walk_deep.go, or every plan subtree nested in that slot stays "+
				"invisible to WalkDeep", name, name)
		}
	}
}

// nodeExprsHandledTypes returns the Node type names nodeExprs dispatches
// on with a non-empty arm, read out of walk_deep.go.
//
// The empty arm is meaningful and is deliberately excluded: it is the
// explicit roster of Node types with no Expr-typed field, and listing
// them there rather than under a `default` is what makes the switch
// total. So the arms partition the Node set, and this returns the half
// that carries Exprs.
func nodeExprsHandledTypes(t *testing.T) map[string]bool {
	t.Helper()

	const scannedFile = "walk_deep.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, scannedFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", scannedFile, err)
	}

	handled := map[string]bool{}
	var arms int
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "nodeExprs" {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			cc, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}
			arms++
			if len(cc.Body) == 0 {
				return true
			}
			for _, expr := range cc.List {
				if name := nodeExprsCaseTypeName(expr); name != "" {
					handled[name] = true
				}
			}
			return true
		})
		return false
	})
	if arms == 0 {
		t.Fatalf("scanned %s and found no case arms in nodeExprs — the scan lost its grip "+
			"on the source shape, so this ratchet is vacuous", scannedFile)
	}
	return handled
}

// nodeExprsCaseTypeName returns the bare type name of one `case *T:`
// entry, or "" for a shape the scan does not recognise.
func nodeExprsCaseTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// TestWalkDeep_ReachesNodesBehindExprSlots pins the reachability contract
// WalkDeep exists for: a plan subtree whose only path from the root runs
// through an Expr slot is invisible to Walk and visible to WalkDeep.
func TestWalkDeep_ReachesNodesBehindExprSlots(t *testing.T) {
	t.Parallel()

	// The PromQL per-step scalar binding shape: the only ts-grid node in the
	// plan sits inside a ScalarSubquery hanging off a Project projection.
	buried := &RangeWindowStaleResample{Input: &Scan{Table: "otel_metrics_gauge"}}
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
		t.Error("WalkDeep did not reach the RangeWindowStaleResample inside the ScalarSubquery")
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
