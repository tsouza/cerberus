package regression

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestJustfileNoDoubleDollarShellVar guards against the bug fixed in
// commit 759b05f: in a Just recipe, `$$` is NOT an escape for `$` —
// both `$x` and `$$x` are passed verbatim to bash, where `$$` is the
// PID. The recipe `for f in ...; do echo "$$f"; done` produced output
// like `+ 12521f` (PID concatenated with the literal `f`), and the
// seed step tried to read SQL from a file named `12521f` that doesn't
// exist.
//
// This test scans the Justfile for `$$` followed by a shell-variable
// identifier (not by a digit, `?`, `!`, `$`, `*`, `@`, `#`, or `-` —
// those are legitimate bash special parameters). If found, that's the
// Make-style escape leaking through; should be a single `$`.
func TestJustfileNoDoubleDollarShellVar(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	// `$$` followed by an alphabetic char or underscore = the bug.
	// `$$` followed by `?`/`!`/`*`/`@`/`#`/`-`/`0`-`9` = legitimate
	// bash special variable, leave alone.
	doubleDollarVarRE := regexp.MustCompile(`\$\$[a-zA-Z_]`)

	lines := strings.Split(string(buf), "\n")
	for i, line := range lines {
		// Strip strings inside quotes? Could be a false-positive in
		// comments / docstrings; for now flag everything and let the
		// author add an inline `//justfile-ignore-doubledollar` marker
		// if a legitimate case shows up.
		if strings.Contains(line, "justfile-ignore-doubledollar") {
			continue
		}
		if doubleDollarVarRE.MatchString(line) {
			t.Errorf("Justfile:%d: `$$` followed by a shell-variable identifier — Just does NOT escape $$; bash sees the literal $$ (PID). Use single `$` for shell vars: %s",
				i+1, strings.TrimSpace(line))
		}
	}
}
