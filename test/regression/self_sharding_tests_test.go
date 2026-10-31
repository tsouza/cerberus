package regression

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The promql roundtrip legs run two kinds of top-level test. Most are
// hash-partitioned by NAME across the legs (.github/scripts/chdb-roundtrip.mjs
// runFilterFor): each runs whole, on one leg. A self-sharding test instead
// reads the SPEC_SHARD_INDEX / SPEC_SHARD_COUNT pair and runs only its own
// slice of its cases — fixtures through spec.ShardFromEnv, case-table
// subtests through spec.NewSubtestShard — so it must run on EVERY leg, which
// is what membership in SELF_SHARDING_TESTS.promql buys it.
//
// The two declarations live in different languages, and both ways of letting
// them drift are silent:
//
//   - a self-sharding test missing from the list is name-hashed onto one leg,
//     runs that leg's slice there, and the cases every other leg holds run
//     nowhere — while every leg reports green;
//   - a listed test that does not self-shard runs its whole body on every leg,
//     multiplying its cost by the leg count.
const (
	// selfShardingScript owns SELF_SHARDING_TESTS.
	selfShardingScript = ".github/scripts/chdb-roundtrip.mjs"
	// selfShardingHead is the head whose list this test pins: the only head
	// whose roundtrip lane fans out.
	selfShardingHead = "promql"
)

// selfShardingPackages are the directories the promql roundtrip legs run, the
// pair chdb-roundtrip.mjs's legCommands passes to `go test`.
var selfShardingPackages = []string{"test/spec/promql", "internal/promql"}

// shardReaders are the functions of specImportPath whose caller self-shards.
var shardReaders = []string{"ShardFromEnv", "NewSubtestShard"}

// TestSelfShardingTestsMatchTheirCallers pins SELF_SHARDING_TESTS.promql to
// exactly the top-level tests of the promql roundtrip packages that read the
// shard environment.
//
// Every file is parsed regardless of build tags, so the chdb-tagged tests are
// seen from this untagged lane. A shard reader called from anything other
// than a top-level Test function fails the test outright: the check below
// attributes a call to the function whose body holds it, and a helper would
// hide which tests reach it.
func TestSelfShardingTestsMatchTheirCallers(t *testing.T) {
	t.Parallel()

	var callers []string
	for _, dir := range selfShardingPackages {
		callers = append(callers, shardReaderCallers(t, filepath.Join(repoRoot, dir))...)
	}
	sort.Strings(callers)
	if len(callers) == 0 {
		t.Fatalf("no function in %v calls spec.%v; the comparison below would hold vacuously",
			selfShardingPackages, shardReaders)
	}

	listed := slices.Clone(selfShardingList(t))
	sort.Strings(listed)

	for _, name := range callers {
		if !slices.Contains(listed, name) {
			t.Errorf("%s reads the shard environment but is missing from SELF_SHARDING_TESTS.%s in %s: "+
				"it is name-hashed onto one leg, runs only that leg's slice of its cases, and the rest "+
				"run on no leg at all", name, selfShardingHead, selfShardingScript)
		}
	}
	for _, name := range listed {
		if !slices.Contains(callers, name) {
			t.Errorf("SELF_SHARDING_TESTS.%s in %s lists %s, which does not call spec.%v in %v: it runs "+
				"in full on every leg", selfShardingHead, selfShardingScript, name, shardReaders,
				selfShardingPackages)
		}
	}
}

// shardReaderCallers parses every Go file in dir and returns the top-level
// Test functions that call one of shardReaders through the spec package.
func shardReaderCallers(t *testing.T, dir string) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		specName := importName(file, specImportPath)
		if specName == "" {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !callsShardReader(fn.Body, specName) {
				continue
			}
			if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				t.Errorf("%s: %s calls a spec shard reader (%v) but is not a top-level Test function; "+
					"call it from the test itself so SELF_SHARDING_TESTS can be checked against it",
					fset.Position(fn.Pos()), fn.Name.Name, shardReaders)
				continue
			}
			callers = append(callers, fn.Name.Name)
		}
	}
	return callers
}

// importName is the name file refers to importPath by, or "" when it does
// not import it.
func importName(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return filepath.Base(importPath)
	}
	return ""
}

// callsShardReader reports whether body calls specName.<reader> for one of
// shardReaders.
func callsShardReader(body *ast.BlockStmt, specName string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == specName && slices.Contains(shardReaders, sel.Sel.Name) {
			found = true
		}
		return !found
	})
	return found
}

// selfShardingList reads SELF_SHARDING_TESTS.promql by importing the script
// that owns it, so this test and the leg runner cannot disagree about it. A
// missing `node` is a hard failure, as in mutationLegs.
func selfShardingList(t *testing.T) []string {
	t.Helper()

	program := "const m = await import('./" + selfShardingScript + "'); " +
		"process.stdout.write(JSON.stringify(m.SELF_SHARDING_TESTS))"
	cmd := exec.Command("node", "--input-type=module", "-e", program)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("read SELF_SHARDING_TESTS from %s: %v\n%s", selfShardingScript, err, stderr)
	}
	var lists map[string][]string
	if err := json.Unmarshal(out, &lists); err != nil {
		t.Fatalf("parse SELF_SHARDING_TESTS from %s: %v", selfShardingScript, err)
	}
	list, ok := lists[selfShardingHead]
	if !ok || len(list) == 0 {
		t.Fatalf("%s declares no SELF_SHARDING_TESTS.%s; got %s", selfShardingScript, selfShardingHead, out)
	}
	return list
}
