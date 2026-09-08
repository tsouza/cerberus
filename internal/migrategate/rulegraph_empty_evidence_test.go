package migrategate_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
)

// This pins #3182. The rulegraph stage was the only required stage with no
// zero-evidence guard: evalClassify refuses an empty corpus ("nothing
// classified"), evalVerify refuses an empty replay ("nothing verified"), and
// this stage returned PASS on a graph in which nothing had been harvested at
// all.
//
// It was reachable, not theoretical. promrules.Parse accepted any well-formed
// YAML, so a `--rules` glob matching a prometheus.yml or an empty file produced
// zero recorded series, zero consumers and — because that is neither a
// zero-match glob nor a YAML error — zero skips. The gate then certified
// "nothing must stay materialized after cutover" for a Prometheus whose
// recording rules had never been read.
func TestRuleGraphStageRefusesAnEmptyHarvest(t *testing.T) {
	g := migrate.RuleGraph{Counts: migrate.RuleGraphCounts{}}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: writeArtifact(t, "rulegraph.json", g.WriteJSON),
	}

	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	st := stageByName(t, dec, "rulegraph")
	if st.Verdict != migrategate.VerdictFail {
		t.Errorf("an empty rulegraph must FAIL, got %q (%v)", st.Verdict, st.Reasons)
	}
	if !st.Blocking {
		t.Error("an empty rulegraph must BLOCK: a stage that proved nothing cannot certify a cutover")
	}
	if dec.Pass {
		t.Error("the overall decision must not pass on an empty rulegraph")
	}
	var named bool
	for _, r := range st.Reasons {
		if strings.Contains(r, "nothing graphed") {
			named = true
		}
	}
	if !named {
		t.Errorf("the reason must distinguish an empty harvest from a clean one; got %v", st.Reasons)
	}
}

// The other direction, so the guard cannot be satisfied by failing everything:
// a graph that HARVESTED something and found only orphans still passes. Without
// this, narrowing the guard to "always fail" would leave the case above green.
func TestRuleGraphStageStillPassesOnAHarvestWithOnlyOrphans(t *testing.T) {
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: cleanRuleGraph(t),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if st := stageByName(t, dec, "rulegraph"); st.Verdict != migrategate.VerdictPass || st.Blocking {
		t.Errorf("a real harvest of orphan-only series must PASS; got %q blocking=%v (%v)",
			st.Verdict, st.Blocking, st.Reasons)
	}
}

// A harvest that recorded nothing but DID report skips is already covered by
// the blocking-skip rule; it must stay blocking and must not be mistaken for
// the empty-harvest case, whose message says something different.
func TestRuleGraphStageBlocksOnSkipsWithoutClaimingNothingWasGraphed(t *testing.T) {
	g := migrate.RuleGraph{Counts: migrate.RuleGraphCounts{Skipped: 1}}
	in := migrategate.Inputs{
		Verify:    cleanVerify(t),
		Classify:  cleanClassify(t),
		Inventory: cleanInventory(t),
		RuleGraph: writeArtifact(t, "rulegraph.json", g.WriteJSON),
	}
	dec, err := migrategate.Evaluate(in, migrategate.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	st := stageByName(t, dec, "rulegraph")
	if st.Verdict != migrategate.VerdictFail || !st.Blocking {
		t.Errorf("a skip must still block; got %q blocking=%v", st.Verdict, st.Blocking)
	}
	for _, r := range st.Reasons {
		if strings.Contains(r, "nothing graphed") {
			t.Errorf("a reported skip is evidence, not an empty harvest; got %v", st.Reasons)
		}
	}
}
