package regression

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/routerrules"
)

// exitStatusColumn is the corpus column whose token set the two packages below
// must agree on.
const exitStatusColumn = "exit_status"

// TestCorpusExitStatusLockstep pins the exit_status token set across the two
// packages that hold their own copy of it.
//
// optcorpus WRITES the column; routerrules VALIDATES mining rules against a
// closed domain for it, and deliberately keeps its own copy so it depends on
// neither optcorpus nor chclient (the layering the architecture rules enforce).
// Two independent copies drift in both directions, and both directions are
// silent at runtime:
//
//   - a token optcorpus emits but routerrules lacks makes every mining rule
//     naming it fail catalog validation, so the corpus is written and never
//     queried;
//   - a token routerrules accepts but optcorpus cannot emit makes a rule that
//     validates cleanly match nothing, forever.
//
// This test is the only place the two sets meet.
func TestCorpusExitStatusLockstep(t *testing.T) {
	t.Parallel()

	written := optcorpus.ExitStatusTokens()
	accepted := routerrules.EnumDomain(exitStatusColumn)

	if len(written) == 0 {
		t.Fatal("optcorpus.ExitStatusTokens() is empty; this test would compare nothing")
	}
	if len(accepted) == 0 {
		t.Fatalf("routerrules.EnumDomain(%q) is empty; this test would compare nothing", exitStatusColumn)
	}

	sorted := append([]string(nil), written...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != strings.Join(accepted, ",") {
		t.Errorf("exit_status token sets differ\noptcorpus writes:   %v\nrouterrules accepts: %v",
			sorted, accepted)
	}
}

// TestCorpusExitStatusRulesValidate is the behavioural half of the lockstep: a
// mining rule that compares exit_status against each token optcorpus can write
// must LOAD, exercising the real catalog validator rather than the domain map
// it reads. A token missing from routerrules' domain fails here with the same
// error an operator would see when their rule is rejected at boot.
func TestCorpusExitStatusRulesValidate(t *testing.T) {
	t.Parallel()

	tokens := optcorpus.ExitStatusTokens()
	if len(tokens) == 0 {
		t.Fatal("optcorpus.ExitStatusTokens() is empty; this test would validate nothing")
	}
	for _, token := range tokens {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			if _, err := routerrules.LoadCatalog([]byte(exitStatusRuleCatalog(token))); err != nil {
				t.Errorf("catalog naming exit_status %q must load: %v", token, err)
			}
		})
	}
}

// TestCorpusExitStatusDomainIsClosed pins that the domain rejects a token no
// exit status renders to, so the lockstep above is a real equality check and
// not an artefact of an open domain that accepts anything.
func TestCorpusExitStatusDomainIsClosed(t *testing.T) {
	t.Parallel()

	const notAnExitStatus = "killed"
	for _, token := range optcorpus.ExitStatusTokens() {
		if token == notAnExitStatus {
			t.Fatalf("%q is now a real exit status; pick another token for the closure check", notAnExitStatus)
		}
	}
	if _, err := routerrules.LoadCatalog([]byte(exitStatusRuleCatalog(notAnExitStatus))); err == nil {
		t.Errorf("catalog naming exit_status %q loaded; the domain must be closed", notAnExitStatus)
	}
}

// exitStatusRuleCatalog renders a minimal well-formed catalog whose single rule
// compares exit_status against token.
func exitStatusRuleCatalog(token string) string {
	return fmt.Sprintf(`
apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
rules:
  - id: exit_status_lockstep
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition:
      all:
        - { col: exit_status, op: eq, enum: %s }
    finding: "exit status %s"
`, token, token)
}
