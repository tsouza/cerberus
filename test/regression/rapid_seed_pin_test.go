package regression

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The coverage ledger (test/coverage-floor/) is a ratchet: it raises a
// package's floor to what a run measured and never lowers one. That only holds
// together while the measurement is reproducible. pgregory.net/rapid defaults
// its PRNG seed to a random value, and which branches of a generator-driven
// test execute moves a package's coverage by whole statements between two runs
// of an identical tree — measured on tsouza/cerberus#3000, where
// test/property/oracle/traceql drew 242, 243, 244 and 245 of its 272
// statements across fifteen full-lane profiles. A floor enrolled from the 245
// draw is one the 242 draw fails, and nothing in a ratchet ever corrects it.
//
// The coverage lanes therefore export CERBERUS_RAPID_SEED, and every test
// binary that links rapid honours it in an init that pins `-rapid.seed`. The
// pin has to be per binary: rapid registers that flag from its own init, so
// only a binary linking rapid has it, and a lane-wide
// `go test -rapid.seed=N ./...` would abort every package that does not.
//
// Per-binary means repeated, and repeated means it can be forgotten. This test
// derives the set that owes a pin from the IMPORT GRAPH rather than from a
// list somebody maintains, so a new rapid-driven test package cannot quietly
// rejoin the unpinned set: it either carries the pin or it fails here.
const (
	rapidImportPath = "pgregory.net/rapid"
	rapidSeedEnvVar = "CERBERUS_RAPID_SEED"
	rapidSeedFlag   = "rapid.seed"
	rapidTreeRoot   = "../.."
)

// rapidSeedSkippedDirs are directories the walk never descends into: build
// output, VCS metadata, agent worktrees (which are whole checkouts of this
// same repository and would double every finding), and node_modules.
var rapidSeedSkippedDirs = map[string]bool{
	".git":         true,
	".claude":      true,
	"bin":          true,
	"node_modules": true,
	"vendor":       true,
}

// rapidLinkingPackageDirs returns every directory holding a Go file that
// imports rapid — the exact set of test binaries whose draws move.
//
// Imports are read with go/parser rather than matched as text, so the import
// path spelled in this file's own constants does not count itself, and a
// mention inside a comment or a string literal never inflates the set.
// Build tags are deliberately NOT applied: a file excluded from today's tag
// set still ships a rapid-driven test, and the pin has to be there when a lane
// does build it.
func rapidLinkingPackageDirs(t *testing.T) []string {
	t.Helper()

	dirs := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(rapidTreeRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rapidSeedSkippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == rapidImportPath {
				dirs[filepath.Dir(path)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s for rapid importers: %v", rapidTreeRoot, err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no package imports %s — this test can no longer see the thing it guards", rapidImportPath)
	}

	out := make([]string, 0, len(dirs))
	for dir := range dirs {
		out = append(out, dir)
	}
	return out
}

// dirPinsRapidSeed reports whether some file in dir installs the pin: it must
// name the environment variable the lanes export AND the flag rapid registers,
// in the same file, because either one alone pins nothing.
func dirPinsRapidSeed(t *testing.T, dir string) bool {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(dir, e.Name()), err)
		}
		text := string(src)
		if strings.Contains(text, `"`+rapidSeedEnvVar+`"`) && strings.Contains(text, `"`+rapidSeedFlag+`"`) {
			return true
		}
	}
	return false
}

// TestRapidSeedPinCoversEveryRapidBinary fails when a package that draws
// through rapid does not honour the coverage lanes' pinned seed.
func TestRapidSeedPinCoversEveryRapidBinary(t *testing.T) {
	t.Parallel()

	var unpinned []string
	for _, dir := range rapidLinkingPackageDirs(t) {
		if !dirPinsRapidSeed(t, dir) {
			unpinned = append(unpinned, dir)
		}
	}
	if len(unpinned) > 0 {
		t.Fatalf("these packages import %s but do not pin its seed, so their coverage moves run to "+
			"run and the floor ledger's ratchet can enrol a floor from a lucky draw: %v\n"+
			"Copy the init from internal/chplan/rapid_seed_test.go into each. See docs/toolchain.md.",
			rapidImportPath, unpinned)
	}
}

// TestRapidSeedPinIsWiredIntoTheCoverageLanes pins the other half: an init
// nothing sets the variable for is an init that never runs. Both coverage
// lanes must export it, and they must export the SAME value — two lanes drawing
// different samples would merge into a profile neither of them measured.
func TestRapidSeedPinIsWiredIntoTheCoverageLanes(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join(rapidTreeRoot, "Justfile"))
	if err != nil {
		t.Fatalf("reading Justfile: %v", err)
	}
	text := string(src)

	if !strings.Contains(text, "COVERAGE_RAPID_SEED := ") {
		t.Fatal("the Justfile no longer defines COVERAGE_RAPID_SEED, so nothing pins the seed the " +
			"coverage lanes measure with")
	}
	for _, recipe := range []string{"coverage-default:", "coverage-chdb:"} {
		body := recipeBody(t, text, recipe)
		if !strings.Contains(body, rapidSeedEnvVar+"={{COVERAGE_RAPID_SEED}}") {
			t.Errorf("recipe %s does not export %s={{COVERAGE_RAPID_SEED}}, so its half of the "+
				"merged profile is still drawn from a random seed", recipe, rapidSeedEnvVar)
		}
	}
}

// recipeBody returns the lines of a Justfile recipe: everything from its
// header to the next line that starts in column zero.
func recipeBody(t *testing.T, justfile, header string) string {
	t.Helper()

	lines := strings.Split(justfile, "\n")
	start := -1
	for i, line := range lines {
		if line == header {
			start = i + 1
			break
		}
	}
	if start == -1 {
		t.Fatalf("recipe %q not found in the Justfile", header)
	}
	var body []string
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}
