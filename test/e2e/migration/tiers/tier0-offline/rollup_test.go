//go:build migration

package tier0

import (
	"path/filepath"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/steps"
)

// gateNoGoExit is the status `cerberus migrate gate` leaves with when a
// blocking stage says no-go. Every Tier-0 fold runs without parity evidence, so
// every archetype must refuse — with THIS status, not the generic error status a
// crash would produce.
const gateNoGoExit = 3

// gateStages is the number of stages the gate folds. A decision that recorded
// fewer would have aborted partway, leaving the operator without the rest of the
// checklist.
const gateStages = 4

// TestTier0GateFold is the whole-run aggregator: it folds EVERY committed
// archetype's offline artifacts into one cutover decision and pins that
// decision against the archetype's committed golden. It is a plain Go test
// rather than a scenario because it belongs to no migration story — it exists
// so no committed fixture goes unasserted, and so the reason each archetype's
// gate objects (a consumed recorded series that must stay materialised, an
// unsupported dialect construct, an input the graph could not read) is pinned
// per archetype by value rather than described in prose.
func TestTier0GateFold(t *testing.T) {
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	bin, err := lib.BuildCerberus(root, t.TempDir())
	if err != nil {
		t.Fatalf("build the cerberus binary the fold drives: %v", err)
	}
	archetypes, err := steps.CommittedArchetypes(root)
	if err != nil {
		t.Fatalf("list the committed archetypes: %v", err)
	}

	work := t.TempDir()
	for _, a := range archetypes {
		t.Run(a, func(t *testing.T) {
			fold, err := steps.FoldGate(root, bin, filepath.Join(work, a), a)
			if err != nil {
				t.Fatalf("fold %s: %v", a, err)
			}
			assertRefused(t, fold)
			assertBlockersAreExplained(t, fold)
			if err := lib.AssertGolden(steps.GoldenGatePath(root, a), fold.Raw); err != nil {
				t.Error(err)
			}
		})
	}
}

// assertRefused pins the outcome every offline fold must reach: no parity
// evidence exists at Tier 0, and parity is a required stage, so the gate refuses
// with its documented status and records the full checklist while doing it.
func assertRefused(t *testing.T, fold steps.GateFold) {
	t.Helper()
	if fold.Decision.Pass {
		t.Errorf("%s passed the cutover with no parity evidence", fold.Archetype)
	}
	if fold.Decision.Overall != migrategate.OverallFail {
		t.Errorf("%s reports %q, want %q", fold.Archetype, fold.Decision.Overall, migrategate.OverallFail)
	}
	if fold.ExitCode != gateNoGoExit {
		t.Errorf("%s left with status %d, want the documented no-go status %d",
			fold.Archetype, fold.ExitCode, gateNoGoExit)
	}
	if len(fold.Decision.Stages) != gateStages {
		t.Errorf("%s records %d stages, want %d", fold.Archetype, len(fold.Decision.Stages), gateStages)
	}
	var sawVerify bool
	for _, m := range fold.Decision.Missing {
		if m == migrategate.StageVerify {
			sawVerify = true
		}
	}
	if !sawVerify {
		t.Errorf("%s lists %v as missing without naming %q",
			fold.Archetype, fold.Decision.Missing, migrategate.StageVerify)
	}
}

// assertBlockersAreExplained pins WHY each stage objects, per archetype, against
// the artifacts the same fold produced. A blocking stage whose reasons are empty
// tells the operator to stop without telling them what to fix; a stage that
// blocks while its own artifact reports nothing to object to would mean the gate
// and the artifact disagree.
func assertBlockersAreExplained(t *testing.T, fold steps.GateFold) {
	t.Helper()
	for _, s := range fold.Decision.Stages {
		if !s.Blocking {
			continue
		}
		if len(s.Reasons) == 0 {
			t.Errorf("%s: stage %q blocks without stating a reason", fold.Archetype, s.Stage)
		}
		switch s.Stage {
		case migrategate.StageVerify:
			if s.Present {
				t.Errorf("%s: the parity stage blocks while reporting an artifact was supplied", fold.Archetype)
			}
		case migrategate.StageClassify:
			if fold.Classify.Counts.Unsupported == 0 {
				t.Errorf("%s: classify blocks while its ledger reports no unsupported query", fold.Archetype)
			}
		case migrategate.StageRuleGraph:
			if fold.RuleGraph.Counts.Consumed == 0 && fold.RuleGraph.Counts.Skipped == 0 {
				t.Errorf("%s: the rule graph blocks while reporting no consumed series and no unusable input",
					fold.Archetype)
			}
		case migrategate.StageInventory:
			t.Errorf("%s: the advisory inventory stage must never block", fold.Archetype)
		default:
			t.Errorf("%s: unknown stage %q", fold.Archetype, s.Stage)
		}
	}
	// The rule graph's own arithmetic has to hold for the blocker above to mean
	// anything: a consumed series is one that MUST keep being materialised.
	g := fold.RuleGraph
	if g.Counts.Consumed+g.Counts.Orphan != g.Counts.Recorded {
		t.Errorf("%s: %d consumed + %d orphan does not equal %d recorded",
			fold.Archetype, g.Counts.Consumed, g.Counts.Orphan, g.Counts.Recorded)
	}
	for _, n := range g.Recorded {
		if n.Status == migrate.StatusConsumed && len(n.Consumers) == 0 {
			t.Errorf("%s: consumed series %q lists no consumer", fold.Archetype, n.Name)
		}
	}
}
