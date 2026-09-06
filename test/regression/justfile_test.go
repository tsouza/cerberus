package regression

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// TestJustfileNoDoubleDollarShellVar guards against the bug fixed in
// commit 759b05f: in a Just recipe, `$$` is NOT an escape for `$` —
// both `$x` and `$$x` are passed verbatim to bash, where `$$` is the
// PID. The recipe `for f in ...; do echo "$$f"; done` produced output
// like `+ 12521f` (PID concatenated with the literal `f`), and the
// seed step tried to read SQL from a file named `12521f` that doesn't
// exist.
//
// This test scans every just/*.just file (plus the root Justfile) for `$$`
// followed by a shell-variable identifier (not by a digit, `?`, `!`, `$`,
// `*`, `@`, `#`, or `-` — those are legitimate bash special parameters). If
// found, that's the Make-style escape leaking through; should be a single
// `$`. Reads via justfileSources() (#3093), not a single hardcoded
// `../../Justfile`, since recipe bodies now live across 14 files — this scan
// needs the raw source text (for a real file:line address), which
// `just --dump` does not carry at all, so it cannot move to justDump().
func TestJustfileNoDoubleDollarShellVar(t *testing.T) {
	t.Parallel()

	// `$$` followed by an alphabetic char or underscore = the bug.
	// `$$` followed by `?`/`!`/`*`/`@`/`#`/`-`/`0`-`9` = legitimate
	// bash special variable, leave alone.
	doubleDollarVarRE := regexp.MustCompile(`\$\$[a-zA-Z_]`)

	for _, src := range justfileSources(t) {
		lines := strings.Split(src.Text, "\n")
		for i, line := range lines {
			// Strip strings inside quotes? Could be a false-positive in
			// comments / docstrings; for now flag everything and let the
			// author add an inline `//justfile-ignore-doubledollar` marker
			// if a legitimate case shows up.
			if strings.Contains(line, "justfile-ignore-doubledollar") {
				continue
			}
			if doubleDollarVarRE.MatchString(line) {
				t.Errorf("%s:%d: `$$` followed by a shell-variable identifier — Just does NOT escape $$; bash sees the literal $$ (PID). Use single `$` for shell vars: %s",
					src.File, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestJustfileTestRecipeIncludesChaosSleep pins that the `test` composite
// recipe still chains test-unit, test-chaos-sleep and vet-tagged. Reads via
// justDump() (#3093): a recipe's dependency list is exactly what the dump
// exposes, regardless of which just/*.just file declares `test:` today.
func TestJustfileTestRecipeIncludesChaosSleep(t *testing.T) {
	t.Parallel()

	got := justDump(t).recipe(t, "test").dependencyNames()
	want := []string{"test-unit", "test-chaos-sleep", "vet-tagged"}
	if len(got) != len(want) {
		t.Fatalf("the `test` recipe must compose exactly %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the `test` recipe must compose %v in order, got %v", want, got)
		}
	}
}

// justRecipeRE matches a Justfile recipe header: a name at column 0, optional
// parameters, then the `:` that opens the body or the dependency list. The
// negative lookahead `just` itself lacks is spelled here as an explicit check
// on the byte after the colon, which is what keeps `NAME := value` assignments
// and `NAME ::= value` out of the recipe set.
var justRecipeRE = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9_-]*)(\s+[^:]*)?:`)

// minJustfileRecipes guards this test against passing vacuously. If a Justfile
// syntax change (or an edit to justRecipeRE) stopped the parser from matching
// recipe headers, every assertion below would iterate over nothing and report
// success. The tree has had well over a hundred recipes for the whole life of
// this file; a parse that finds fewer than this is a broken parser, not a
// shrunken Justfile.
const minJustfileRecipes = 100

// TestJustfileRecipeDescriptionsAreSummaries pins the rule that makes
// `just --list` readable, closing tsouza/cerberus#3003.
//
// `just` renders a recipe's description from the contiguous `#` block
// immediately above it — and renders ONLY THE LAST LINE of that block. Recipes
// here carry long rationale comments, which is the right thing for a reader of
// the file and the wrong thing for that renderer: the description became
// whichever clause the rationale happened to end on. `coverage` listed as
// "process."; `e2e-run` as "these."; 95 of 117 recipes rendered a fragment.
//
// The shape that satisfies both readers is a blank line: rationale block, blank
// line, then ONE deliberate summary line directly above the recipe. The blank
// line ends the doc comment, so the rationale keeps every byte it had while
// `just --list` shows the summary.
//
// So the rule this asserts is structural, not stylistic: the comment block
// immediately above a listed recipe is exactly one line, and that line reads as
// a written summary (opens upper-case, closes with a period) rather than the
// tail of an argument. A lower-case fragment that happens to sit alone above a
// recipe — the pre-#3003 `# go mod tidy.` on `deps-tidy` — fails the second
// half even though it passes the first.
//
// Private recipes (leading `_`) are exempt: `just --list` never shows them, so
// their comment block has no renderer to satisfy.
//
// Reads via justfileSources() (#3093), one file at a time — a comment block's
// height is a per-file, per-line-number question `just --dump` cannot answer
// at all (it collapses any preceding comment down to a single `doc` string),
// and a block must never be measured across a FILE boundary (the last line of
// tools.just is not a comment above the first recipe of build.just).
func TestJustfileRecipeDescriptionsAreSummaries(t *testing.T) {
	t.Parallel()

	var listed int
	for _, src := range justfileSources(t) {
		lines := strings.Split(src.Text, "\n")
		for i, line := range lines {
			m := justRecipeRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// `NAME := value` / `NAME ::= value` are assignments, not recipes.
			if rest := line[len(m[0]):]; strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ":") {
				continue
			}
			name := m[1]
			if strings.HasPrefix(name, "_") {
				continue // private: never rendered by `just --list`
			}
			listed++

			start := i
			for start > 0 && strings.HasPrefix(lines[start-1], "#") {
				start--
			}
			switch block := i - start; {
			case block == 0:
				t.Errorf("%s:%d: recipe %q has no comment above it, so `just --list` shows no description at all. Add one summary line directly above the recipe.",
					src.File, i+1, name)
				continue
			case block > 1:
				t.Errorf("%s:%d: recipe %q has a %d-line comment block, so `just --list` renders only its last line (%q) as the description. Keep the rationale, but put a blank line after it and one summary line directly above the recipe.",
					src.File, i+1, name, block, strings.TrimPrefix(lines[i-1], "# "))
				continue
			}

			desc := strings.TrimSpace(strings.TrimPrefix(lines[i-1], "#"))
			if desc == "" || !unicode.IsUpper([]rune(desc)[0]) {
				t.Errorf("%s:%d: recipe %q's description %q does not open upper-case — that is the signature of a rationale tail rather than a written summary.",
					src.File, i+1, name, desc)
			}
			if !strings.HasSuffix(desc, ".") {
				t.Errorf("%s:%d: recipe %q's description %q does not end in a period; say what the recipe does in one complete sentence.",
					src.File, i+1, name, desc)
			}
		}
	}

	if listed < minJustfileRecipes {
		t.Fatalf("parsed only %d listed recipes across every just/*.just file, below the %d floor — justRecipeRE has stopped matching recipe headers, so every assertion above ran over an empty set",
			listed, minJustfileRecipes)
	}
}
