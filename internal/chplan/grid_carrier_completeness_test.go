package chplan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// gridCarrierRegistry is the declared set of grid-bearing plan nodes, one
// instance per kind. Its element type is [GridCarrier], so a node listed here
// that does not implement the interface fails to COMPILE — the registry cannot
// drift into claiming a non-carrier.
//
// The completeness test below proves the other direction: that no struct in
// this package declares an eval grid without appearing here. Together the two
// make the carrier set closed in both directions, which is what lets consumers
// dispatch on the interface instead of enumerating concrete kinds.
var gridCarrierRegistry = []GridCarrier{
	&StepGrid{},
	&RangeWindow{},
	&RangeWindowNative{},
	&RangeWindowResample{},
	&RangeLWR{},
	&RangeBucketFanout{},
	&AbsentOverTime{},
}

// gridCarrierSetter is the test-only mirror of GridCarrier: it lets the
// read-back test below write a carrier's grid fields through a typed method
// instead of reflect.FieldByName (forbidden project-wide, see CLAUDE.md).
// Implemented as unexported methods below, defined in this _test.go file —
// they compile into the test binary only and add no production API surface.
type gridCarrierSetter interface {
	GridCarrier
	setEvalGrid(start, end time.Time, step time.Duration)
}

func (n *StepGrid) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *RangeWindow) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *RangeWindowNative) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *RangeWindowResample) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *RangeLWR) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *RangeBucketFanout) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

func (n *AbsentOverTime) setEvalGrid(start, end time.Time, step time.Duration) {
	n.Start, n.End, n.Step = start, end, step
}

// gridCarrierSetterFactories pairs gridCarrierRegistry with a fresh-instance
// constructor per carrier — a func, not a shared pointer, so each parallel
// subtest below mutates its own instance rather than racing on a package-level
// value. A carrier added here without a matching setEvalGrid method fails to
// compile — the same closed-both-directions property the registry already has
// for GridCarrier itself.
var gridCarrierSetterFactories = []func() gridCarrierSetter{
	func() gridCarrierSetter { return &StepGrid{} },
	func() gridCarrierSetter { return &RangeWindow{} },
	func() gridCarrierSetter { return &RangeWindowNative{} },
	func() gridCarrierSetter { return &RangeWindowResample{} },
	func() gridCarrierSetter { return &RangeLWR{} },
	func() gridCarrierSetter { return &RangeBucketFanout{} },
	func() gridCarrierSetter { return &AbsentOverTime{} },
}

// gridFieldStart / gridFieldEnd / gridFieldStep name the three fields whose
// joint presence on a plan-node struct IS the definition of "this node owns an
// eval grid". The completeness scan below recognises a carrier by exactly this
// signature, so the definition lives in one place rather than in a hand-kept
// list.
const (
	gridFieldStart = "Start"
	gridFieldEnd   = "End"
	gridFieldStep  = "Step"
)

// TestGridCarrier_Completeness is the class-level ratchet for the eval-grid
// contract. It parses THIS package's own source, collects every struct type
// declaring the (Start time.Time, End time.Time, Step time.Duration)
// signature, and asserts that set is exactly the set of kinds in
// gridCarrierRegistry.
//
// A new plan node that materialises an eval grid therefore cannot be added
// without also implementing GridCarrier: the scan finds its fields, the
// registry does not list it, and this test fails. That is the whole point —
// every consumer of the outer grid dispatches on GridCarrier, so a node that
// skips the interface would be invisible to all of them at once and its range
// queries would be reported as having no grid at all.
//
// There is no allow-list and no exemption mechanism: a struct either declares
// the grid signature or it does not.
func TestGridCarrier_Completeness(t *testing.T) {
	t.Parallel()

	declared := gridDeclaringStructsInPackage(t)
	registered := make(map[string]bool, len(gridCarrierRegistry))
	for _, c := range gridCarrierRegistry {
		registered[reflect.TypeOf(c).Elem().Name()] = true
	}

	for _, name := range declared {
		if !registered[name] {
			t.Errorf("plan node %s declares an eval grid (%s/%s/%s) but is not in gridCarrierRegistry — "+
				"implement GridCarrier on it and add it to the registry, or every consumer of the outer "+
				"grid will silently read a zero grid from plans rooted on it",
				name, gridFieldStart, gridFieldEnd, gridFieldStep)
		}
		delete(registered, name)
	}
	for name := range registered {
		t.Errorf("gridCarrierRegistry lists %s, but that type no longer declares the %s/%s/%s eval grid — "+
			"drop it from the registry", name, gridFieldStart, gridFieldEnd, gridFieldStep)
	}
}

// TestGridCarrier_EvalGridReadsBackFields pins that every registered carrier's
// EvalGrid returns its OWN grid fields verbatim, so a consumer reading through
// the interface sees exactly what the emitter reads off the struct. Without
// this, a carrier could satisfy the interface with a stub and stay invisible to
// the completeness ratchet above (which only checks the type set).
func TestGridCarrier_EvalGridReadsBackFields(t *testing.T) {
	t.Parallel()

	// Distinct, non-zero values so a swapped Start/End or a hard-coded return
	// cannot pass.
	var (
		wantStart = time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
		wantEnd   = wantStart.Add(time.Hour)
		wantStep  = 45 * time.Second
	)

	for _, newCarrier := range gridCarrierSetterFactories {
		c := newCarrier()
		name := reflect.TypeOf(c).Elem().Name()
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Fresh instance per subtest (from the factory), grid fields set
			// through the typed setter — so this stays correct for carriers
			// added later without editing here, with no reflect.FieldByName.
			c.setEvalGrid(wantStart, wantEnd, wantStep)

			gotStart, gotEnd, gotStep := c.EvalGrid()
			if !gotStart.Equal(wantStart) {
				t.Errorf("EvalGrid start = %s, want %s", gotStart, wantStart)
			}
			if !gotEnd.Equal(wantEnd) {
				t.Errorf("EvalGrid end = %s, want %s", gotEnd, wantEnd)
			}
			if gotStep != wantStep {
				t.Errorf("EvalGrid step = %s, want %s", gotStep, wantStep)
			}
		})
	}
}

// gridDeclaringStructsInPackage parses the non-test sources of this package and
// returns the sorted names of every struct type declaring the full eval-grid
// field signature. Walks the directory + parses each file directly rather than
// go/parser.ParseDir (deprecated since Go 1.25): this scan only needs type
// declarations from this package's own directory, none of the build-tag-aware
// package resolution ParseDir's replacement (golang.org/x/tools/go/packages)
// exists for.
func gridDeclaringStructsInPackage(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if structDeclaresEvalGrid(st) {
				names = append(names, ts.Name.Name)
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

// structDeclaresEvalGrid reports whether st declares Start and End as
// time.Time and Step as time.Duration. Grouped declarations
// (`Start, End time.Time`) and separate ones are both recognised.
func structDeclaresEvalGrid(st *ast.StructType) bool {
	types := map[string]string{}
	for _, f := range st.Fields.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			continue
		}
		qualified := pkgIdent.Name + "." + sel.Sel.Name
		for _, name := range f.Names {
			types[name.Name] = qualified
		}
	}
	return types[gridFieldStart] == "time.Time" &&
		types[gridFieldEnd] == "time.Time" &&
		types[gridFieldStep] == "time.Duration"
}
