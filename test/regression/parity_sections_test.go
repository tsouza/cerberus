package regression

import (
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// TestParitySectionsParse (split out of the parity contract in #2194) asserts every fixture carrying a `parity:`
// section has a WELL-FORMED one.
//
// LoadParity deliberately errors rather than skipping on a malformed body,
// because a typo'd key would otherwise un-enrol a fixture from its oracle
// while still looking enrolled in the diff — the fixture would keep its
// `-- parity --` header, keep passing, and check nothing. This test is
// what makes that error reachable in the fast, untagged lane rather than
// only in the chdb-tagged one.
func TestParitySectionsParse(t *testing.T) {
	t.Parallel()

	for _, dir := range parityFixtureDirs(t) {
		for _, path := range txtarFilesForParity(t, dir) {
			c, err := spec.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if _, _, err := spec.LoadParity(c); err != nil {
				t.Errorf("%s: %v", filepath.Base(path), err)
			}
		}
	}
}

// TestParityExemptSectionsParse mirrors TestParitySectionsParse for the
// `parity_exempt:` section: every fixture carrying one must have a
// well-formed one in the fast untagged lane.
func TestParityExemptSectionsParse(t *testing.T) {
	t.Parallel()

	for _, dir := range parityFixtureDirs(t) {
		for _, path := range txtarFilesForParity(t, dir) {
			c, err := spec.Load(path)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if _, _, err := spec.LoadParityExempt(c); err != nil {
				t.Errorf("%s: %v", filepath.Base(path), err)
			}
		}
	}
}
