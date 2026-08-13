package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This pins #1971: a `-race` build of ANY chdb-tagged test binary died with
// SIGSEGV inside libchdb.so during runtime.racefini, at process exit, after
// every test in the binary had already passed — so a suite in which nothing
// failed reported the lane as failed.
//
// The mechanism is a teardown-ordering contract nobody was holding up.
// libchdb is an embedded ClickHouse dlopen'd into the test process; chdb-go
// caches ONE process-wide session and its driver's (*conn).Close is a no-op,
// so the native engine is still live when the process exits. A plain
// `go test` never notices, because Go's os.Exit reaches exit_group directly
// and libchdb's C++ static destructors never run. Under `-race`, os.Exit
// first calls runtime_beforeExit -> runtime.racefini -> __tsan_fini, which
// (per the Go runtime's own comment on racefini) "will run C atexit
// functions and C++ destructors" — tearing libchdb down while its engine
// still runs.
//
// internal/chdbsession.CloseForExit closes the session first, so those
// destructors run against an already-shut-down engine. It only works if
// every chdb-tagged test package actually CALLS it from its TestMain, and
// TestMain is per-package by language design — there is no single root to
// fix. This test is that root: it makes the per-package wiring a checked
// property instead of a convention a new package can silently skip.

const (
	// chdbSessionSelector / chdbSessionFunc name the wiring this test
	// enforces: a `chdbsession.CloseForExit()` call inside TestMain.
	chdbSessionSelector = "chdbsession"
	chdbSessionFunc     = "CloseForExit"

	// chdbSessionImplFile defines CloseForExit for the chdb build. The scan
	// below proves nothing if the helper it points every package at has been
	// deleted or renamed, so its presence is asserted separately.
	chdbSessionImplFile = "../../internal/chdbsession/close_chdb.go"

	// minChDBTestPackages is a floor under the discovered package set. The
	// walk is the only thing standing between a new chdb-tagged package and
	// a silently unwired TestMain, so a walk that suddenly finds almost
	// nothing — a moved tree, a broken filter — must fail loudly rather than
	// pass by finding no work to do. The real count was 25 when this landed;
	// the floor sits below that so ordinary churn does not trip it.
	minChDBTestPackages = 15
)

// chdbBuildTagRE matches a `//go:build` line that names the chdb tag. It is
// only ever applied to the text BEFORE a file's package clause, which is
// where build constraints must live — that also keeps this test file, whose
// own source contains the pattern below the package clause, from matching
// itself.
var chdbBuildTagRE = regexp.MustCompile(`(?m)^//go:build\b[^\n]*\bchdb\b`)

// TestChDBTaggedPackagesCloseSessionOnExit asserts that every package with a
// chdb-tagged test file declares a TestMain that calls
// chdbsession.CloseForExit.
func TestChDBTaggedPackagesCloseSessionOnExit(t *testing.T) {
	t.Parallel()

	// Vacuity guard: the wiring this test enforces is worthless if the
	// helper every TestMain calls no longer exists.
	impl, err := os.ReadFile(chdbSessionImplFile)
	if err != nil {
		t.Fatalf("read %s: %v\nthis is the helper every chdb-tagged TestMain calls; without it "+
			"the per-package scan below enforces a call to nothing", chdbSessionImplFile, err)
	}
	if !strings.Contains(string(impl), "func "+chdbSessionFunc+"(") {
		t.Fatalf("%s no longer defines %s — the scan below would enforce a call to a function "+
			"that does not exist", chdbSessionImplFile, chdbSessionFunc)
	}

	pkgDirs := chdbTaggedPackageDirs(t)
	if len(pkgDirs) < minChDBTestPackages {
		t.Fatalf("found only %d chdb-tagged test packages (floor %d) — the walk is not finding "+
			"the tree it is supposed to police, so a green result here proves nothing",
			len(pkgDirs), minChDBTestPackages)
	}

	for _, dir := range pkgDirs {
		if wired, err := packageTestMainClosesSession(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		} else if !wired {
			t.Errorf("%s has chdb-tagged tests but no TestMain calling %s.%s.\n"+
				"Under -race this package's test binary segfaults inside libchdb during "+
				"runtime.racefini at process exit, AFTER its tests pass (#1971). Add:\n\n"+
				"\tfunc TestMain(m *testing.M) {\n"+
				"\t\tcode := m.Run()\n"+
				"\t\t%s.%s()\n"+
				"\t\tos.Exit(code)\n"+
				"\t}\n",
				dir, chdbSessionSelector, chdbSessionFunc, chdbSessionSelector, chdbSessionFunc)
		}
	}
}

// chdbTaggedPackageDirs walks the repo for _test.go files carrying the chdb
// build tag and returns their directories, deduplicated and sorted.
func chdbTaggedPackageDirs(t *testing.T) []string {
	t.Helper()

	seen := map[string]bool{}
	for _, tree := range []string{"internal", "test"} {
		root := filepath.Join(repoRootFromRegression, tree)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if hasChDBBuildTag(string(body)) {
				seen[filepath.Dir(path)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sortStrings(dirs)
	return dirs
}

// hasChDBBuildTag reports whether src carries a `//go:build` constraint
// naming chdb. Only the header — everything before the package clause — is
// considered, since that is the only place a build constraint is honoured.
func hasChDBBuildTag(src string) bool {
	header := src
	if idx := strings.Index(src, "\npackage "); idx >= 0 {
		header = src[:idx]
	}
	return chdbBuildTagRE.MatchString(header)
}

// packageTestMainClosesSession reports whether any _test.go file in dir
// declares a TestMain whose body calls chdbsession.CloseForExit.
func packageTestMainClosesSession(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if parseErr != nil {
			return false, parseErr
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "TestMain" || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if callsCloseForExit(fn.Body) {
				return true, nil
			}
		}
	}
	return false, nil
}

// callsCloseForExit reports whether body contains a chdbsession.CloseForExit
// call.
func callsCloseForExit(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != chdbSessionFunc {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == chdbSessionSelector {
			found = true
			return false
		}
		return true
	})
	return found
}

// sortStrings keeps the failure output stable without pulling sort into the
// call site.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
