package spec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The `-- chplan --` golden section exists to pin the IR a query lowers
// to. It can only do that for what the printer renders: a field the
// printer skips is a field two plans can disagree on while printing
// byte-identical snapshots, and every rule, lowering and rewrite that
// moves such a field is unpinned by this layer.
//
// The tests below are that layer's own ratchet. They derive the roster
// of node and expression kinds from internal/chplan's source rather
// than restating it here — a hand-maintained list compared against
// another hand-maintained list proves nothing when both get edited in
// the same commit, the same reasoning TestFnGoNames_CoversEveryDeclaredFn
// already applies to fnGoNames.
//
// Two properties are ratcheted, each with a rule that admits no
// exceptions:
//
//   - Structural children. Everything a node's Children() accessor
//     returns must appear in the printed subtree. Children() is chplan's
//     own answer to "what hangs below this node", so a printer that
//     renders less than that is hiding a subtree by definition.
//
//   - Boolean fields. Every exported bool field of every node and
//     expression kind must be visible. A bool carries no value to plumb
//     through to the SQL — its only possible effect is to select one
//     emitter branch over another, so an invisible one is exactly the
//     "two plans, different SQL, identical IR" defect.
//
// Column-name and alias strings are deliberately not ratcheted, and the
// reason is checkable rather than a matter of taste: every one of the
// TXTAR fixtures carrying a `-- chplan --` section also carries a
// `-- sql --` section, so a wrong column name already fails the golden
// suite byte-for-byte at the SQL layer. Fields whose emptiness selects a
// branch rather than naming a column are printed regardless —
// FieldAccess.MaterializedColumn is the worked example, pinned by
// TestChplanPrintDistinguishesMaterializedColumn below.

// chplanNodePrototypes carries one typed nil per chplan node kind, which
// is what lets the tests below reach each kind's struct type through
// reflection. TestChplanPrintPrototypesCoverEveryNodeKind ratchets it
// against the kinds internal/chplan actually declares.
var chplanNodePrototypes = []chplan.Node{
	(*chplan.AbsentOverTime)(nil),
	(*chplan.Aggregate)(nil),
	(*chplan.CrossJoin)(nil),
	(*chplan.Filter)(nil),
	(*chplan.HistogramFloatVectorJoin)(nil),
	(*chplan.HistogramProjection)(nil),
	(*chplan.HistogramQuantile)(nil),
	(*chplan.HistogramQuantileNative)(nil),
	(*chplan.HistogramVectorJoin)(nil),
	(*chplan.InfoJoin)(nil),
	(*chplan.Limit)(nil),
	(*chplan.MetricsAggregate)(nil),
	(*chplan.MetricsCompare)(nil),
	(*chplan.MetricsHistogramOverTime)(nil),
	(*chplan.MetricsSecondStage)(nil),
	(*chplan.MixedVectorJoin)(nil),
	(*chplan.NaryVectorSetOp)(nil),
	(*chplan.NestedSetAnnotate)(nil),
	(*chplan.OneRow)(nil),
	(*chplan.OrderBy)(nil),
	(*chplan.Project)(nil),
	(*chplan.RangeBucketFanout)(nil),
	(*chplan.RangeBucketGridNative)(nil),
	(*chplan.RangeLWR)(nil),
	(*chplan.RangeWindow)(nil),
	(*chplan.RangeWindowGridNative)(nil),
	(*chplan.RangeWindowGridNativeInstant)(nil),
	(*chplan.RangeWindowGridNativeVectorAgg)(nil),
	(*chplan.RangeWindowStaleResample)(nil),
	(*chplan.Scan)(nil),
	(*chplan.SearchTraceLimit)(nil),
	(*chplan.SetOperation)(nil),
	(*chplan.StepGrid)(nil),
	(*chplan.StructuralJoin)(nil),
	(*chplan.TopK)(nil),
	(*chplan.UnionAll)(nil),
	(*chplan.VectorJoin)(nil),
	(*chplan.VectorSetOp)(nil),
}

// chplanExprPrototypes is chplanNodePrototypes' sibling for the
// expression tree, ratcheted by
// TestChplanPrintPrototypesCoverEveryExprKind.
var chplanExprPrototypes = []chplan.Expr{
	(*chplan.BareIdent)(nil),
	(*chplan.Binary)(nil),
	(*chplan.BoundedTraceScope)(nil),
	(*chplan.ColumnRef)(nil),
	(*chplan.FieldAccess)(nil),
	(*chplan.FuncCall)(nil),
	(*chplan.InList)(nil),
	(*chplan.InSubquery)(nil),
	(*chplan.InlineString)(nil),
	(*chplan.LabelJoin)(nil),
	(*chplan.LabelReplace)(nil),
	(*chplan.Lambda)(nil),
	(*chplan.LineContent)(nil),
	(*chplan.LitBool)(nil),
	(*chplan.LitFloat)(nil),
	(*chplan.LitInt)(nil),
	(*chplan.LitString)(nil),
	(*chplan.MapAccess)(nil),
	(*chplan.MapWithoutEmptyValues)(nil),
	(*chplan.MapWithoutKeys)(nil),
	(*chplan.NestedArrayExists)(nil),
	(*chplan.ScalarSubquery)(nil),
	(*chplan.Subscript)(nil),
	(*chplan.WindowExpr)(nil),
}

var (
	chplanNodeInterface = reflect.TypeOf((*chplan.Node)(nil)).Elem()
	chplanExprInterface = reflect.TypeOf((*chplan.Expr)(nil)).Elem()
)

// declaredMarkerImplementors parses internal/chplan and returns the name
// of every type declaring the given marker method — `planNode` for the
// node interface, `exprNode` for the expression one. Deriving the roster
// from the source is what makes the prototype lists above a ratchet
// rather than a second hand-maintained list.
func declaredMarkerImplementors(t *testing.T, marker string) map[string]bool {
	t.Helper()

	chplanDir := filepath.Join("..", "..", "internal", "chplan")
	entries, err := os.ReadDir(chplanDir)
	if err != nil {
		t.Fatalf("read %s: %v", chplanDir, err)
	}

	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(chplanDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != marker || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			id, ok := star.X.(*ast.Ident)
			if !ok {
				continue
			}
			out[id.Name] = true
		}
	}
	return out
}

func prototypeNames(protos []any) map[string]bool {
	out := make(map[string]bool, len(protos))
	for _, p := range protos {
		out[reflect.TypeOf(p).Elem().Name()] = true
	}
	return out
}

func nodePrototypesAsAny() []any {
	out := make([]any, 0, len(chplanNodePrototypes))
	for _, n := range chplanNodePrototypes {
		out = append(out, n)
	}
	return out
}

func exprPrototypesAsAny() []any {
	out := make([]any, 0, len(chplanExprPrototypes))
	for _, e := range chplanExprPrototypes {
		out = append(out, e)
	}
	return out
}

func assertPrototypesMatchSource(t *testing.T, marker string, protos []any) {
	t.Helper()

	declared := declaredMarkerImplementors(t, marker)
	if len(declared) == 0 {
		t.Fatalf("scanned internal/chplan and found no type declaring %s() — "+
			"the scan lost its grip on the source shape, so this ratchet is vacuous", marker)
	}

	have := prototypeNames(protos)
	for name := range declared {
		if !have[name] {
			t.Errorf("chplan.%s declares %s() but has no prototype entry — add one, or the "+
				"printer-visibility ratchets below never look at it and its fields can go "+
				"missing from every `-- chplan --` golden unnoticed", name, marker)
		}
	}
	for name := range have {
		if !declared[name] {
			t.Errorf("prototype list carries chplan.%s, which no longer declares %s() — drop it",
				name, marker)
		}
	}
}

func TestChplanPrintPrototypesCoverEveryNodeKind(t *testing.T) {
	t.Parallel()
	assertPrototypesMatchSource(t, "planNode", nodePrototypesAsAny())
}

func TestChplanPrintPrototypesCoverEveryExprKind(t *testing.T) {
	t.Parallel()
	assertPrototypesMatchSource(t, "exprNode", exprPrototypesAsAny())
}

// TestChplanPrintRendersEveryStructuralChild fails when a node kind's
// printer arm reaches fewer children than the node's own Children()
// accessor reports.
//
// It builds one instance per kind with every chplan.Node-typed field
// pointed at a uniquely named sentinel Scan, then asserts each sentinel
// that Children() returns also shows up in the printed text. That is how
// RangeWindow's DeltaPrefixAggregateInput and DownsampleTierInput and
// MetricsCompare's RootLookup were found: all three were real structural
// children the printer walked straight past.
func TestChplanPrintRendersEveryStructuralChild(t *testing.T) {
	t.Parallel()

	for _, proto := range chplanNodePrototypes {
		st := reflect.TypeOf(proto).Elem()
		t.Run(st.Name(), func(t *testing.T) {
			t.Parallel()

			node, sentinels := nodeWithSentinelChildren(st)
			if len(sentinels) == 0 {
				return
			}
			printed := PrintChplan(node)
			for _, child := range node.Children() {
				scan, ok := child.(*chplan.Scan)
				if !ok {
					continue
				}
				if !strings.Contains(printed, scan.Table) {
					t.Errorf("%s.%s is a structural child (Children() returns it) but the printer "+
						"never renders it, so a golden cannot see anything in that subtree.\n"+
						"printed:\n%s", st.Name(), sentinels[scan.Table], printed)
				}
			}
		})
	}
}

// nodeWithSentinelChildren returns a node of type st with every exported
// chplan.Node-typed field (scalar or slice-of-Node) set to a Scan whose
// table name identifies the field it came from, plus the table-name →
// field-name map.
func nodeWithSentinelChildren(st reflect.Type) (chplan.Node, map[string]string) {
	v := reflect.New(st)
	sentinels := map[string]string{}
	for i := range st.NumField() {
		f := st.Field(i)
		if f.PkgPath != "" {
			continue
		}
		table := fmt.Sprintf("sentinel_%s_%s", st.Name(), f.Name)
		switch {
		case f.Type == chplanNodeInterface:
			v.Elem().Field(i).Set(reflect.ValueOf(chplan.Node(&chplan.Scan{Table: table})))
		case f.Type.Kind() == reflect.Slice && f.Type.Elem() == chplanNodeInterface:
			slice := reflect.MakeSlice(f.Type, 1, 1)
			slice.Index(0).Set(reflect.ValueOf(chplan.Node(&chplan.Scan{Table: table})))
			v.Elem().Field(i).Set(slice)
		default:
			continue
		}
		sentinels[table] = f.Name
	}
	return v.Interface().(chplan.Node), sentinels
}

// TestChplanPrintDistinguishesEveryNodeFlag fails when flipping an
// exported bool field of a node kind leaves the printed IR unchanged.
//
// A bool field in chplan holds no value the SQL can carry — it exists to
// pick one emitter branch over another — so one the printer cannot see
// is precisely the defect this file guards: two plans that emit
// different SQL printing the same snapshot.
func TestChplanPrintDistinguishesEveryNodeFlag(t *testing.T) {
	t.Parallel()
	assertFlagsVisible(t, nodePrototypesAsAny(), func(v any) string {
		return PrintChplan(v.(chplan.Node))
	})
}

// TestChplanPrintDistinguishesEveryExprFlag is the expression-tree half
// of TestChplanPrintDistinguishesEveryNodeFlag.
func TestChplanPrintDistinguishesEveryExprFlag(t *testing.T) {
	t.Parallel()
	assertFlagsVisible(t, exprPrototypesAsAny(), func(v any) string {
		return printExpr(v.(chplan.Expr))
	})
}

func assertFlagsVisible(t *testing.T, protos []any, render func(any) string) {
	t.Helper()

	checked := 0
	for _, proto := range protos {
		st := reflect.TypeOf(proto).Elem()
		for i := range st.NumField() {
			f := st.Field(i)
			if f.PkgPath != "" || f.Type.Kind() != reflect.Bool {
				continue
			}
			checked++
			cleared := populatedInstance(st)
			set := populatedInstance(st)
			set.Elem().Field(i).SetBool(true)
			if render(cleared.Interface()) == render(set.Interface()) {
				t.Errorf("chplan.%s.%s is invisible to the printer: setting it leaves the "+
					"snapshot byte-identical, so the `-- chplan --` golden cannot tell the two "+
					"plans apart even though the emitter renders different SQL for them",
					st.Name(), f.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("walked every prototype and found no exported bool field to flip — " +
			"the reflection lost its grip on the struct shapes, so this ratchet is vacuous")
	}
}

// populatedInstance returns a new struct of type st with its exported
// string, chplan.Node and chplan.Expr fields filled in.
//
// The flag ratchet flips bools against this rather than against a
// zero-valued struct because some flags qualify a companion field and
// are meaningless without it — FieldAccess.MaterializedColumnNumeric
// says how MaterializedColumn is read, and chplan itself documents it as
// carrying no meaning when that column is empty
// (internal/chplan/field_access.go). Flipping such a flag on an empty
// struct would demand the printer render a mode that no real plan can be
// in. Populating the companions first asks the honest question instead:
// given a plan where the flag means something, can the golden see it?
func populatedInstance(st reflect.Type) reflect.Value {
	v := reflect.New(st)
	for i := range st.NumField() {
		f := st.Field(i)
		if f.PkgPath != "" {
			continue
		}
		marker := fmt.Sprintf("populated_%s_%s", st.Name(), f.Name)
		switch {
		case f.Type.Kind() == reflect.String:
			v.Elem().Field(i).SetString(marker)
		case f.Type == chplanNodeInterface:
			v.Elem().Field(i).Set(reflect.ValueOf(chplan.Node(&chplan.Scan{Table: marker})))
		case f.Type == chplanExprInterface:
			v.Elem().Field(i).Set(reflect.ValueOf(chplan.Expr(&chplan.ColumnRef{Name: marker})))
		}
	}
	return v
}

// TestChplanPrintDistinguishesMaterializedColumn pins the one
// emission-selecting field that is a string rather than a bool, and so
// falls outside the flag ratchet above: FieldAccess.MaterializedColumn
// diverts the emitter off JSON-path extraction and onto a plain column
// read when it is non-empty.
func TestChplanPrintDistinguishesMaterializedColumn(t *testing.T) {
	t.Parallel()

	source := &chplan.ColumnRef{Name: "Attributes"}
	plain := printExpr(&chplan.FieldAccess{Source: source, Path: "http.status"})
	materialized := printExpr(&chplan.FieldAccess{
		Source:             source,
		Path:               "http.status",
		MaterializedColumn: "HttpStatus",
	})
	numeric := printExpr(&chplan.FieldAccess{
		Source:                    source,
		Path:                      "http.status",
		MaterializedColumn:        "HttpStatus",
		MaterializedColumnNumeric: true,
	})

	if plain == materialized {
		t.Errorf("a materialized FieldAccess prints the same as an unmaterialized one (%q) — "+
			"the golden cannot see which emitter path the plan selected", plain)
	}
	if materialized == numeric {
		t.Errorf("MaterializedColumnNumeric is invisible: both spellings print %q", materialized)
	}
}
