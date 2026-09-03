package regression

// Class-closing gate for #2971: the PromQL parser's options must have exactly
// one source, and it must be the one production parses through.
//
// WHAT WENT WRONG. Ten non-test sites each spelled
// `parser.NewParser(parser.Options{EnableExperimentalFunctions: true})` inline
// — the PromQL HTTP handler, both offline explain langs, the migration rule
// graph and six bench-report harnesses — with nothing holding the ten in
// agreement. That is a DRY defect on its face, but one of those options is
// load-bearing: with ExperimentalDurationExpr unset the grammar rejects a
// duration EXPRESSION, which is the only spelling that parses to a ZERO
// MatrixSelector.Range, and the histogram-quantile lowerings bind their window
// and staleness bounds from that field with no positivity guard (the guards
// were deleted in #2970 on precisely that proof). Enabling the option at ONE
// entry point would have re-opened a degenerate window THERE while the other
// nine stayed correct, and no test in the tree would have said so — the pin
// that asserts the invariant constructed an ELEVENTH copy of the options, so
// it was asserting about itself.
//
// WHAT THIS ENFORCES. [promparse.New] is the single construction site. Any
// other non-test file that calls the upstream parser's NewParser is a site
// that can carry different options, and this test fails on it by name. The
// remedy is never an entry here — it is calling promparse.New.
//
// SCOPE, stated plainly. The scan covers non-test Go files only. Tests
// legitimately build parsers with other options to exercise the grammar itself
// (`parser.Options{}` appears in hundreds of `_test.go` files to assert what
// the base grammar accepts), and banning those would police the wrong thing.
// The one test that must NOT diverge — internal/promql's windowRange pin —
// is coupled by construction instead: it parses through promparse.New, so it
// cannot stay green while a production site changes options.
//
// Vendored upstream snapshots under compatibility/*/upstream/** are outside
// cerberus's authorship boundary (the same exclusion forbid-skip.mjs applies)
// and are not rewritten to call cerberus code.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// upstreamPromQLParserPkg is the import path whose NewParser this gate
// centralises. Aliases do not matter: the local name is resolved per file from
// the import spec rather than assumed.
const upstreamPromQLParserPkg = "github.com/prometheus/prometheus/promql/parser"

// promparseImplFile is the ONE file allowed to call the upstream constructor —
// the implementation of the shared configuration, not a consumer of it.
const promparseImplFile = "internal/promql/promparse/promparse.go"

// promparseSelector is the call every consumer makes instead.
const promparseSelector = "promparse.New"

// TestPromQLParserOptionsHaveASingleSource walks the repository's non-test Go
// files and fails on any that constructs an upstream PromQL parser directly.
func TestPromQLParserOptionsHaveASingleSource(t *testing.T) {
	t.Parallel()

	var offenders, consumers []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Only the shared noise-directory names are skipped, by name.
			// lint_build_tags_test.go additionally matches skippedDirs
			// against the relative path, which drops the nested test/oracle
			// module; this scan deliberately keeps it. That module carries a
			// `replace github.com/tsouza/cerberus => ../..`, so its PromQL
			// inventory can and does construct a cerberus parser, and a copy
			// of the options there would drift exactly like any other.
			if skippedDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !isScannedGoFile(rel) {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		if rel != promparseImplFile {
			for _, pos := range directParserConstructions(file) {
				offenders = append(offenders, fmt.Sprintf("%s: %s", rel, pos))
			}
		}
		if callsSharedConstructor(file) {
			consumers = append(consumers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("non-test files construct a PromQL parser outside %s:\n  %s\n\n"+
			"Each such site carries its own copy of parser.Options, and one of those options "+
			"(ExperimentalDurationExpr) decides whether MatrixSelector.Range can be zero — which "+
			"the histogram-quantile lowerings bind their window from without a positivity guard. "+
			"Call %s() instead; if the options genuinely need to differ at this site, that is a "+
			"lowering question, not a local flag.",
			promparseImplFile, strings.Join(offenders, "\n  "), promparseSelector)
	}

	// A scan over a tree where nothing parses PromQL any more would report
	// clean for the wrong reason. The consumers are what the gate protects,
	// so their absence is a failure, not a pass.
	if len(consumers) == 0 {
		t.Fatalf("no non-test file calls %s(); the gate above passed vacuously", promparseSelector)
	}
}

// TestPromQLParserSingleSourceDetectorFires proves the detector is not inert:
// it reports the constructor under a plain import, under an alias, and inside
// a composite literal, and stays silent on the shared call and on a same-named
// method of an unrelated package.
func TestPromQLParserSingleSourceDetectorFires(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{
			name: "plain import",
			src: `package p
import "` + upstreamPromQLParserPkg + `"
func f() { _ = parser.NewParser(parser.Options{EnableExperimentalFunctions: true}) }`,
			want: 1,
		},
		{
			name: "aliased import",
			src: `package p
import promparser "` + upstreamPromQLParserPkg + `"
func f() { _ = promparser.NewParser(promparser.Options{}) }`,
			want: 1,
		},
		{
			name: "inside a composite literal",
			src: `package p
import promparser "` + upstreamPromQLParserPkg + `"
type lang struct{ Parser any }
func f() *lang { return &lang{Parser: promparser.NewParser(promparser.Options{})} }`,
			want: 1,
		},
		{
			name: "shared constructor",
			src: `package p
import "github.com/tsouza/cerberus/internal/promql/promparse"
func f() { _ = promparse.New() }`,
			want: 0,
		},
		{
			name: "unrelated package with the same method name",
			src: `package p
import "github.com/tsouza/cerberus/internal/logql/lsyntax"
func f() { _ = lsyntax.NewParser() }`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, tc.name+".go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got := directParserConstructions(file)
			if len(got) != tc.want {
				t.Fatalf("directParserConstructions found %d constructions (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

// isScannedGoFile reports whether the repo-relative path is a non-test Go file
// inside cerberus's authorship boundary.
func isScannedGoFile(rel string) bool {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	// compatibility/<backend>/upstream/** is a pinned vendored snapshot of the
	// reference backend's own source; it is reference material, not cerberus
	// code, and forbid-skip.mjs excludes it by the same shape.
	segments := strings.Split(rel, "/")
	if len(segments) > 3 && segments[0] == "compatibility" && segments[2] == "upstream" {
		return false
	}
	return true
}

// directParserConstructions returns the receiver-qualified NewParser calls the
// file makes against the upstream PromQL parser package, as `alias.NewParser`
// strings. The local name is read from the file's own import spec, so an alias
// cannot hide a call.
func directParserConstructions(file *ast.File) []string {
	locals := map[string]bool{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != upstreamPromQLParserPkg {
			continue
		}
		if imp.Name != nil {
			locals[imp.Name.Name] = true
			continue
		}
		locals[path[strings.LastIndex(path, "/")+1:]] = true
	}
	if len(locals) == 0 {
		return nil
	}

	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewParser" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || !locals[pkg.Name] {
			return true
		}
		found = append(found, pkg.Name+".NewParser")
		return true
	})
	return found
}

// callsSharedConstructor reports whether the file consumes promparse.New.
func callsSharedConstructor(file *ast.File) bool {
	pkg, fn, _ := strings.Cut(promparseSelector, ".")
	consumes := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != fn {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if ok && ident.Name == pkg {
			consumes = true
		}
		return true
	})
	return consumes
}
