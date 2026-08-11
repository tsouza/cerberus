package regression

// Class-closing ratchet for the "declared duplicate of a derivable fact"
// family (#1504).
//
// The family is one mechanism: a fact that the code already determines
// gets restated by hand somewhere else — a count, a mirrored switch, a
// list of type names — and nothing re-derives the restatement, so it
// drifts. The drift is silent precisely because the restatement's
// existence implies a check that is not happening. Every member of the
// family found so far was a sealed interface's implementer set: the
// chplan Node kinds restated as `31`, then `32`; the chplan Expr kinds
// restated as `23` twice over; the derived-shape classifier mirrored
// into the HTTP layer while a docstring on each copy asserted they
// agreed.
//
// A sealed interface is the sharpest case because the truth is not
// merely derivable, it is *only* derivable: a type joins chplan.Node by
// declaring `planNode()` and by nothing else, so the marker method
// declarations are the set. Any second statement of that set is a copy.
//
// This test therefore derives every sealed marker's implementer set in
// the tree (internal/sealedscan) and fails on the two shapes a copy
// takes:
//
//	Rule A — a test declares an integer constant that restates the
//	set's cardinality and compares it against a measured count. This is
//	`const wantExprTypes = 23; if len(all) != wantExprTypes`. Bumping
//	the number is the entire maintenance protocol, which is why it is
//	forgotten.
//
//	Rule B — a test hand-enumerates the whole set as a composite
//	literal without its package deriving that set from source. A hand
//	list is legitimate (the tests need instances to exercise), but only
//	with a derivation behind it that diffs the list against the marker
//	scan; without one it is a mirror with nothing re-deriving it.
//
// Neither rule compares a hand-maintained value against another
// hand-maintained value — the offending set is recomputed from the
// marker declarations on every run, and both rules name the specific
// offender. That distinction is the whole point of #1504: a gate that
// diffs two declarations is how the mirror it was supposed to catch
// drifted in the first place.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

const (
	// sealedScanRoot is the repo root relative to this package's
	// directory, which is where `go test` runs.
	sealedScanRoot = "../.."
	// sealedScanPkg is the import path of the one derivation engine. A
	// package that hand-enumerates a sealed set is expected to consume
	// it rather than grow a private copy of the scan.
	sealedScanPkg = "github.com/tsouza/cerberus/internal/sealedscan"
)

// TestSealedMarkerSetsAreDerived is the ratchet. It fails when a test
// restates a sealed interface's implementer set instead of deriving it.
func TestSealedMarkerSetsAreDerived(t *testing.T) {
	t.Parallel()

	dirs := goPackageDirs(t, sealedScanRoot)
	var (
		markersSeen   int
		testFilesRead int
	)
	for _, dir := range dirs {
		markers, err := sealedscan.Markers(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		if len(markers) == 0 {
			continue
		}
		markersSeen += len(markers)

		files := parseTestFiles(t, dir)
		testFilesRead += len(files)
		for _, m := range markers {
			for _, f := range files {
				checkRestatedCardinality(t, f, m)
			}
			checkUnbackedEnumeration(t, dir, files, m)
		}
	}

	// Vacuity guard. Both halves of this test are "scan the tree, then
	// assert nothing matches", which is exactly the shape that passes
	// forever once the scan stops finding anything. If the marker
	// convention is renamed or the walk loses the tree, that must fail
	// here rather than quietly turn the ratchet into a no-op.
	if markersSeen == 0 {
		t.Fatal("found no sealed marker interfaces in the tree — the scan lost its grip " +
			"on the source shape, so this ratchet is vacuous")
	}
	if testFilesRead == 0 {
		t.Fatal("found sealed marker interfaces but read no _test.go beside them — the " +
			"scan lost its grip on the source shape, so this ratchet is vacuous")
	}
}

// testFile is one parsed test file plus what the rules need from it.
type testFile struct {
	path       string
	pkg        string
	fset       *token.FileSet
	file       *ast.File
	imports    map[string]bool
	stringLits map[string]bool
}

// checkRestatedCardinality implements Rule A: a named integer constant
// whose value restates |implementers| and which is compared against a
// measured count.
//
// Both halves are required. The value alone is a coincidence — a lexer
// test wanting offset 6 is not a claim about the six LabelFilterer
// kinds — and the comparison alone is ordinary assertion. Together they
// are a declaration that the set has N members, checked against a
// measurement of the same set.
func checkRestatedCardinality(t *testing.T, f *testFile, m sealedscan.Marker) {
	t.Helper()
	want := len(m.Implementers)
	measured := measuredCountOperands(f.file)
	ast.Inspect(f.file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) || !restatesCount(vs.Values[i], want) {
				continue
			}
			if !measured[name.Name] {
				continue
			}
			t.Errorf("%s:%d: %s restates the number of %s() implementers (%d) and is "+
				"compared against a measured count; derive the set with %s instead of "+
				"declaring its size — see the interface %s",
				f.path, f.fset.Position(vs.Pos()).Line, name.Name, m.Method, want,
				sealedScanPkg, m.Interface)
		}
		return true
	})
}

// checkUnbackedEnumeration implements Rule B: a composite literal that
// names every implementer of a marker, in a test package that does not
// derive that marker's set from source.
//
// Completeness is what identifies the literal as an enumeration of the
// set rather than a deliberate subset (the ten slice-invariant-
// registered node kinds out of thirty-two are a policy choice, not a
// mirror of the Node set). Once a package derives the marker, its lists
// are diffed by that derivation on every run, so this rule steps aside
// and the derivation carries the ratchet from then on — including for
// the case this rule cannot see, where the list has already fallen one
// type behind.
func checkUnbackedEnumeration(t *testing.T, dir string, files []*testFile, m sealedscan.Marker) {
	t.Helper()
	derived := map[string]bool{}
	for _, f := range files {
		if f.imports[sealedScanPkg] && f.stringLits[m.Method] {
			derived[f.pkg] = true
		}
	}
	for _, f := range files {
		if derived[f.pkg] {
			continue
		}
		for _, pos := range completeEnumerations(f, m) {
			t.Errorf("%s:%d: this literal names all %d %s() implementers, but package %q "+
				"in %s never derives that set; import %s and diff the list against "+
				"sealedscan.Implementers(\".\", %q) so a new %s implementer fails here "+
				"instead of going unnoticed",
				f.path, pos, len(m.Implementers), m.Method, f.pkg, dir,
				sealedScanPkg, m.Method, m.Interface)
		}
	}
}

// completeEnumerations returns the line of every composite literal in f
// that names each implementer of m at least once. It looks at the
// concrete types written inside the literal at any depth, so it sees the
// three shapes the tests use interchangeably: `[]Expr{&ColumnRef{}, …}`,
// `map[reflect.Type]bool{reflect.TypeOf(&Scan{}): true, …}`, and a table
// of structs each carrying a node instance.
func completeEnumerations(f *testFile, m sealedscan.Marker) []int {
	var lines []int
	ast.Inspect(f.file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		named := map[string]bool{}
		ast.Inspect(cl, func(inner ast.Node) bool {
			icl, ok := inner.(*ast.CompositeLit)
			if !ok || icl.Type == nil {
				return true
			}
			named[typeName(icl.Type)] = true
			return true
		})
		for _, impl := range m.Implementers {
			if !named[impl] {
				return true
			}
		}
		lines = append(lines, f.fset.Position(cl.Pos()).Line)
		// A literal that covers the set makes every literal enclosing
		// it cover the set too; report the innermost one only, which is
		// the list the author actually wrote.
		return false
	})
	return lines
}

// measuredCountOperands returns the names compared against something
// that counts: `len(x)`, or a variable the same function only ever
// increments. Those are measurements of a set; a constant compared
// against one is a claim about that set's size.
func measuredCountOperands(file *ast.File) map[string]bool {
	counters := incrementedVars(file)
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok || !isComparison(be.Op) {
			return true
		}
		for _, pair := range [2][2]ast.Expr{{be.X, be.Y}, {be.Y, be.X}} {
			id, ok := pair[1].(*ast.Ident)
			if !ok {
				continue
			}
			if isLenCall(pair[0]) {
				out[id.Name] = true
				continue
			}
			if other, ok := pair[0].(*ast.Ident); ok && counters[other.Name] {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// incrementedVars returns the names targeted by a `x++` anywhere in the
// file — the running-total shape a reflective or scanning test uses to
// measure a set it did not build directly.
func incrementedVars(file *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		inc, ok := n.(*ast.IncDecStmt)
		if !ok {
			return true
		}
		if id, ok := inc.X.(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// restatesCount reports whether e is an integer-constant expression that
// mentions want. The value is matched anywhere in the expression, not
// just as the result, because `32 - 10` restates the thirty-two Node
// kinds every bit as much as a bare `32` does.
func restatesCount(e ast.Expr, want int) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return true
		}
		if v, err := strconv.Atoi(lit.Value); err == nil && v == want {
			found = true
		}
		return true
	})
	return found
}

// isComparison reports whether op compares two values. Equality and
// ordering both count: `len(all) != want` and `seen > want` state the
// same claim about the same set.
func isComparison(op token.Token) bool {
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

func isLenCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return false
	}
	id, ok := c.Fun.(*ast.Ident)
	return ok && id.Name == "len"
}

func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.StarExpr:
		return typeName(v.X)
	}
	return ""
}

// parseTestFiles parses every _test.go directly in dir.
func parseTestFiles(t *testing.T, dir string) []*testFile {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*testFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		tf := &testFile{
			path:       path,
			pkg:        file.Name.Name,
			fset:       fset,
			file:       file,
			imports:    map[string]bool{},
			stringLits: map[string]bool{},
		}
		for _, imp := range file.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil {
				tf.imports[p] = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					tf.stringLits[s] = true
				}
			}
			return true
		})
		out = append(out, tf)
	}
	return out
}

// goPackageDirs returns every directory under root that holds Go
// sources, sorted, skipping the trees no package lives in.
func goPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata holds fixture packages, which are shapes under
			// test rather than code under ratchet. .claude holds the
			// agent harness, and in a developer checkout also the
			// worktrees — whole second copies of this repository, which
			// the walk must not descend into and report twice.
			case ".git", ".claude", "node_modules", "bin", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			seen[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}
