package regression

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/routerrules"
	"github.com/tsouza/cerberus/internal/solver"
)

// This pins #1062, the concrete 5-rule ruleset and chDB-parity harness.
//
// routerCorpusDir holds every JSONL fixture the routerrules harnesses mine.
// Path is relative to this test package directory.
const routerCorpusDir = "../../internal/routerrules/testdata"

// validRouterDecisionReasons is the closed set of decision_reason tokens
// production can emit: the solver's own Reason* vocabulary plus the corpus-only
// non-PromQL token the engine stamps on an unclassified head. Both halves are
// derived from the production consts rather than re-listed here — a hand-copied
// list only catches a RENAME (as a compile break) and silently misses an
// ADDITION, which is how this set drifted a Reason behind the solver once
// already.
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

// classified reports whether the row carries any solver OUTPUT at all.
//
// The non-PromQL reason is explicitly not such output: it is the corpus's way of
// saying "no solver ran here", so counting it as a classification would invert
// its meaning and make every LogQL / TraceQL row fail the PromQL-only invariant
// below. It is the one decision_reason value that leaves a row unclassified.
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
//     reason, or absent.
//
//  2. Only solver.LangPromQL rows carry a classification. solver.Classify is
//     PromQL-gated, so for any other head route is empty and every geometry
//     column is the zero value. A fixture that fakes geometry on a logql row
//     lets a `cumulative_d >= p(cumulative_d)` gate look selective when, on real
//     data, that language's whole population is 0 and the gate matches every
//     failing row it has.
//
//     The non-PromQL reason is ADMITTED here, not required: production now
//     stamps it on every such row, but these fixtures predate that and carry an
//     absent reason instead. Both are unclassified, and the classification
//     boundary is what this invariant guards — so the reason is excluded from
//     `classified()` rather than mandated, which keeps the fixtures honest about
//     their own vintage instead of asserting a shape they do not have.
//
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
				t.Errorf("%s line %d has decision_reason %q, which is no token production can emit", path, n, r.DecisionReason)
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

// TestRouterBenchCorpusIsProducible extends the reachable-states pin above from
// the JSONL fixtures to the GENERATED benchmark corpus, which
// TestRouterCorpusFixturesAreProducible never saw: it globs *.jsonl, and the
// benchmark corpus is built in Go by routerrules.GenerateBenchCorpus and handed
// straight to the evaluator through the in-memory seam, never touching a file.
//
// That gap is not hypothetical. The benchmark corpus used to plant
// decision_reason tokens no production path can emit — "sliceable" on its
// route-B rows and "high-cardinality" on its route-A OOM and timeout rows —
// neither of which is a solver.Reasons member. Three catalog rules group by
// decision_reason, so the regression floors, the sweep and the ClickHouse
// parity lane all scored a corpus whose reason column was fiction, and the
// route-B rows additionally claimed a refusal reason while carrying route B.
//
// All three invariants documented on TestRouterCorpusFixturesAreProducible are
// asserted here, every one of them derived from the production consts rather
// than restated:
//
//  1. decision_reason is a solver.Reasons member or the corpus-only non-PromQL
//     token.
//
//  2. only solver.LangPromQL rows carry a classification, and a row on any
//     other head names its own absence with the non-PromQL reason. The
//     generated corpus is stricter than the JSONL fixtures on that second half:
//     the fixtures predate the token and are allowed to carry an absent reason
//     instead, while the generator writes every row itself and so has no vintage
//     to be honest about.
//
//  3. route == "B" iff decision_reason == solver.ReasonRouted.
//
// Invariant 2 reaches the benchmark corpus through the same corpusRow helpers
// the fixture check uses, so "classified" means one thing in this file. A
// BenchRow's geometry is float64 where the fixture's is int64 — the generator
// draws integer-valued jitter into the same UInt32 corpus columns — so the
// conversion is exact and a fabricated non-zero cannot round itself away.
func TestRouterBenchCorpusIsProducible(t *testing.T) {
	t.Parallel()

	corpus := routerrules.GenerateBenchCorpus(routerrules.BenchParams{})
	if len(corpus.Rows) == 0 {
		t.Fatal("benchmark corpus generated no rows — this pin would assert nothing")
	}
	if len(corpus.Classes) == 0 {
		t.Fatal("benchmark corpus generated no labeled classes — this pin would assert nothing")
	}

	valid := validRouterDecisionReasons()

	// Both sides of invariant 3 must be exercised, or a corpus that happened to
	// contain only route-A rows would satisfy it vacuously; and both sides of
	// the classification boundary must be populated, or invariant 2 is satisfied
	// by a corpus that simply has no non-PromQL rows to get wrong.
	var routeB, routeA, classified, unclassified int
	for i, r := range corpus.Rows {
		if _, ok := valid[r.DecisionReason]; !ok {
			t.Errorf("bench row %d (%s/%s) has decision_reason %q, which is no token production can emit",
				i, r.ShapeID, r.Language, r.DecisionReason)
		}
		if routed := r.DecisionReason == solver.ReasonRouted; (r.Route == "B") != routed {
			t.Errorf("bench row %d (%s/%s) has route=%q with decision_reason=%q; route B and %q are the same event",
				i, r.ShapeID, r.Language, r.Route, r.DecisionReason, solver.ReasonRouted)
		}
		if row := benchCorpusRow(r); row.classified() {
			classified++
			if r.Language != solver.LangPromQL {
				t.Errorf("bench row %d (%s) is %s but carries a classification (route=%q reason=%q geometry=%d); the solver only classifies %s",
					i, r.ShapeID, r.Language, r.Route, r.DecisionReason, row.geometry(), solver.LangPromQL)
			}
		} else {
			unclassified++
			if r.Language != solver.LangPromQL && r.DecisionReason != engine.CorpusReasonNonPromQL {
				t.Errorf("bench row %d (%s) is %s and unclassified but its decision_reason is %q, not %q; production names that absence rather than leaving it blank",
					i, r.ShapeID, r.Language, r.DecisionReason, engine.CorpusReasonNonPromQL)
			}
		}
		switch r.Route {
		case "B":
			routeB++
		case "A":
			routeA++
		}
	}
	if routeB == 0 {
		t.Error("no route-B bench row — the route-B half of the routed-reason invariant is untested")
	}
	if routeA == 0 {
		t.Error("no route-A bench row — the refusal half of the routed-reason invariant is untested")
	}
	if classified == 0 {
		t.Error("no classified bench row — the route-scoped rules score against nothing")
	}
	if unclassified == 0 {
		t.Error("no unclassified bench row — the two non-PromQL heads are absent from the benchmark, so invariant 2 holds vacuously")
	}

	// The labeled classes carry the same classification boundary, and a rule is
	// matched back to a class by decision_reason: a non-PromQL class labeled with
	// a solver reason would score findings against a class no row belongs to.
	for i, c := range corpus.Classes {
		if c.Language != solver.LangPromQL && c.DecisionReason != engine.CorpusReasonNonPromQL {
			t.Errorf("bench class %d (%s/%s) has decision_reason %q; a head the solver never classifies carries %q",
				i, c.ShapeID, c.Language, c.DecisionReason, engine.CorpusReasonNonPromQL)
		}
	}

	// The labeled ground truth carries decision_reason too, and a finding is
	// matched back to its class by that column: a class labeled with an
	// unproducible reason scores rules against a class no row can belong to.
	for i, c := range corpus.Classes {
		if c.DecisionReason == "" {
			continue // a class may group on shape_id alone
		}
		if _, ok := valid[c.DecisionReason]; !ok {
			t.Errorf("bench class %d (%s/%s) has decision_reason %q, which is no token production can emit",
				i, c.ShapeID, c.Language, c.DecisionReason)
		}
	}
}

// benchCorpusRow projects a generated BenchRow onto the corpusRow shape the
// fixture invariants are written against, so both populations are judged by one
// definition of "classified" rather than by two that can drift.
func benchCorpusRow(r routerrules.BenchRow) corpusRow {
	return corpusRow{
		Language:       r.Language,
		Route:          r.Route,
		DecisionReason: r.DecisionReason,
		NAnchors:       int64(r.NAnchors),
		Fanout:         int64(r.Fanout),
		CumulativeD:    int64(r.CumulativeD),
		OuterRange:     int64(r.OuterRange),
		Step:           int64(r.Step),
		KShards:        int64(r.KShards),
		Parallelism:    int64(r.Parallelism),
	}
}
