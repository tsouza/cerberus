package regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAGPLOracleTagSetCoverage asserts that every `//go:build agpl_oracle`
// file in the root module is known to the agpl-oracle CI lane
// (.github/workflows/agpl-oracle.yml). This is the "hold the tag set to the
// lanes" regression described in #1610: a new oracle file that carries the tag
// but is not covered by the lane's `go test` scope would compile under no CI
// lane, defeating the differential oracle entirely.
//
// The test does NOT run the tagged files itself (it carries no agpl_oracle tag)
// — it only verifies coverage. The agpl-oracle workflow runs
// `go test -tags agpl_oracle -count=1 ./internal/... ./test/agpl_oracle/...
// ./test/surface-parity/... ./test/property/...`, which covers every file in
// the set below. If you add a new //go:build agpl_oracle file outside those
// scopes, add the package path to agplOracleLaneScopes below AND extend the
// workflow's `go test` invocation to include it.
func TestAGPLOracleTagSetCoverage(t *testing.T) {
	// Resolve to an absolute path so filepath.Base of the root isn't "..".
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	// The package subtrees the agpl-oracle lane covers with `go test -tags
	// agpl_oracle`. Expressed as repo-relative directory prefixes (slash-separated).
	// These must match the scopes in .github/workflows/agpl-oracle.yml's
	// "Run agpl_oracle differential oracle tests" step.
	agplOracleLaneScopes := []string{
		"internal/",
		"test/agpl_oracle/",
		"test/surface-parity/",
		"test/property/",
	}

	tagged := findAGPLOracleFiles(t, absRoot)
	if len(tagged) == 0 {
		t.Fatal("no //go:build agpl_oracle files found in the root module; " +
			"either the tag was removed from all files (update this test) " +
			"or repoRoot is wrong")
	}

	var uncovered []string
	for _, rel := range tagged {
		rel = filepath.ToSlash(rel)
		covered := false
		for _, scope := range agplOracleLaneScopes {
			if strings.HasPrefix(filepath.ToSlash(rel), scope) {
				covered = true
				break
			}
		}
		if !covered {
			uncovered = append(uncovered, rel)
		}
	}

	if len(uncovered) > 0 {
		t.Errorf("%d //go:build agpl_oracle file(s) are NOT covered by the agpl-oracle CI lane:\n",
			len(uncovered))
		for _, f := range uncovered {
			t.Errorf("  %s", f)
		}
		t.Error("\nAdd the file's package subtree to agplOracleLaneScopes in this test AND " +
			"to the `go test -tags agpl_oracle` invocation in " +
			".github/workflows/agpl-oracle.yml.")
	}

	t.Logf("agpl_oracle tag set: %d file(s) across %d scope(s) — all covered",
		len(tagged), len(agplOracleLaneScopes))
	for _, f := range tagged {
		t.Logf("  %s", filepath.ToSlash(f))
	}
}

// findAGPLOracleFiles walks the module root and returns repo-relative paths
// of every Go file (including _test.go) whose first build constraint line
// contains agpl_oracle.
func findAGPLOracleFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Skip hidden dirs and vendor, but NOT the root itself
			// (filepath.Base of an absolute root is the dir name, not ".").
			if path != root && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			// Nested module: has its own go.mod — don't recurse.
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		if hasAGPLOracleBuildTag(path) {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return out
}

// hasAGPLOracleBuildTag reports whether the Go file at path carries a
// //go:build constraint that includes agpl_oracle. Reads only the preamble
// (lines before the package declaration) to stay cheap.
func hasAGPLOracleBuildTag(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// Scan the preamble: lines before the package declaration.
	for _, line := range strings.SplitN(string(data), "\n", 50) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			break
		}
		if strings.HasPrefix(trimmed, "//go:build ") && strings.Contains(trimmed, "agpl_oracle") {
			return true
		}
	}
	return false
}
