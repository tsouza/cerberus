package chsql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEmitterReDerivesRoleResolution pins that resolving a chplan.Column
// role (or histogram field) to the column carrying it happens in exactly one
// place, chplan.Schema.UniqueNamedRole / UniqueNamedHistogramField, and never
// by hand in an emitter.
//
// The signature of a hand re-derivation is a loop over a schema's Columns
// whose body consults a column's identity (`.Role` or `.HistogramField`) AND
// keeps the column — reads its `.Name`, or assigns the whole column to a
// variable that outlives the iteration: that is a role-to-name mapping, which
// is the resolver's job. A loop that only compares roles (a positional shape
// check between two schemas) or only reads names (a projection list) is not
// one, and is left alone.
//
// The second lookup path is chplan.Schema.Find / FindHistogramField: the
// first carrier, or the boolean view, with the failure reason discarded. An
// emitter never calls either; it resolves through the error-returning
// resolvers so a malformed child is rejected for the reason it is malformed.
// (Schema.Has, a presence check, is not a resolution and stays allowed.)
//
// The test walks every non-test file of this package and fails naming the
// file:line of each loop or call that matches.
func TestNoEmitterReDerivesRoleResolution(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && callsFirstCarrierLookup(call) {
				offenders = append(offenders, fset.Position(call.Pos()).String()+" calls "+call.Fun.(*ast.SelectorExpr).Sel.Name)
				return true
			}
			loop, ok := n.(*ast.RangeStmt)
			if !ok || !rangesOverColumns(loop) {
				return true
			}
			column, ok := loop.Value.(*ast.Ident)
			if !ok || column.Name == "_" {
				return true
			}
			if readsIdentity(loop.Body, column.Name) && keepsColumn(loop.Body, column.Name) {
				offenders = append(offenders, fset.Position(loop.Pos()).String())
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("no package source parsed; the walk is looking at the wrong directory")
	}
	if len(offenders) > 0 {
		t.Fatalf("%d site(s) resolve a role to a column by hand or through Schema.Find instead of "+
			"chplan.Schema.UniqueNamedRole / UniqueNamedHistogramField:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// callsFirstCarrierLookup reports whether call is `<expr>.Find(x)` or
// `<expr>.FindHistogramField(x)`: the one-argument method forms of
// chplan.Schema's first-carrier lookups. No other type in this package's
// sources has a method of either name, so the name alone identifies them.
func callsFirstCarrierLookup(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	return sel.Sel.Name == "Find" || sel.Sel.Name == "FindHistogramField"
}

// rangesOverColumns reports whether loop iterates `<expr>.Columns`.
func rangesOverColumns(loop *ast.RangeStmt) bool {
	sel, ok := loop.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Columns"
}

// readsIdentity reports whether body contains `<column>.Role` or
// `<column>.HistogramField`.
func readsIdentity(body ast.Node, column string) bool {
	return readsField(body, column, "Role") || readsField(body, column, "HistogramField")
}

// keepsColumn reports whether body reads `<column>.Name` or assigns the
// whole `<column>` to something that outlives the iteration.
func keepsColumn(body ast.Node, column string) bool {
	if readsField(body, column, "Name") {
		return true
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range assign.Rhs {
			if ident, ok := rhs.(*ast.Ident); ok && ident.Name == column {
				found = true
			}
		}
		return !found
	})
	return found
}

func readsField(body ast.Node, receiver, field string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != field {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == receiver {
			found = true
		}
		return !found
	})
	return found
}

// TestRoleResolutionClassTestSeesTheEmitterSources guards the walk above
// against silently scanning nothing: the resolver's own consumers must be
// among the files it parses.
func TestRoleResolutionClassTestSeesTheEmitterSources(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"child_schema_columns.go", "set_op.go", "emit_node.go"} {
		if _, err := os.Stat(filepath.Join(".", name)); err != nil {
			t.Fatalf("%s is not visible from the package directory: %v", name, err)
		}
	}
}
