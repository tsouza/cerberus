package regression

// Class-closing ratchet for the "declared duplicate of a derivable fact"
// family (#1504).
//
// The family is one mechanism: a fact that the code already determines
// gets restated by hand somewhere else — a count, a mirrored switch, a
// list of type names — and nothing re-derives the restatement, so it
// drifts. The drift is silent precisely because the restatement's
// existence implies a check that is not happening.
//
// The instances of the family that live in TESTS have all been one
// thing: a restatement of a sealed interface's implementer set. That is
// the sharpest case, because the truth is not merely derivable but ONLY
// derivable — a type joins chplan.Node by declaring `planNode()` and by
// nothing else, so the marker declarations ARE the set and any second
// statement of it is a copy by construction. The chplan Node kinds were
// restated as `31`, then `32`; the chplan Expr kinds as `23`, twice
// over, by a guard that compared the number against the length of the
// very list it was meant to police.
//
// So this test derives every sealed marker's implementer set
// (internal/sealedscan) and fails on the two shapes a copy takes:
//
//	Rule A — a test declares an integer that restates the set's
//	cardinality and compares it against a measured count. Bumping the
//	number is the entire maintenance protocol, which is why it is
//	forgotten.
//
//	Rule B — a test hand-enumerates the whole set as a composite
//	literal without its package deriving that set from source. A hand
//	list is legitimate (the tests need instances, which no scan can
//	produce), but only with a derivation behind it that diffs the list
//	against the marker scan.
//
// Neither rule compares a hand-maintained value against another
// hand-maintained value: the offending set is recomputed from the marker
// declarations on every run, and both rules name the specific offender.
// That distinction is the whole point of #1504 — a gate diffing two
// declarations is how this issue's own mirror drifted while a docstring
// asserted it could not.
//
// SCOPE, stated plainly so it is not mistaken for more than it is. Both
// rules read `_test.go` only, and match a test against the markers
// declared in its OWN directory. Matching within the directory is what
// buys the precision: a tree-wide search for the value 3 or 4 would be
// unusable. A dispatch mirrored between two PRODUCTION files — the shape
// #1504 itself recorded, closed by unifying on chplan.IsDerivedShape —
// is outside what this scan can see, and #2031 tracks it.

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

// sealedScanPkg is the import path of the one derivation engine. A test
// package that hand-enumerates a sealed set is expected to consume it
// rather than grow a private copy of the scan.
const sealedScanPkg = "github.com/tsouza/cerberus/internal/sealedscan"

// finding is one rule violation: where it is, and what to do about it.
//
// The rules RETURN findings rather than reporting them through *testing.T,
// so that the rules themselves can be driven against fixtures. A
// scan-and-assert-nothing-matches gate whose detectors have no positive
// test is the same silent no-op this file exists to prevent — it goes
// green either because the tree is clean or because the detector broke,
// and nothing distinguishes the two.
type finding struct {
	path string
	line int
	msg  string
}

// TestSealedMarkerSetsAreDerived is the ratchet over the live tree.
func TestSealedMarkerSetsAreDerived(t *testing.T) {
	t.Parallel()

	dirs := goPackageDirs(t, repoRoot)
	var markersSeen, testFilesRead int
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
			var found []finding
			for _, f := range files {
				found = append(found, restatedCardinality(f, m)...)
			}
			found = append(found, unbackedEnumerations(dir, files, m)...)
			for _, v := range found {
				t.Errorf("%s:%d: %s", v.path, v.line, v.msg)
			}
		}
	}

	// Vacuity guard on the WALK. If the marker convention is renamed or
	// the walk loses the tree, that must fail here rather than quietly
	// turn the ratchet into a no-op. The vacuity guard on the RULES is
	// TestSealedMarkerRules_FireOnTheDefectShapes: this one cannot cover
	// them, because a broken detector and a clean tree produce the same
	// silence.
	if markersSeen == 0 {
		t.Fatal("found no sealed marker interfaces in the tree — the scan lost its grip " +
			"on the source shape, so this ratchet is vacuous")
	}
	if testFilesRead == 0 {
		t.Fatal("found sealed marker interfaces but read no _test.go beside them — the " +
			"scan lost its grip on the source shape, so this ratchet is vacuous")
	}
}

// TestSealedMarkerRules_FireOnTheDefectShapes is the guard on the guard.
// Each fixture package under testdata holds the defect its rule must
// catch alongside the near-misses it must NOT catch, and the expectation
// is the exact set of flagged lines — so a rule that stops detecting,
// and a rule that starts over-detecting, both fail here.
func TestSealedMarkerRules_FireOnTheDefectShapes(t *testing.T) {
	t.Parallel()

	allFiles := func(run func(*testFile, sealedscan.Marker) []finding) func(string, []*testFile, sealedscan.Marker) []finding {
		return func(_ string, files []*testFile, m sealedscan.Marker) []finding {
			var out []finding
			for _, f := range files {
				out = append(out, run(f, m)...)
			}
			return out
		}
	}
	cases := []struct {
		dir string
		// positive says the fixture holds at least one line the rule
		// must flag. Without it a fixture whose markers were all deleted
		// would expect nothing, match a rule that detects nothing, and
		// pass — the dead-rule case this test exists to prevent.
		positive bool
		run      func(dir string, files []*testFile, m sealedscan.Marker) []finding
	}{
		// The three spellings of a restated cardinality — const, var,
		// and short variable declaration — each compared against a
		// different measurement shape.
		{dir: "restated", positive: true, run: allFiles(restatedCardinality)},
		// The same values, none of them a claim about the set: never
		// compared, compared against a scalar the code returned, and a
		// counter incremented in a different function.
		{dir: "restatednearmiss", run: allFiles(restatedCardinality)},
		// A complete enumeration with no derivation behind it, beside a
		// deliberate subset that must stay unflagged.
		{dir: "enumerated", positive: true, run: unbackedEnumerations},
		// The same complete enumeration, in a package that derives the
		// marker: the derivation carries the ratchet, so the rule
		// stands aside.
		{dir: "derived", run: unbackedEnumerations},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join("testdata", "sealedrules", tc.dir)
			markers, err := sealedscan.Markers(dir)
			if err != nil {
				t.Fatalf("scan %s: %v", dir, err)
			}
			if len(markers) != 1 {
				t.Fatalf("fixture %s declares %d sealed markers, want exactly 1 — the "+
					"fixture no longer exercises what it claims to", dir, len(markers))
			}
			files := parseTestFiles(t, dir)
			if len(files) == 0 {
				t.Fatalf("fixture %s holds no _test.go, so there is nothing to detect", dir)
			}
			gotLines := []int{}
			for _, v := range tc.run(dir, files, markers[0]) {
				gotLines = append(gotLines, v.line)
			}
			sort.Ints(gotLines)
			want := wantMarkedLines(t, dir)
			if tc.positive != (len(want) > 0) {
				t.Fatalf("fixture %s marks %d lines with %s, but positive=%v — the fixture "+
					"no longer states the case it is named for", dir, len(want), wantMarker, tc.positive)
			}
			if tc.positive != (len(want) > 0) {
				t.Fatalf("fixture %s marks %d lines with %s, but positive=%v — the fixture "+
					"no longer states the case it is named for", dir, len(want), wantMarker, tc.positive)
			}
			if !sameInts(gotLines, want) {
				t.Errorf("fixture %s: rule flagged lines %v, want %v (the lines marked %s)",
					dir, gotLines, want, wantMarker)
			}
		})
	}
}

// wantMarker is the comment a fixture line carries when the rule under
// test must flag it. Reading the expectation out of the fixture keeps
// the two from drifting: a new fixture case is one edit, in one file,
// and there is no list of line numbers to keep in step — which is the
// mistake this whole file exists to prevent.
const wantMarker = "// WANT"

// wantMarkedLines returns the sorted fixture lines marked with
// wantMarker. Whether a fixture is expected to carry any is the caller's
// business — the negative fixtures deliberately carry none.
func wantMarkedLines(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	lines := []int{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, group := range file.Comments {
			for _, c := range group.List {
				if strings.TrimSpace(c.Text) == wantMarker {
					lines = append(lines, fset.Position(c.Pos()).Line)
				}
			}
		}
	}
	sort.Ints(lines)
	return lines
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// restatedCardinality implements Rule A: an integer declaration whose
// value restates |implementers| and which is compared against a measured
// count.
//
// Both halves are required. The value alone is a coincidence — a lexer
// test wanting offset 6 is not a claim about the six LabelFilterer kinds
// — and the comparison alone is ordinary assertion. Together they are a
// declaration that the set has N members, checked against a measurement
// of the same set.
func restatedCardinality(f *testFile, m sealedscan.Marker) []finding {
	want := len(m.Implementers)
	measured := measuredCountOperands(f.file)
	var out []finding
	for name, decl := range intDeclarations(f.file) {
		if !measured[name] || !restatesCount(decl.value, want) {
			continue
		}
		out = append(out, finding{
			path: f.path,
			line: f.fset.Position(decl.pos).Line,
			msg: name + " restates the number of " + m.Method + "() implementers (" +
				strconv.Itoa(want) + ") and is compared against a measured count; derive " +
				"the set with " + sealedScanPkg + " instead of declaring its size — see " +
				"the interface " + m.Interface,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// intDecl is one integer-valued declaration: where it is and what it says.
type intDecl struct {
	pos   token.Pos
	value ast.Expr
}

// intDeclarations returns every name bound to an integer-constant
// expression in the file, whether by `const`, by `var`, or by a short
// variable declaration. All three spell the same restatement, and inside
// a test function the short form is the one an author reaches for.
func intDeclarations(file *ast.File) map[string]intDecl {
	out := map[string]intDecl{}
	record := func(name *ast.Ident, value ast.Expr, pos token.Pos) {
		if name.Name == "_" || !isIntConstExpr(value) {
			return
		}
		out[name.Name] = intDecl{pos: pos, value: value}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) {
					record(name, v.Values[i], v.Pos())
				}
			}
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE || len(v.Lhs) != len(v.Rhs) {
				return true
			}
			for i, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					record(id, v.Rhs[i], v.Pos())
				}
			}
		}
		return true
	})
	return out
}

// isIntConstExpr reports whether e is built only from integer literals
// and arithmetic — the shape a restated cardinality takes, including the
// `32 - 10` form that states a set's size while subtracting from it.
func isIntConstExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.INT
	case *ast.ParenExpr:
		return isIntConstExpr(v.X)
	case *ast.BinaryExpr:
		return isIntConstExpr(v.X) && isIntConstExpr(v.Y)
	}
	return false
}

// unbackedEnumerations implements Rule B: a composite literal that names
// every implementer of a marker, in a test package that does not derive
// that marker's set from source.
//
// Completeness is what identifies the literal as an enumeration of the
// set rather than a deliberate subset — the ten slice-invariant-
// registered node kinds out of thirty-two are a policy choice, not a
// mirror of the Node set. Once a package derives the marker its lists
// are diffed by that derivation on every run, so this rule steps aside
// and the derivation carries the ratchet from then on, including for the
// case this rule cannot see: a list that has already fallen one type
// behind is no longer complete, so only the derivation can catch it.
func unbackedEnumerations(dir string, files []*testFile, m sealedscan.Marker) []finding {
	derived := map[string]bool{}
	for _, f := range files {
		if f.imports[sealedScanPkg] && f.stringLits[m.Method] {
			derived[f.pkg] = true
		}
	}
	var out []finding
	for _, f := range files {
		if derived[f.pkg] {
			continue
		}
		for _, line := range completeEnumerations(f, m) {
			out = append(out, finding{
				path: f.path,
				line: line,
				msg: "this literal names all " + strconv.Itoa(len(m.Implementers)) + " " +
					m.Method + "() implementers, but package " + strconv.Quote(f.pkg) +
					" in " + dir + " never derives that set; import " + sealedScanPkg +
					" and diff the list against sealedscan.Implementers(\".\", " +
					strconv.Quote(m.Method) + ") so a new " + m.Interface +
					" implementer fails here instead of going unnoticed",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// completeEnumerations returns the line of every composite literal in f
// that names each implementer of m at least once. It looks at the
// concrete types written inside the literal at any depth, so it sees the
// shapes the tests use interchangeably: `[]Expr{&ColumnRef{}, …}`,
// `map[reflect.Type]bool{reflect.TypeOf(&Scan{}): true, …}`, and a table
// of structs each carrying a node instance.
//
// Only the OUTERMOST covering literal is reported. A literal that covers
// the set makes every literal enclosing it cover the set too, and the
// outermost one is the whole table — which is the thing that has to gain
// a row, and so the line a reader needs to be sent to.
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
		return false
	})
	return lines
}

// measuredCountOperands returns the names compared against something
// that counts: `len(x)`, or a variable the SAME function only ever
// increments. Those are measurements of a set; an integer compared
// against one is a claim about that set's size.
//
// Counters are scoped to the function that increments them. A file-wide
// scope would let an unrelated `i++` in one function turn a plain
// expectation in another into a cardinality claim — which is how a gate
// starts failing on changes it has no business judging, and how it earns
// the weakening that follows.
func measuredCountOperands(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		counters := incrementedVars(fn)
		ast.Inspect(fn, func(n ast.Node) bool {
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
	}
	return out
}

// incrementedVars returns the names targeted by an `x++` inside fn — the
// running-total shape a reflective or scanning test uses to measure a
// set it did not build directly.
func incrementedVars(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
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

// restatesCount reports whether e mentions want. The value is matched
// anywhere in the expression, not just as the result, because `32 - 10`
// restates the thirty-two Node kinds every bit as much as a bare `32`
// does.
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

// sealedScanExtraSkips are directories goPackageDirs skips on top of the
// package-wide skippedDirs: fixture packages (this file keeps its own
// under testdata, and they hold the defects on purpose), and the agent
// harness, which in a developer checkout also holds worktrees — whole
// second copies of this repository the walk must not descend into.
var sealedScanExtraSkips = map[string]bool{"testdata": true, ".claude": true}

// goPackageDirs returns every directory under root that holds Go
// sources, sorted, skipping the trees no cerberus package lives in.
func goPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] || sealedScanExtraSkips[d.Name()] {
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
