package regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agplOracleLane is one CI lane that RUNS agpl_oracle-tagged code, and the
// repo-relative directory prefixes (slash-separated) its `go test` scope
// covers.
//
// There is more than one such lane because the tag composes: a file needing
// only the reference parser runs in the tag's own lane, while one that also
// needs the chDB session runs in the chDB lane under the synthetic
// `chdb_agpl_oracle` tag. Modelling a single lane would force a choice
// between a false negative (a genuinely covered file reported uncovered) and
// a false positive (widening one lane's scope to claim files it never runs).
type agplOracleLane struct {
	// workflow is the file whose `go test` invocation these scopes must
	// match, quoted in the failure message so the fix has an address.
	workflow string

	// tags is the build-tag set that lane passes.
	tags string

	// scopes are the repo-relative directory prefixes the lane's `go test`
	// arguments expand to.
	scopes []string
}

// agplOracleLanes must stay in step with the workflows it names. Adding a
// new `//go:build agpl_oracle` (or `*_agpl_oracle`) file outside every scope
// below fails this test: extend the right lane's `go test` invocation AND
// its scopes here.
var agplOracleLanes = []agplOracleLane{
	{
		workflow: ".github/workflows/agpl-oracle.yml",
		tags:     "agpl_oracle",
		scopes: []string{
			"internal/",
			"test/agpl_oracle/",
			"test/spec/parityoracle/",
			"test/surface-parity/",
			"test/property/",
		},
	},
	{
		// The TraceQL parity runner needs the reference engine AND the
		// chDB session that holds the seeded spans, so it runs on the
		// chDB lane's traceql leg rather than in the tag's own lane.
		workflow: ".github/workflows/chdb.yml",
		tags:     "chdb,agpl_oracle,chdb_agpl_oracle",
		scopes: []string{
			"test/spec/",
			"internal/traceql/",
		},
	},
}

// TestAGPLOracleTagSetCoverage asserts that every `//go:build agpl_oracle`
// file in the root module is RUN by some CI lane. This is the "hold the tag
// set to the lanes" regression described in #1610: a new oracle file that
// carries the tag but is covered by no lane's `go test` scope would compile
// under no CI lane, defeating the differential oracle entirely.
//
// The test does NOT run the tagged files itself (it carries no agpl_oracle
// tag) — it only verifies coverage, against the lane inventory above.
func TestAGPLOracleTagSetCoverage(t *testing.T) {
	// Resolve to an absolute path so filepath.Base of the root isn't "..".
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

	tagged := findAGPLOracleFiles(t, absRoot)
	if len(tagged) == 0 {
		t.Fatal("no //go:build agpl_oracle files found in the root module; " +
			"either the tag was removed from all files (update this test) " +
			"or repoRoot is wrong")
	}

	var uncovered []string
	scopeCount := 0
	for _, lane := range agplOracleLanes {
		scopeCount += len(lane.scopes)
	}
	for _, rel := range tagged {
		rel = filepath.ToSlash(rel)
		covered := false
		for _, lane := range agplOracleLanes {
			for _, scope := range lane.scopes {
				if strings.HasPrefix(rel, scope) {
					covered = true
					break
				}
			}
			if covered {
				break
			}
		}
		if !covered {
			uncovered = append(uncovered, rel)
		}
	}

	if len(uncovered) > 0 {
		t.Errorf("%d //go:build agpl_oracle file(s) are RUN by no CI lane:\n", len(uncovered))
		for _, f := range uncovered {
			t.Errorf("  %s", f)
		}
		t.Error("\nPick the lane whose build tags the file needs, extend that lane's " +
			"`go test` invocation, and add the package subtree to its scopes in " +
			"agplOracleLanes. The lanes are:")
		for _, lane := range agplOracleLanes {
			t.Errorf("  %s (-tags %s)", lane.workflow, lane.tags)
		}
	}

	t.Logf("agpl_oracle tag set: %d file(s) across %d scope(s) in %d lane(s) — all covered",
		len(tagged), scopeCount, len(agplOracleLanes))
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
