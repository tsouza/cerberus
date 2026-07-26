package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tiersDir holds one package per tier, each standing up its own godog suite.
const tiersDir = "../../test/e2e/migration/tiers"

// wantTierSuites is the number of tier packages the lane drives. Pinned so a
// tier deleted (or a fourth added and left unwired) is noticed here rather than
// by a roll-up that silently has one fewer result to weigh.
const wantTierSuites = 3

// TestMigrationTierSuitesResolveTheirFormatter pins every tier's godog suite to
// resolving its formatter through lib.SuiteFormat rather than naming one
// inline.
//
// SuiteFormat returns the pretty formatter for the job log AND, when the lane
// asks for one, a cucumber-JSON run report beside it. That report is the only
// evidence MODE=attest has: attestation holds each counted scenario to
// "executed, and passed" instead of "exists in a feature file", and with no
// report it fires A0 — no run report at all.
//
// A tier that hardcodes "pretty" therefore fails in the most expensive possible
// place. tier2-ruler did exactly that: its five scenarios and forty-seven steps
// all passed, the compose stack came up and went down, and the lane then failed
// attestation because nothing had been written for it to attest. Every tier
// running green while the lane reports failure is the worst signal this suite
// can emit, so the shape is pinned here in the required `check` lane, where it
// costs a file read.
//
// The scan reads the AST, so a comment or an error string mentioning "pretty"
// cannot satisfy or break it — only the value actually assigned to Options.Format.
func TestMigrationTierSuitesResolveTheirFormatter(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(tiersDir)
	if err != nil {
		t.Fatalf("read %s: %v", tiersDir, err)
	}

	var checked int
	for _, tier := range entries {
		if !tier.IsDir() {
			continue
		}
		dir := filepath.Join(tiersDir, tier.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}

		var (
			sawOptions bool
			sawResolve bool
		)
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}

			ast.Inspect(file, func(n ast.Node) bool {
				if isSuiteFormatCall(n) {
					sawResolve = true
					return true
				}
				value, ok := godogOptionsFormatValue(n)
				if !ok {
					return true
				}
				sawOptions = true
				if lit, literal := value.(*ast.BasicLit); literal {
					t.Errorf("%s names its godog formatter inline (Format: %s). Resolve it through "+
						"lib.SuiteFormat instead: a hardcoded formatter writes no cucumber run report, so "+
						"the tier's scenarios can pass while MODE=attest fires A0 and fails the lane with "+
						"nothing to attest.", path, lit.Value)
				}
				return true
			})
		}

		if !sawOptions {
			t.Errorf("tier %s declares no godog.Options; it stands up no suite, or the suite moved "+
				"somewhere this pin no longer reads", tier.Name())
			continue
		}
		if !sawResolve {
			t.Errorf("tier %s never calls lib.SuiteFormat, so it emits no cucumber run report and its "+
				"scenarios cannot be attested as executed", tier.Name())
		}
		checked++
	}

	if checked != wantTierSuites {
		t.Fatalf("scanned %d tier suites under %s, want %d", checked, tiersDir, wantTierSuites)
	}
}

// isSuiteFormatCall reports whether n is a call to lib.SuiteFormat.
func isSuiteFormatCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "SuiteFormat" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "lib"
}

// godogOptionsFormatValue returns the expression assigned to the Format field of
// a `godog.Options{…}` composite literal.
func godogOptionsFormatValue(n ast.Node) (ast.Expr, bool) {
	lit, ok := n.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Options" {
		return nil, false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "godog" {
		return nil, false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Format" {
			return kv.Value, true
		}
	}
	return nil, false
}
