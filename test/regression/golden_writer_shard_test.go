package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// specImportPath is the package whose Match rewrites a TXTAR golden section
// under GOLDEN_UPDATE=1.
const specImportPath = "github.com/tsouza/cerberus/test/spec"

// specMatchFunc is the golden-rewriting entry point of specImportPath.
const specMatchFunc = "Match"

// goTestRunFlag introduces a plan command's test-name filter.
const goTestRunFlag = "-run"

// goldenWriter is one test function that rewrites TXTAR goldens.
type goldenWriter struct {
	pkg  string // repo-relative package directory, slash-separated
	name string // top-level test function name
}

// skippedWalkDirs are directory names that hold no first-party Go source.
var skippedWalkDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "bin": true}

// goldenWriters lists every top-level test function in the repository that
// calls spec.Match, directly or from a closure inside it.
func goldenWriters(t *testing.T) []goldenWriter {
	t.Helper()

	var out []goldenWriter
	fset := token.NewFileSet()
	err := filepath.WalkDir(repoRootFromRegression, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedWalkDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		local := specLocalName(f)
		if local == "" {
			return nil
		}
		rel, err := filepath.Rel(repoRootFromRegression, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !callsSpecMatch(fn.Body, local) {
				continue
			}
			if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				t.Errorf("%s: %s calls %s.%s outside a top-level Test function, so no `go test -run` "+
					"filter can name the generator that rewrites its goldens. Call it from the Test "+
					"function (or a closure inside it).", path, fn.Name.Name, local, specMatchFunc)
				continue
			}
			out = append(out, goldenWriter{pkg: filepath.ToSlash(rel), name: fn.Name.Name})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRootFromRegression, err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].pkg+"."+out[i].name < out[j].pkg+"."+out[j].name
	})
	return out
}

// specLocalName returns the name a file refers to specImportPath by, or "".
func specLocalName(f *ast.File) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != specImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return filepath.Base(specImportPath)
	}
	return ""
}

// callsSpecMatch reports whether body contains a `<local>.Match(...)` call.
func callsSpecMatch(body ast.Node, local string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != specMatchFunc {
			return !found
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
			found = true
		}
		return !found
	})
	return found
}

// planCommand is one golden-rewriting `go test` step of the update-golden plan.
type planCommand struct {
	shard string
	pkgs  []string
	run   *regexp.Regexp
}

// goldenRewritingCommands extracts every GOLDEN_UPDATE=1 `go test` step of the
// `all` plan, with its package list and its -run filter.
func goldenRewritingCommands(t *testing.T) []planCommand {
	t.Helper()

	var cmds []planCommand
	for _, line := range goldenUpdatePlan(t) {
		if !strings.Contains(line, goldenUpdateEnvMarker) {
			continue
		}
		fields := strings.Fields(line)
		c := planCommand{shard: fields[1]}
		for i, f := range fields {
			switch {
			case f == goTestRunFlag && i+1 < len(fields):
				re, err := regexp.Compile(fields[i+1])
				if err != nil {
					t.Fatalf("plan line carries an invalid -run filter %q: %v\n%s", fields[i+1], err, line)
				}
				c.run = re
			case strings.HasPrefix(f, "./"):
				c.pkgs = append(c.pkgs, strings.TrimSuffix(strings.TrimPrefix(f, "./"), "/"))
			}
		}
		cmds = append(cmds, c)
	}
	return cmds
}

// covers reports whether c runs top-level test name in package pkg.
func (c planCommand) covers(pkg, name string) bool {
	if c.run != nil && !c.run.MatchString(name) {
		return false
	}
	for _, p := range c.pkgs {
		if p == pkg {
			return true
		}
	}
	return false
}

// TestEveryGoldenWriterIsAShardGenerator pins that `just update-golden`
// regenerates every TXTAR golden the tree holds: each test function that
// rewrites a fixture section through spec.Match under GOLDEN_UPDATE=1 must be
// run, in its own package, by some shard's plan.
//
// A writer no shard runs leaves its fixtures stale after any regeneration, and
// only an asserting CI lane notices — which is how the query_exemplars fixtures
// under test/spec/promql drifted while `update-golden promql chsql` reported a
// clean diff.
func TestEveryGoldenWriterIsAShardGenerator(t *testing.T) {
	t.Parallel()

	writers := goldenWriters(t)
	if len(writers) == 0 {
		t.Fatalf("found no test function calling %s.%s under %s; the scan is not seeing the tree "+
			"and this check cannot fail", filepath.Base(specImportPath), specMatchFunc, repoRootFromRegression)
	}
	cmds := goldenRewritingCommands(t)
	if len(cmds) == 0 {
		t.Fatalf("the `all` plan holds no %s go test step; this check cannot fail", goldenUpdateEnvMarker)
	}

	for _, w := range writers {
		covered := false
		for _, c := range cmds {
			if c.covers(w.pkg, w.name) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s in ./%s/ rewrites TXTAR goldens through spec.Match, but no shard in "+
				".github/scripts/lib/golden-shards.mjs runs it, so `just update-golden` leaves its "+
				"fixtures stale. Add it as a generator of the shard whose goldens it writes.",
				w.name, w.pkg)
		}
	}
}
