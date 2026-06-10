// Package spec is the cerberus spec-test harness.
//
// Spec tests are TXTAR fixtures (one file per case) under
// test/spec/<head>/*.txtar. Each file has named sections written by the
// runner. Setting GOLDEN_UPDATE=1 in the env rewrites expected sections
// from the values the test produced; review the diff before committing.
package spec

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/txtar"
)

// envGoldenUpdate is the environment variable name that enables in-place
// regeneration of the expected sections in TXTAR fixtures.
const envGoldenUpdate = "GOLDEN_UPDATE"

// Case is a single TXTAR fixture loaded from disk.
type Case struct {
	// Path is the absolute path of the fixture file.
	Path string

	// Name is the basename without the .txtar suffix — used as the test name.
	Name string

	archive *txtar.Archive
}

// Sections returns a map of section name → content. Mutating the returned
// map does not affect the underlying archive.
func (c *Case) Sections() map[string]string {
	out := make(map[string]string, len(c.archive.Files))
	for _, f := range c.archive.Files {
		out[f.Name] = string(f.Data)
	}
	return out
}

// Section returns the content of the named section, or "" + false if absent.
func (c *Case) Section(name string) (string, bool) {
	for _, f := range c.archive.Files {
		if f.Name == name {
			return string(f.Data), true
		}
	}
	return "", false
}

// Load reads and parses a single TXTAR file at path.
func Load(path string) (*Case, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path supplied by tests
	if err != nil {
		return nil, err
	}
	a := txtar.Parse(data)
	name := strings.TrimSuffix(filepath.Base(path), ".txtar")
	return &Case{Path: path, Name: name, archive: a}, nil
}

// Walk loads every *.txtar fixture under dir (non-recursive) and calls fn
// inside a t.Run subtest named after each fixture. Fixtures are visited in
// sorted order for stable output.
func Walk(t *testing.T, dir string, fn func(t *testing.T, c *Case)) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.txtar"))
	if err != nil {
		t.Fatalf("spec.Walk: glob %q: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("spec.Walk: no *.txtar fixtures in %s", dir)
	}
	sort.Strings(matches)
	for _, m := range matches {
		c, err := Load(m)
		if err != nil {
			t.Fatalf("spec.Walk: load %s: %v", m, err)
		}
		t.Run(c.Name, func(t *testing.T) {
			fn(t, c)
		})
	}
}

// Match asserts that each (section, actual) pair matches what's stored on
// disk in c. When GOLDEN_UPDATE=1 is set, mismatches rewrite the section
// in-place instead of failing — so the dev flow is:
//
//	just update-golden                   # regenerate fixtures (both lanes)
//	git diff                             # review the new expected output
//	git add test/spec && git commit ...
//
// Mismatches without GOLDEN_UPDATE call t.Errorf with a unified-style diff
// hint and a `git diff` command to inspect.
func Match(t *testing.T, c *Case, actual map[string]string) {
	t.Helper()

	if os.Getenv(envGoldenUpdate) == "1" {
		updateGolden(t, c, actual)
		return
	}

	// Compare each section.
	names := make([]string, 0, len(actual))
	for n := range actual {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		got := actual[name]
		want, ok := c.Section(name)
		if !ok {
			t.Errorf("[%s] missing section %q in fixture %s (rerun with %s=1 to create it)",
				c.Name, name, c.Path, envGoldenUpdate)
			continue
		}
		if normalize(got) != normalize(want) {
			t.Errorf("[%s] section %q mismatch in %s\n--- want ---\n%s\n--- got ---\n%s\n(rerun with %s=1 to update)",
				c.Name, name, c.Path, want, got, envGoldenUpdate)
		}
	}
}

// normalize collapses trailing whitespace per line + a trailing newline so
// minor formatting drift doesn't trip equality.
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

func updateGolden(t *testing.T, c *Case, actual map[string]string) {
	t.Helper()

	// Build a new archive: keep any non-actual sections as-is, replace
	// actual ones in place, append new ones at the end in sorted order.
	seen := map[string]bool{}
	files := make([]txtar.File, 0, len(c.archive.Files)+len(actual))
	for _, f := range c.archive.Files {
		if v, ok := actual[f.Name]; ok {
			files = append(files, txtar.File{Name: f.Name, Data: ensureTrailingNL([]byte(v))})
			seen[f.Name] = true
		} else {
			files = append(files, f)
		}
	}
	// Append new sections in sorted order for determinism.
	newNames := make([]string, 0)
	for name := range actual {
		if !seen[name] {
			newNames = append(newNames, name)
		}
	}
	sort.Strings(newNames)
	for _, name := range newNames {
		files = append(files, txtar.File{Name: name, Data: ensureTrailingNL([]byte(actual[name]))})
	}

	newArchive := &txtar.Archive{Comment: c.archive.Comment, Files: files}
	if err := os.WriteFile(c.Path, txtar.Format(newArchive), 0o644); err != nil { //nolint:gosec // golden update only runs locally
		t.Fatalf("spec.updateGolden: write %s: %v", c.Path, err)
	}
}

func ensureTrailingNL(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return append(b, '\n')
	}
	return b
}
