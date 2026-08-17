package chsql

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

// TestFnResolutions_CoversEveryDeclaredFn and TestFnResolutions_NoOrphanEntry
// are the two completeness ratchets #2060 PR 1 asks for: chplan.Fn (the
// engine-agnostic vocabulary, internal/chplan/fn.go) and chsql's
// fnResolutions (its ClickHouse resolution, fnresolution.go) must name the
// exact same set of functions.
//
// Both derive the declared side from source with go/parser, the same
// approach internal/sealedscan and internal/solver's
// offset_widening_test.go already use for cross-package completeness
// ratchets: a hand-maintained list compared against another
// hand-maintained list proves nothing, because both get edited together
// in the same commit (or in neither). Scanning fn.go's const block means
// a new Fn constant trips the ratchet by name, with nothing to bump.
//
// The declared side is the SET OF UNDERLYING STRING VALUES the const
// block assigns (`FnArrayMap Fn = "arrayMap"` contributes "arrayMap"),
// not the Go identifiers — chsql's map is keyed by chplan.Fn *values*,
// and parsing the literal RHS is the same "declarations only, no
// go/types" scan sealedscan itself uses to stay lightweight.

// TestFnResolutions_CoversEveryDeclaredFn fails when internal/chplan/fn.go
// declares an Fn constant fnResolutions does not resolve — the gap #2060's
// design calls out explicitly: "a missing one is a named test failure,
// not a runtime CH error."
func TestFnResolutions_CoversEveryDeclaredFn(t *testing.T) {
	t.Parallel()

	declared := declaredFnValues(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan/fn.go and found no `Fn = \"...\"` const declarations — " +
			"the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	for name, value := range declared {
		if _, ok := fnResolutions[chplan.Fn(value)]; !ok {
			t.Errorf("chplan.%s (%q) has no entry in chsql.fnResolutions (fnresolution.go) — "+
				"add one, or the emitter falls through to a runtime ClickHouse error instead "+
				"of a compile/test failure", name, value)
		}
	}
}

// TestFnResolutions_NoOrphanEntry fails when fnResolutions carries an entry
// naming a Fn value fn.go no longer declares — the const block IS the
// vocabulary, so a table entry with no matching const is a ghost left
// behind by a rename or deletion.
func TestFnResolutions_NoOrphanEntry(t *testing.T) {
	t.Parallel()

	declared := declaredFnValues(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan/fn.go and found no `Fn = \"...\"` const declarations — " +
			"the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, value := range declared {
		declaredValues[value] = true
	}

	for fn := range fnResolutions {
		if !declaredValues[string(fn)] {
			t.Errorf("chsql.fnResolutions has an entry for %q, which internal/chplan/fn.go no "+
				"longer declares as an Fn constant — drop it", string(fn))
		}
	}
}

// TestCombinatorSuffixes_CoversEveryDeclaredAggCombinator and
// TestCombinatorSuffixes_NoOrphanEntry are combinatorSuffixes' own pair of
// the same ratchet TestFnResolutions_CoversEveryDeclaredFn /
// TestFnResolutions_NoOrphanEntry run for fnResolutions (issue #2280's
// structural Combinators split): internal/chplan/aggregate.go's
// AggCombinator vocabulary and chsql's combinatorSuffixes resolution table
// must name the exact same set of combinators.

// TestCombinatorSuffixes_CoversEveryDeclaredAggCombinator fails when
// internal/chplan declares an AggCombinator constant combinatorSuffixes
// does not resolve.
func TestCombinatorSuffixes_CoversEveryDeclaredAggCombinator(t *testing.T) {
	t.Parallel()

	declared := declaredAggCombinatorValues(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan for `AggCombinator = \"...\"` const declarations and found " +
			"none — the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	for name, value := range declared {
		if _, ok := combinatorSuffixes[chplan.AggCombinator(value)]; !ok {
			t.Errorf("chplan.%s (%q) has no entry in chsql.combinatorSuffixes (fnresolution.go) — "+
				"add one, or the emitter falls through to a runtime ClickHouse error instead "+
				"of a compile/test failure", name, value)
		}
	}
}

// TestCombinatorSuffixes_NoOrphanEntry fails when combinatorSuffixes
// carries an entry naming an AggCombinator value chplan no longer
// declares.
func TestCombinatorSuffixes_NoOrphanEntry(t *testing.T) {
	t.Parallel()

	declared := declaredAggCombinatorValues(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan for `AggCombinator = \"...\"` const declarations and found " +
			"none — the scan lost its grip on the source shape, so this ratchet is vacuous")
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, value := range declared {
		declaredValues[value] = true
	}

	for c := range combinatorSuffixes {
		if !declaredValues[string(c)] {
			t.Errorf("chsql.combinatorSuffixes has an entry for %q, which internal/chplan no "+
				"longer declares as an AggCombinator constant — drop it", string(c))
		}
	}
}

// declaredFnValues scans every non-test .go file in internal/chplan for
// `const` declarations typed Fn, returning symbol name -> underlying
// string value (e.g. "FnArrayMap" -> "arrayMap").
//
// A const's RHS is either a string literal directly, or (once, for
// FnMapSort) an identifier reusing another package-level string constant's
// spelling rather than restating it — see CanonicalMapFunc's doc comment.
// The scan resolves that one indirection level from a first pass over
// every top-level string-literal const in the package, Fn-typed or not,
// so an Fn constant spelled by reference is still counted as declared
// instead of silently vanishing from this ratchet.
func declaredFnValues(t *testing.T) map[string]string {
	t.Helper()
	return declaredTypedConstValues(t, "Fn")
}

// declaredAggCombinatorValues is declaredFnValues' sibling for the
// AggCombinator vocabulary (internal/chplan/aggregate.go).
func declaredAggCombinatorValues(t *testing.T) map[string]string {
	t.Helper()
	return declaredTypedConstValues(t, "AggCombinator")
}

// declaredTypedConstValues scans every non-test .go file in
// internal/chplan for top-level `const` declarations typed typeName,
// returning symbol name -> underlying string value.
func declaredTypedConstValues(t *testing.T, typeName string) map[string]string {
	t.Helper()

	const chplanDir = "../chplan"
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
		collectStringLiteralConsts(f, literals)
	}

	out := map[string]string{}
	for _, f := range files {
		collectTypedConsts(f, literals, typeName, out)
	}
	return out
}

// collectStringLiteralConsts walks f's top-level const blocks, recording
// every spec whose RHS is a plain string literal — regardless of type —
// so an Fn const referencing one by identifier can be resolved.
func collectStringLiteralConsts(f *ast.File, out map[string]string) {
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

// collectTypedConsts walks f's top-level const blocks, recording every
// spec typed typeName. Go lets a const spec omit Type/Values to inherit
// the previous spec's (the iota idiom); this codebase's Fn/AggCombinator
// blocks state both explicitly on every line, but the scan tracks the
// last seen Type/Values anyway so it degrades safely rather than
// silently under-counting if that style ever changes.
func collectTypedConsts(f *ast.File, literals map[string]string, typeName string, out map[string]string) {
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
