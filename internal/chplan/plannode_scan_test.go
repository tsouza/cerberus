package chplan_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// planNodeMarkerMethod is the sealing marker on chplan.Node. A type
// declaring it IS a plan node — that method declaration is the only
// definition of the Node set there is, which is why the scan below reads
// it out of the source rather than trusting any list.
const planNodeMarkerMethod = "planNode"

// planNodeImplementers parses this package's own non-test sources and
// returns the sorted names of every type declaring the `planNode()`
// sealing marker — i.e. the live, complete set of chplan.Node kinds.
//
// Every ratchet over "the Node set" derives from here. A hand-maintained
// count or list compared against another hand-maintained count or list
// proves nothing: both are edited together by the same person in the same
// commit, so neither can catch the type that was added without touching
// either. Scanning the marker method means adding a Node kind trips the
// ratchets with no number and no list to bump.
//
// Walks the directory and parses each file directly rather than
// go/parser.ParseDir (deprecated since Go 1.25); this scan needs only
// declarations from this package's own directory, which is exactly what
// ParseDir's replacement would add build-tag machinery for.
func planNodeImplementers(t *testing.T) []string {
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != planNodeMarkerMethod || fn.Recv == nil {
				return true
			}
			if recv := receiverTypeName(fn.Recv); recv != "" {
				names = append(names, recv)
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatalf("scanned the chplan sources and found no %s() declarations — "+
			"the scan lost its grip on the source shape, so every ratchet built on it is vacuous",
			planNodeMarkerMethod)
	}
	sort.Strings(names)
	return names
}

// receiverTypeName returns the bare type name of a method receiver,
// unwrapping the pointer every plan node's marker uses (`func (*Scan)
// planNode() {}`). Returns "" for a shape the scan does not recognise so
// the caller skips it rather than recording a bogus name.
func receiverTypeName(recv *ast.FieldList) string {
	if len(recv.List) != 1 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}
