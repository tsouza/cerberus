package regression

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/solver"
)

// routerCorpusSeed is the fixture the routerrules default-lane harness mines.
// Path is relative to this test package directory.
const routerCorpusSeed = "../../internal/routerrules/testdata/seed.jsonl"

// validRouterDecisionReasons is the closed set of decision_reason tokens the
// production solver actually emits. It is derived from solver.Reasons rather
// than re-listed here: a hand-copied list only catches a RENAME (as a compile
// break) and silently misses an ADDITION, which is how this set drifted a
// Reason behind the solver once already.
func validRouterDecisionReasons() map[string]struct{} {
	m := make(map[string]struct{}, len(solver.Reasons))
	for _, r := range solver.Reasons {
		m[r] = struct{}{}
	}
	return m
}

// TestRouterCorpusSeedUsesRealDecisionReasons pins the routerrules seed corpus
// to the production solver's decision-reason vocabulary. The catalogVersion-2
// failure rules group by decision_reason, so a seed that carries fabricated
// tokens (the original below_threshold / high_fanout / manual underscore set)
// would make those rules' FIRE tests pass against fiction that never appears in
// a real corpus. This meta-test makes that drift a CI failure: every
// decision_reason in the seed must be a token the solver can actually emit.
func TestRouterCorpusSeedUsesRealDecisionReasons(t *testing.T) {
	t.Parallel()
	valid := validRouterDecisionReasons()

	f, err := os.Open(routerCorpusSeed)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	defer func() { _ = f.Close() }()

	type row struct {
		DecisionReason string `json:"decision_reason"`
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n, checked := 0, 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r row
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("seed line %d not valid JSON: %v", n, err)
		}
		if r.DecisionReason == "" {
			continue // a row may legitimately omit the field
		}
		checked++
		if _, ok := valid[r.DecisionReason]; !ok {
			t.Errorf("seed line %d has decision_reason %q, which is not a production solver Reason* const", n, r.DecisionReason)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan seed: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected at least one seed row carrying a decision_reason")
	}
}
