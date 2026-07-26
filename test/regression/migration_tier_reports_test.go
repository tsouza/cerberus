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

// TestMigrationTierJobsUploadTheirRunReport pins the second half of the same
// evidence chain: writing a run report proves nothing if the job never hands it
// to the aggregate.
//
// MODE=attest runs in migration-aggregate, which reads whatever the
// `migration-report-*` artifacts contain. A tier that writes its report into
// the runner's workspace and then uploads nothing leaves the aggregate holding
// reports for the other tiers — and every scenario of the silent tier reads as
// enumerated-but-never-run, no matter how green the tier itself was.
//
// migration-tier2 did exactly this even after it started writing a report: five
// scenarios passed, no artifact was uploaded, and the aggregate failed all five
// @tier2 stories for never having run. Write and upload are pinned together
// here because either one alone is an unfalsifiable pass.
func TestMigrationTierJobsUploadTheirRunReport(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(tier1WorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", tier1WorkflowPath, err)
	}
	body := string(workflow)

	entries, err := os.ReadDir(tiersDir)
	if err != nil {
		t.Fatalf("read %s: %v", tiersDir, err)
	}

	var checked int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Tier packages are named `<tier>-<substrate>`; the tier is the part the
		// jobs, tags and artifacts all key on.
		tier, _, ok := strings.Cut(e.Name(), "-")
		if !ok {
			t.Fatalf("tier package %q is not named <tier>-<substrate>, so its job and artifact "+
				"names cannot be derived", e.Name())
		}
		checked++

		job := workflowJobBody(t, body, "migration-"+tier)
		artifact := "name: migration-report-" + tier
		if !strings.Contains(job, artifact) {
			t.Errorf("job migration-%s uploads no %q artifact, so migration-aggregate never sees its "+
				"run report and MODE=attest fails every @%s scenario as never-run — however green the "+
				"tier itself was. Body:\n%s", tier, "migration-report-"+tier, tier, job)
		}
	}

	if checked != wantTierSuites {
		t.Fatalf("scanned %d tier packages under %s, want %d", checked, tiersDir, wantTierSuites)
	}

	// The aggregate must collect them all. A pattern that stopped matching would
	// silently narrow the evidence base rather than fail.
	if !strings.Contains(body, "pattern: migration-report-*") {
		t.Fatalf("%s does not download the migration-report-* artifacts; MODE=attest would run over "+
			"an empty report directory and fail every scenario in the tree", tier1WorkflowPath)
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
