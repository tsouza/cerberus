package regression

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/solver"
)

// routerCorpusDir holds every JSONL fixture the routerrules harnesses mine.
// Path is relative to this test package directory.
const routerCorpusDir = "../../internal/routerrules/testdata"

// validRouterDecisionReasons is the closed set of decision_reason tokens the
// production emits. It is derived from the solver vocabulary plus the explicit
// corpus-only non-PromQL reason rather than re-listed here.
func validRouterDecisionReasons() map[string]struct{} {
	m := make(map[string]struct{}, len(solver.Reasons)+1)
	for _, r := range solver.Reasons {
		m[r] = struct{}{}
	}
	m[engine.CorpusReasonNonPromQL] = struct{}{}
	return m
}

// corpusRow is the subset of the corpus schema these invariants constrain. The
// geometry columns are fixed BEFORE dispatch — the solver's own measurements of
// the query shape, plus the concurrency the executor admits over that shape; the
// runtime-cost columns (read_rows, memory_usage, shards_observed, …) are what
// actually happened once the query ran and are therefore unconstrained here.
type corpusRow struct {
	Language       string `json:"language"`
	Route          string `json:"route"`
	DecisionReason string `json:"decision_reason"`
	NAnchors       int64  `json:"n_anchors"`
	Fanout         int64  `json:"fanout"`
	CumulativeD    int64  `json:"cumulative_d"`
	OuterRange     int64  `json:"outer_range"`
	Step           int64  `json:"step"`
	KShards        int64  `json:"k_shards"`
	Parallelism    int64  `json:"parallelism"`
}

// classified reports whether the row carries any solver output at all. The
// corpus-only non-PromQL reason identifies an unclassified row explicitly.
func (r corpusRow) classified() bool {
	return r.Route != "" ||
		(r.DecisionReason != "" && r.DecisionReason != engine.CorpusReasonNonPromQL) ||
		r.geometry() != 0
}

// geometry folds the pre-dispatch columns so "all zero" is one check.
func (r corpusRow) geometry() int64 {
	return r.NAnchors | r.Fanout | r.CumulativeD | r.OuterRange | r.Step | r.KShards | r.Parallelism
}

// TestRouterCorpusFixturesAreProducible pins every routerrules corpus fixture to
// the states production can actually reach. The catalog's failure rules group by
// decision_reason and gate on the geometry columns, so a fixture carrying a
// combination the solver cannot emit makes those rules' FIRE tests pass against
// fiction — and, worse, hides the inverse: a rule that would degenerate on real
// data never gets the chance to show it.
//
// Three invariants, each derived from production code rather than restated:
//
//  1. decision_reason is a solver.Reasons member, the corpus-only non-PromQL
//     reason, or absent on historical rows.
//  2. Only solver.LangPromQL rows carry a classification. solver.Classify is
//     PromQL-gated, so for any other head route is empty, geometry is zero, and
//     the corpus records the non-PromQL reason. A fixture that fakes geometry on
//     a logql row lets a `cumulative_d >= p(cumulative_d)` gate look selective
//     when, on real data, that language's whole population is 0 and the gate
//     matches every failing row it has.
//  3. route == "B" iff decision_reason == solver.ReasonRouted. Strategy is set
//     exactly on a route and a routed decision reports ReasonRouted, so a
//     route-B row cannot carry a refusal reason and a refused row cannot claim
//     route B.
func TestRouterCorpusFixturesAreProducible(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join(routerCorpusDir, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob corpus fixtures: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no corpus fixtures under %s", routerCorpusDir)
	}

	valid := validRouterDecisionReasons()
	var totalClassified, totalUnclassified int
	for _, path := range files {
		name := filepath.Base(path)
		classified, unclassified := checkCorpusFixture(t, path, valid)
		if classified+unclassified == 0 {
			t.Errorf("%s has no rows", name)
		}
		totalClassified += classified
		totalUnclassified += unclassified
	}

	// Both populations must be non-empty across the corpus, or the invariants
	// above are vacuously satisfied: an all-classified corpus is exactly the
	// blind spot that let a geometry gate fire on every LogQL and TraceQL
	// failure, and an all-unclassified one would silence every route-scoped rule.
	if totalClassified == 0 {
		t.Error("no fixture carries a classified row — route-scoped rules are untested")
	}
	if totalUnclassified == 0 {
		t.Error("no fixture carries an unclassified row — the two non-PromQL heads are untested")
	}
}

// checkCorpusFixture asserts the three invariants over one JSONL fixture and
// returns how many rows fell on each side of the classification boundary.
func checkCorpusFixture(t *testing.T, path string, valid map[string]struct{}) (classified, unclassified int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	const maxCorpusLine = 1 << 20
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxCorpusLine)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r corpusRow
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("%s line %d not valid JSON: %v", path, n, err)
		}

		if r.DecisionReason != "" {
			if _, ok := valid[r.DecisionReason]; !ok {
				t.Errorf("%s line %d has decision_reason %q, which is not a production solver Reason* const", path, n, r.DecisionReason)
			}
		}

		if r.classified() {
			classified++
			if r.Language != solver.LangPromQL {
				t.Errorf("%s line %d is %s but carries a classification (route=%q reason=%q geometry=%d); the solver only classifies %s",
					path, n, r.Language, r.Route, r.DecisionReason, r.geometry(), solver.LangPromQL)
			}
		} else {
			unclassified++
		}

		if routed := r.DecisionReason == solver.ReasonRouted; (r.Route == "B") != routed {
			t.Errorf("%s line %d has route=%q with decision_reason=%q; route B and %q are the same event",
				path, n, r.Route, r.DecisionReason, solver.ReasonRouted)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return classified, unclassified
}
