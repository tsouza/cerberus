package spec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestFnGoNames_CoversEveryDeclaredFn and TestFnGoNames_NoOrphanEntry
// ratchet fnGoNames (chplan_print.go) against internal/chplan/fn.go's
// const block, the same derive-from-source approach
// internal/chsql/fnresolution_completeness_test.go already uses for
// fnResolutions: a hand-maintained list compared against another
// hand-maintained list proves nothing when both get edited in the same
// commit, so the declared side is parsed straight out of fn.go instead
// of restated by hand here.
func TestFnGoNames_CoversEveryDeclaredFn(t *testing.T) {
	t.Parallel()

	declared := declaredFnNames(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan/fn.go and found no `Fn = \"...\"` const declarations — " +
			"the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	for name, value := range declared {
		if got, ok := fnGoNames[chplan.Fn(value)]; !ok {
			t.Errorf("chplan.%s (%q) has no entry in fnGoNames (chplan_print.go) — "+
				"add one, or an IR snapshot involving it silently falls back to the raw "+
				"CH spelling instead of the symbol name", name, value)
		} else if got != name {
			t.Errorf("fnGoNames[chplan.%s] = %q, want %q", name, got, name)
		}
	}
}

// TestFnGoNames_NoOrphanEntry fails when fnGoNames carries an entry
// naming a Fn value fn.go no longer declares.
func TestFnGoNames_NoOrphanEntry(t *testing.T) {
	t.Parallel()

	declared := declaredFnNames(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan/fn.go and found no `Fn = \"...\"` const declarations — " +
			"the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, value := range declared {
		declaredValues[value] = true
	}

	for fn := range fnGoNames {
		if !declaredValues[string(fn)] {
			t.Errorf("fnGoNames has an entry for %q, which internal/chplan/fn.go no longer "+
				"declares as an Fn constant — drop it", string(fn))
		}
	}
}

// TestCombinatorGoNames_CoversEveryDeclaredAggCombinator and
// TestCombinatorGoNames_NoOrphanEntry are combinatorGoNames' own pair of
// the same ratchet TestFnGoNames_CoversEveryDeclaredFn /
// TestFnGoNames_NoOrphanEntry run for fnGoNames (issue #2280's structural
// Combinators split). Unlike fnGoNames — whose stored value is always the
// declaring identifier's own name, checked for equality —
// combinatorGoNames stores the SUFFIX printAggFunc appends (CombIf ->
// "If", not "CombIf"), so only PRESENCE is checked here, not the value.
func TestCombinatorGoNames_CoversEveryDeclaredAggCombinator(t *testing.T) {
	t.Parallel()

	declared := declaredAggCombinatorNames(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan for `AggCombinator = \"...\"` const declarations and found " +
			"none — the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	for name, value := range declared {
		if _, ok := combinatorGoNames[chplan.AggCombinator(value)]; !ok {
			t.Errorf("chplan.%s (%q) has no entry in combinatorGoNames (chplan_print.go) — "+
				"add one, or an IR snapshot involving it silently falls back to the raw "+
				"CH suffix instead of the printer's own choice", name, value)
		}
	}
}

// TestCombinatorGoNames_NoOrphanEntry fails when combinatorGoNames
// carries an entry naming an AggCombinator value chplan no longer
// declares.
func TestCombinatorGoNames_NoOrphanEntry(t *testing.T) {
	t.Parallel()

	declared := declaredAggCombinatorNames(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan for `AggCombinator = \"...\"` const declarations and found " +
			"none — the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, value := range declared {
		declaredValues[value] = true
	}

	for c := range combinatorGoNames {
		if !declaredValues[string(c)] {
			t.Errorf("combinatorGoNames has an entry for %q, which internal/chplan no longer "+
				"declares as an AggCombinator constant — drop it", string(c))
		}
	}
}

// declaredFnNames scans every non-test .go file in internal/chplan for
// `const` declarations typed Fn, returning symbol name -> underlying
// string value (e.g. "FnArrayMap" -> "arrayMap"). Mirrors
// internal/chsql/fnresolution_completeness_test.go's declaredFnValues,
// duplicated here rather than shared because that helper lives in a
// _test.go file and is not importable from this package.
func declaredFnNames(t *testing.T) map[string]string {
	t.Helper()
	return declaredTypedConstNamesForFnNames(t, "Fn")
}

// declaredAggCombinatorNames is declaredFnNames' sibling for the
// AggCombinator vocabulary (internal/chplan/aggregate.go).
func declaredAggCombinatorNames(t *testing.T) map[string]string {
	t.Helper()
	return declaredTypedConstNamesForFnNames(t, "AggCombinator")
}

// declaredTypedConstNamesForFnNames scans every non-test .go file in
// internal/chplan for top-level `const` declarations typed typeName,
// returning symbol name -> underlying string value.
func declaredTypedConstNamesForFnNames(t *testing.T, typeName string) map[string]string {
	t.Helper()

	chplanDir := filepath.Join("..", "..", "internal", "chplan")
	entries, err := os.ReadDir(chplanDir)
	if err != nil {
		t.Fatalf("read internal/chplan: %v", err)
	}

	var files []*ast.File
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
		files = append(files, f)
	}

	literals := map[string]string{}
	for _, f := range files {
		collectStringLiteralConstsForFnNames(f, literals)
	}

	out := map[string]string{}
	for _, f := range files {
		collectTypedConstsForFnNames(f, literals, typeName, out)
	}
	return out
}

func collectStringLiteralConstsForFnNames(f *ast.File, out map[string]string) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out[n.Name] = value
			}
		}
	}
}

func collectTypedConstsForFnNames(f *ast.File, literals map[string]string, typeName string, out map[string]string) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var lastType ast.Expr
		var lastValues []ast.Expr
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if vs.Type != nil {
				lastType = vs.Type
			}
			if len(vs.Values) > 0 {
				lastValues = vs.Values
			}
			ident, ok := lastType.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for i, n := range vs.Names {
				if i >= len(lastValues) {
					continue
				}
				switch rhs := lastValues[i].(type) {
				case *ast.BasicLit:
					if rhs.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(rhs.Value)
					if err != nil {
						continue
					}
					out[n.Name] = value
				case *ast.Ident:
					if value, ok := literals[rhs.Name]; ok {
						out[n.Name] = value
					}
				}
			}
		}
	}
}
