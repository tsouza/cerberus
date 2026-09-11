package promql

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Mixed-wrapper consumers decide from mixedRowsNeedPreparation, which keeps
// physical payload separate from the live-row proof. No production consumer
// compares RowShapeOf with MixedRowShape; adding a site means a consumer
// regressed to legacy inference.
func TestMixedWrapperLegacyShapeInventory(t *testing.T) {
	var want []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var got []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			found := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				binary, ok := node.(*ast.BinaryExpr)
				if ok && mixedRowShapeComparison(binary) {
					found = true
				}
				return true
			})
			if found {
				got = append(got, name+":"+function.Name.Name)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RowShapeOf mixed comparisons =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func mixedRowShapeComparison(binary *ast.BinaryExpr) bool {
	if binary.Op != token.EQL && binary.Op != token.NEQ {
		return false
	}
	return rowShapeOfCall(binary.X) && mixedRowShapeIdent(binary.Y) ||
		rowShapeOfCall(binary.Y) && mixedRowShapeIdent(binary.X)
}

func rowShapeOfCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "RowShapeOf"
}

func mixedRowShapeIdent(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "MixedRowShape"
}
