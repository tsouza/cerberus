package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// justfileImportRE matches a native `import "just/<name>.just"` statement in
// the root Justfile (#3093).
var justfileImportRE = regexp.MustCompile(`(?m)^import\s+"([^"]+)"\s*$`)

// justfileSource is one physical file in the Justfile's `import` graph,
// paired with its raw text.
type justfileSource struct {
	File string // repo-relative path, e.g. "Justfile" or "just/test.just"
	Text string
}

// justfileSources reads the root Justfile plus every file it `import`s, as
// raw source text.
//
// This exists for the handful of regression checks that scan for a LITERAL
// SOURCE SHAPE `just --dump --dump-format json` structurally cannot answer:
// a `$$`-typo grep across every line regardless of whether it is inside a
// recipe body, or a check on the exact HEIGHT of the comment block sitting
// directly above a recipe header (dump only ever exposes the collapsed
// last line as a recipe's `doc` — the very collapsing
// TestJustfileRecipeDescriptionsAreSummaries exists to police the shape of).
// Neither has a file:line address or a full-comment-block view in the
// dump's flattened recipe map, so these stay raw-text scans — just spread
// across every file the split (#3093) now uses instead of one hardcoded
// `../../Justfile`, discovered from the Justfile's own `import` lines
// rather than a hardcoded list of the current 14 filenames, so this keeps
// working as the epic's later phases move recipes between files.
//
// Every OTHER regression check that only needs a recipe's body, doc,
// dependencies, or parameters (not a raw line number, not the full
// comment block) should use justDump() instead — see justfile_dump_test.go.
func justfileSources(t *testing.T) []justfileSource {
	t.Helper()

	rootBuf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	rootText := string(rootBuf)

	sources := []justfileSource{{File: "Justfile", Text: rootText}}
	for _, m := range justfileImportRE.FindAllStringSubmatch(rootText, -1) {
		rel := m[1]
		buf, err := os.ReadFile(filepath.Join("../..", rel))
		if err != nil {
			t.Fatalf("read %s (imported by Justfile): %v", rel, err)
		}
		sources = append(sources, justfileSource{File: rel, Text: string(buf)})
	}
	if len(sources) < 2 {
		t.Fatal("Justfile declares no `import \"just/...\"` statements — either the just/*.just split " +
			"(#3093) was reverted, or justfileImportRE stopped matching; justfileSources() would " +
			"otherwise silently scan only the root file and every caller's floor guard would pass " +
			"vacuously over a near-empty set")
	}
	return sources
}
