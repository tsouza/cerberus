package regression

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGremlinsMutatorsAllEnabled pins that every mutator `.gremlins.yaml`
// lists under `mutants:` is `enabled: true`.
//
// Nothing else guards the mutator set. `forbid-contradicted-mutants.mjs`
// reads the block only as a vocabulary, and a change to `.gremlins.yaml`
// selects no mutation phase on a pull request (mutation-matrix.mjs treats
// it as a harness path, so the full sweep runs only after the change lands
// on main). A mutator flipped to `enabled: false` would therefore shrink
// every leg's mutant population, and with it the population the 95% bar is
// measured over, without any PR-time check noticing — the same hollow
// green mutation.yml's own header describes, produced by configuration
// instead of by scope. Pinning the set here keeps that a reviewed source
// change with a failing test attached.
func TestGremlinsMutatorsAllEnabled(t *testing.T) {
	t.Parallel()

	text, err := os.ReadFile(filepath.Join(repoRoot, ".gremlins.yaml"))
	if err != nil {
		t.Fatalf("read .gremlins.yaml: %v", err)
	}
	mutators, err := gremlinsMutatorStates(string(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(mutators) == 0 {
		t.Fatal(".gremlins.yaml lists no mutators under `mutants:`")
	}
	for name, enabled := range mutators {
		if !enabled {
			t.Errorf("mutator %q is not `enabled: true`; a disabled mutator shrinks every leg's population silently", name)
		}
	}
}

// TestGremlinsMutatorStatesReadsEnabledFlag proves the parser above can
// fail: a disabled or flag-less mutator is reported, so the pin is not
// vacuous on the committed file.
func TestGremlinsMutatorStatesReadsEnabledFlag(t *testing.T) {
	t.Parallel()

	states, err := gremlinsMutatorStates("silent: false\nmutants:\n  a-b:\n    enabled: true\n  c-d:\n    enabled: false\n  e-f:\n    other: 1\nunleash:\n  x: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a-b": true, "c-d": false, "e-f": false}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for name, enabled := range want {
		if states[name] != enabled {
			t.Errorf("mutator %q enabled = %t, want %t", name, states[name], enabled)
		}
	}
	if _, err := gremlinsMutatorStates("silent: false\n"); err == nil {
		t.Fatal("a file with no `mutants:` block must be an error, not an empty set")
	}
}

var (
	gremlinsMutatorNameRE  = regexp.MustCompile(`^  ([a-z][a-z0-9-]*):\s*$`)
	gremlinsMutatorFlagRE  = regexp.MustCompile(`^    enabled:\s*(true|false)\s*$`)
	gremlinsMutantsBlockRE = regexp.MustCompile(`^mutants:\s*$`)
)

// gremlinsMutatorStates parses the `mutants:` block of a .gremlins.yaml
// text into mutator name -> enabled. The block's shape is fixed by
// gremlins itself (`mt.String()` with `_` -> `-`, one `enabled:` flag per
// mutator), so a line-oriented read is exact rather than a guess; a
// mutator with no `enabled:` line reads as disabled, which is what
// gremlins does for a mutator that is defaults-off.
func gremlinsMutatorStates(text string) (map[string]bool, error) {
	states := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	inBlock := false
	current := ""
	for scanner.Scan() {
		line := scanner.Text()
		if !inBlock {
			inBlock = gremlinsMutantsBlockRE.MatchString(line)
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break // dedent ends the block
		}
		if m := gremlinsMutatorNameRE.FindStringSubmatch(line); m != nil {
			current = m[1]
			states[current] = false
			continue
		}
		if m := gremlinsMutatorFlagRE.FindStringSubmatch(line); m != nil && current != "" {
			states[current] = m[1] == "true"
		}
	}
	if !inBlock {
		return nil, fmt.Errorf(".gremlins.yaml has no `mutants:` block")
	}
	return states, nil
}
