package optimizer_test

// Joint fixpoint of Default() (Layer 4 § C, companion to termination_test.go).
//
// termination_test.go pins each rule's idempotence on its OWN output —
// the property a FixedPoint batch needs to converge. That is necessary
// and not sufficient. The pipeline-level property is stronger:
//
//	nothing Default() registers can still rewrite Default()'s output.
//
// It has to be asserted separately because the two can come apart. Every
// rule can be idempotent on its own output while the batch SEQUENCE
// still leaves work on the table: a rule in an early batch reaches its
// fixpoint, a later batch then CONSTRUCTS the shape it matches, and the
// early batch never runs again. That is exactly what shipped before
// #3507 — ConstantFoldHeuristic ran once before predicate pushdown, and
// FilterFusion's `Filter(Filter(X, p1), true)` → `p1 AND true` handed it
// a foldable Binary it could no longer see, so `WHERE p1 AND true`
// reached the emitter.
//
// Nothing downstream absorbs such a residue: each of the three HTTP
// handlers calls optimizer.Default() exactly once per request, so
// whatever the single call leaves behind is what the chsql emitter
// renders.
//
// Corpus. The 28 pairPlans shapes from rule_interaction_test.go, reused
// rather than duplicated — each was already built to make a specific
// PAIR of rules applicable, which makes the set collectively the
// densest rule-interaction corpus in the package and exactly the set a
// joint-fixpoint claim wants to be checked over. A new rule pair adds a
// pairPlans entry (TestRuleInteractionMatrix fails otherwise), and that
// entry is then checked here too.
//
// Probing is done through the production primitives — an
// optimizer.Batch with optimizer.Once() run by a real Driver — so the
// probe walks the tree exactly the way runBatch does in production
// rather than through a test-only traversal that could disagree with it.

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestDefaultOutputIsJointFixpoint asserts, for every pairPlans shape,
// that no tree-rewriting rule Default() registers reports a change when
// applied once more to Default()'s output.
//
// The verify-only rules are out of scope for the same reason
// rule_interaction_test.go excludes them: neither can return
// changed=true, so an assertion over one could not fail. matrixRules()
// is the shared list, and TestRuleInteractionMatrixCoversDefaultRules
// already pins it against the live Default() registration — so a rule
// added to production without being enrolled fails there, and is
// covered here automatically once enrolled.
func TestDefaultOutputIsJointFixpoint(t *testing.T) {
	t.Parallel()

	for key, build := range pairPlans {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			out := optimizer.Default().Run(ctx, build())

			for _, rule := range matrixRules() {
				probe := &firingRule{inner: rule}
				again := optimizer.NewWithBatches(optimizer.Batch{
					Name:     "joint-fixpoint-probe",
					Strategy: optimizer.Once(),
					Rules:    []optimizer.Rule{probe},
				}).Run(ctx, out)

				if probe.fired > 0 {
					t.Errorf("rule %q still rewrites Default()'s own output (%d node(s)) — "+
						"Default() is not a joint fixpoint of its rules, so the un-rewritten "+
						"shape reaches the emitter\n--- Default() output ---\n%#v\n--- after %s ---\n%#v",
						rule.Name(), probe.fired, out, rule.Name(), again)
					continue
				}
				// A rule that mutates the tree while reporting changed=false is
				// the same defect wearing a different hat: the fired counter
				// above would miss it, and a FixedPoint batch would stop
				// iterating on a tree that is still moving.
				if !again.Equal(out) {
					t.Errorf("rule %q reported no change but returned a different tree — "+
						"the changed flag is lying\n--- before ---\n%#v\n--- after ---\n%#v",
						rule.Name(), out, again)
				}
			}
		})
	}
}

// TestDefaultIsIdempotent asserts the pipeline-level consequence of the
// property above: a second Default() over the first Default()'s output
// is a no-op. Asserted independently of TestDefaultOutputIsJointFixpoint
// because the two can diverge — the per-rule probe applies each rule in
// isolation, while a second full run also re-runs the analyzer batches
// and lets the rules re-arm each other across batch boundaries, which is
// what would surface a residue only a multi-rule sequence can reach.
func TestDefaultIsIdempotent(t *testing.T) {
	t.Parallel()

	for key, build := range pairPlans {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			once := optimizer.Default().Run(ctx, build())
			twice := optimizer.Default().Run(ctx, once)
			if !twice.Equal(once) {
				t.Fatalf("Default() is not idempotent on its own output — the second run "+
					"still found work, which the single per-request call never performs"+
					"\n--- Default(plan) ---\n%#v\n--- Default(Default(plan)) ---\n%#v", once, twice)
			}
		})
	}
}

// TestDefaultCollapsesFusionIntroducedLiteral pins the concrete shape
// #3507 reported, at the granularity a reader can check by eye:
// `Filter(Filter(Scan, p1), 1=1)` must optimize to `Filter(Scan, p1)`,
// with the fusion-introduced `AND true` gone rather than merely
// harmless. The joint-fixpoint test above would also catch a regression
// here, but through a generic "some rule still fires" failure; this one
// names the shape.
func TestDefaultCollapsesFusionIntroducedLiteral(t *testing.T) {
	t.Parallel()

	build, ok := pairPlans[pairKey(ruleFoldSemantic, ruleFilterFusion)]
	if !ok {
		t.Fatalf("pairPlans lost its %q entry — this test needs that exact shape",
			pairKey(ruleFoldSemantic, ruleFilterFusion))
	}

	out := optimizer.Default().Run(context.Background(), build())

	f, isFilter := out.(*chplan.Filter)
	if !isFilter {
		t.Fatalf("expected a single Filter at the root after fusion, got %T", out)
	}
	if _, isScan := f.Input.(*chplan.Scan); !isScan {
		t.Fatalf("expected Filter(Scan) after fusion, got Filter(%T)", f.Input)
	}
	want := labelFilter("MetricName", "up")
	if !f.Predicate.Equal(want) {
		t.Fatalf("expected the residual `AND true` folded away, leaving just the label "+
			"predicate\n--- want ---\n%#v\n--- got ---\n%#v", want, f.Predicate)
	}
}
