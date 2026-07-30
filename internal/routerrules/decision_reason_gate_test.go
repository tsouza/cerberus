package routerrules

import (
	"context"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/solver"
)

// costDeclinedReasons are the two solver reasons that mean "the plan was
// ELIGIBLE for route B and the cost thresholds declined it". They are the only
// reasons for which route_a_high_fanout_should_shard's advice — lower the
// route-B threshold — can change the outcome.
var costDeclinedReasons = map[string]bool{
	solver.ReasonBelowThreshold: true,
	solver.ReasonHighD:          true,
}

// TestDecisionReasonDomainMatchesSolverVocabulary pins the hand-maintained wire
// contract between the solver's Reason* vocabulary and the decision_reason enum
// domain this package declares. routerrules deliberately imports neither the
// solver nor optcorpus in production code (see the CorpusTableName contract in
// columns.go), so nothing but this test stops the two from drifting: a Reason
// added to the solver but not here is a token no rule can ever name, and a token
// here that the solver never emits is a rule that can never fire.
func TestDecisionReasonDomainMatchesSolverVocabulary(t *testing.T) {
	t.Parallel()

	want := append([]string(nil), solver.Reasons...)
	sort.Strings(want)
	got := EnumDomain("decision_reason")

	if len(got) != len(want) {
		t.Fatalf("decision_reason domain has %d tokens, solver.Reasons has %d\n got: %v\nwant: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decision_reason domain drifted from solver.Reasons at %d: got %q, want %q\n got: %v\nwant: %v",
				i, got[i], want[i], got, want)
		}
	}
}

// TestRouteAHighFanoutShouldShard_OnlyFiresOnCostDeclinedReasons is the
// correctness gate on the rule's advice. Every solver reason gets its own
// route-A class, all at the same well-above-floor fan-out, so the ONLY thing
// separating them is decision_reason. The rule must fire for the two
// cost-declined reasons and stay silent for every structural refusal: route B
// cannot take a structurally refused plan at any threshold, so "consider routing
// to B" there is wrong advice rather than weak advice.
//
// The instant class is the one this matters most for. An instant query's anchor
// grid is a single point, so it can never shard — yet it is route A, and once
// the classifier stamps its real cost grid onto the refusal (rather than four
// zeros) its inner-spine fan-out is a real number that clears any floor.
func TestRouteAHighFanoutShouldShard_OnlyFiresOnCostDeclinedReasons(t *testing.T) {
	t.Parallel()

	// A route-B population is required for fanout_route_b_floor to resolve at
	// all — the param is scoped to route B, and an empty sub-population makes it
	// no-signal, which skips the rule wholesale and would make this test vacuous.
	const (
		shardedFanout = 10  // every route-B row, so the p50 floor is exactly this
		probeFanout   = 100 // every route-A probe row, comfortably above the floor
		rowsPerClass  = 2
	)

	var rows []BenchRow
	for range rowsPerClass {
		rows = append(rows, BenchRow{
			ShapeID: "probe:sharded", Language: "promql", Route: "B", KShards: 4,
			Fanout: shardedFanout, DecisionReason: solver.ReasonRouted, ExitStatus: "ok",
		})
	}
	for _, reason := range solver.Reasons {
		if reason == solver.ReasonRouted {
			continue // routed is a route-B outcome; it cannot be a route-A class
		}
		for range rowsPerClass {
			rows = append(rows, BenchRow{
				ShapeID: "probe:" + reason, Language: "promql", Route: "A",
				Fanout: probeFanout, DecisionReason: reason, ExitStatus: "ok",
			})
		}
	}

	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	report, err := NewEvaluator(cat, evalConfig(), (&BenchCorpus{Rows: rows}).AsCorpusSource()).
		Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	fired := map[string]bool{}
	for _, f := range report.Findings {
		if f.RuleID == "route_a_high_fanout_should_shard" {
			fired[f.GroupKey["shape_id"]] = true
		}
	}

	sawFire := false
	for _, reason := range solver.Reasons {
		if reason == solver.ReasonRouted {
			continue
		}
		got, want := fired["probe:"+reason], costDeclinedReasons[reason]
		switch {
		case want && !got:
			t.Errorf("reason %q: rule did not fire; a plan the solver found eligible and declined on "+
				"cost is exactly the population this rule exists to surface", reason)
		case !want && got:
			t.Errorf("reason %q: rule fired; route B cannot take a structurally refused plan at any "+
				"threshold, so recommending a lower one is wrong advice", reason)
		}
		sawFire = sawFire || got
	}
	if !sawFire {
		t.Fatal("the rule fired on no class at all — the fan-out floor or the corpus shape made this " +
			"test vacuous rather than proving the reason gate")
	}
}
