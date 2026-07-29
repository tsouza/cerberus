package regression

// Pins the Map key-order invariant to a single named function.
//
// Every series-identity key in the engine depends on the attributes Map
// being ordered by key: a CH Map compares, groups and hashes POSITIONALLY
// over its (keys, values) arrays, so one logical series delivered under
// two OTLP key orders otherwise splits into two. Cerberus establishes the
// invariant by wrapping the map in chplan.CanonicalMapFunc, and three
// layers depend on that being ONE function:
//
//   - chplan.CanonicalAttributesExpr skips re-wrapping by matching a
//     FuncCall against the constant, so a site that wrapped with a
//     different function would be re-wrapped rather than recognised;
//   - internal/chsql emits the plan IR's calls verbatim;
//   - internal/api/loki builds the same wrap as a Frag for the metadata
//     endpoints, which never cross the plan IR at all.
//
// A spelled-out literal at any of those sites can drift onto a different
// function while the other layers keep believing the map is canonical —
// and the resulting bug is a silent wrong answer (doubled counts, halved
// volumes, empty range results), not an error. Naming the function once
// is what makes the drift impossible, so this test fails if a literal
// reappears.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// The tree the invariant governs, and the sole file allowed to spell
	// the function name — the one that declares the constant.
	canonicalFuncRoot    = "../../internal"
	canonicalFuncDeclare = "chplan/canonical_attributes.go"

	// The Go string-literal form. Prose mentions in doc comments render
	// the call as `mapSort(...)` without surrounding quotes, so matching
	// the quoted form finds code without flagging documentation.
	canonicalFuncLiteral = `"mapSort"`
)

// TestCanonicalMapFuncHasOneSpelling fails if any non-test file under
// internal/ names the canonical key-order function directly instead of
// referencing chplan.CanonicalMapFunc.
func TestCanonicalMapFuncHasOneSpelling(t *testing.T) {
	declare := filepath.FromSlash(canonicalFuncDeclare)
	var offenders []string

	err := filepath.WalkDir(canonicalFuncRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests legitimately assert on emitted SQL text, where the
		// function name is the expected output rather than a choice of
		// implementation.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(canonicalFuncRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == declare {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(src), canonicalFuncLiteral) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", canonicalFuncRoot, err)
	}

	if len(offenders) > 0 {
		t.Fatalf("these files spell the canonical key-order function as %s instead of "+
			"referencing chplan.CanonicalMapFunc: %s.\n"+
			"The plan IR recognises an already-canonical map by matching that constant, so a "+
			"second spelling lets one layer wrap with a different function while the others "+
			"believe the map is ordered — a silent wrong answer, not an error.",
			canonicalFuncLiteral, strings.Join(offenders, ", "))
	}
}

// TestCanonicalMapFuncConstantIsDeclared fails if the declaring file stops
// carrying the literal, which would mean the constant was renamed or moved
// and TestCanonicalMapFuncHasOneSpelling is now guarding nothing.
func TestCanonicalMapFuncConstantIsDeclared(t *testing.T) {
	path := filepath.Join(canonicalFuncRoot, filepath.FromSlash(canonicalFuncDeclare))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(src), "CanonicalMapFunc = "+canonicalFuncLiteral) {
		t.Fatalf("%s no longer declares CanonicalMapFunc = %s; the single-spelling gate "+
			"has nothing to anchor on and would pass over a tree with no canonical "+
			"function at all", canonicalFuncDeclare, canonicalFuncLiteral)
	}
}
