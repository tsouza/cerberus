package optimizer_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// countingRule reports how many times Apply was invoked. Useful for
// asserting Once vs FixedPoint semantics.
type countingRule struct {
	name      string
	calls     *int
	transform func(chplan.Node) (chplan.Node, bool)
}

func (r countingRule) Name() string { return r.name }

func (r countingRule) Apply(n chplan.Node) (chplan.Node, bool) {
	*r.calls++
	if r.transform == nil {
		return n, false
	}
	return r.transform(n)
}

// noopRule never rewrites.
func noopRule(name string, calls *int) optimizer.Rule {
	return countingRule{name: name, calls: calls}
}

// renamingRule swaps a Scan's Table once: "a" → "b". After one rewrite
// it's a no-op (because Table is no longer "a").
func renamingRule(name, from, to string, calls *int) optimizer.Rule {
	return countingRule{
		name:  name,
		calls: calls,
		transform: func(n chplan.Node) (chplan.Node, bool) {
			if s, ok := n.(*chplan.Scan); ok && s.Table == from {
				cp := *s
				cp.Table = to
				return &cp, true
			}
			return n, false
		},
	}
}

// alwaysChangingRule swaps the Scan's table between "a" and "b" each
// time — it never converges. Used to exercise the iteration cap.
func alwaysChangingRule(name string, calls *int) optimizer.Rule {
	return countingRule{
		name:  name,
		calls: calls,
		transform: func(n chplan.Node) (chplan.Node, bool) {
			if s, ok := n.(*chplan.Scan); ok {
				cp := *s
				if cp.Table == "a" {
					cp.Table = "b"
				} else {
					cp.Table = "a"
				}
				return &cp, true
			}
			return n, false
		},
	}
}

func TestStrategy_OnceRunsRulesExactlyOnce(t *testing.T) {
	t.Parallel()

	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "once-batch",
		Strategy: optimizer.Once(),
		Rules:    []optimizer.Rule{noopRule("noop", &calls)},
	})

	d.Run(context.Background(), &chplan.Scan{Table: "t"})

	if calls != 1 {
		t.Fatalf("Once should invoke rule exactly once, got %d", calls)
	}
}

func TestStrategy_OnceRunsEvenWhenRuleChanges(t *testing.T) {
	t.Parallel()

	// A rule that rewrites should still get only one shot under Once.
	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "once-changing",
		Strategy: optimizer.Once(),
		Rules:    []optimizer.Rule{renamingRule("rename", "a", "b", &calls)},
	})

	out := d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if calls != 1 {
		t.Fatalf("Once should invoke rule exactly once even on change, got %d", calls)
	}
	if s, ok := out.(*chplan.Scan); !ok || s.Table != "b" {
		t.Fatalf("expected Scan{Table:b}, got %#v", out)
	}
}

func TestStrategy_FixedPointStopsAtFixpoint(t *testing.T) {
	t.Parallel()

	// renamingRule rewrites once, then stops reporting changes. The
	// FixedPoint loop must:
	//   iter 1: change=true (a → b)
	//   iter 2: change=false → stop
	// → 2 invocations of Apply.
	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "fp-converges",
		Strategy: optimizer.FixedPoint(50),
		Rules:    []optimizer.Rule{renamingRule("rename", "a", "b", &calls)},
	})

	d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if calls != 2 {
		t.Fatalf("FixedPoint should stop after fixpoint (2 calls), got %d", calls)
	}
}

func TestStrategy_FixedPointStopsImmediatelyIfNoChange(t *testing.T) {
	t.Parallel()

	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "fp-noop",
		Strategy: optimizer.FixedPoint(50),
		Rules:    []optimizer.Rule{noopRule("noop", &calls)},
	})

	d.Run(context.Background(), &chplan.Scan{Table: "t"})

	if calls != 1 {
		t.Fatalf("FixedPoint with a no-op rule should call once and stop, got %d", calls)
	}
}

func TestStrategy_FixedPointRespectsMaxIterations(t *testing.T) {
	t.Parallel()

	// alwaysChangingRule never converges; the cap should bound the loop.
	var calls int
	const cap = 7
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "fp-capped",
		Strategy: optimizer.FixedPoint(cap),
		Rules:    []optimizer.Rule{alwaysChangingRule("flap", &calls)},
	})

	d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if calls != cap {
		t.Fatalf("FixedPoint should honour iteration cap (%d), got %d", cap, calls)
	}
}

func TestStrategy_FixedPointClampsToOne(t *testing.T) {
	t.Parallel()

	// FixedPoint(0) is a footgun — clamp to 1.
	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "fp-zero",
		Strategy: optimizer.FixedPoint(0),
		Rules:    []optimizer.Rule{noopRule("noop", &calls)},
	})

	d.Run(context.Background(), &chplan.Scan{Table: "t"})

	if calls != 1 {
		t.Fatalf("FixedPoint(0) should clamp to 1 iteration, got %d", calls)
	}
}

func TestDriver_BatchesRunInOrder(t *testing.T) {
	t.Parallel()

	// Batch A renames "a" → "b"; Batch B renames "b" → "c". The output
	// must be "c" — confirms batches run sequentially, each seeing the
	// output of the previous.
	var aCalls, bCalls int
	d := optimizer.NewWithBatches(
		optimizer.Batch{
			Name:     "first",
			Strategy: optimizer.FixedPoint(10),
			Rules:    []optimizer.Rule{renamingRule("a-to-b", "a", "b", &aCalls)},
		},
		optimizer.Batch{
			Name:     "second",
			Strategy: optimizer.FixedPoint(10),
			Rules:    []optimizer.Rule{renamingRule("b-to-c", "b", "c", &bCalls)},
		},
	)

	out := d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if s, ok := out.(*chplan.Scan); !ok || s.Table != "c" {
		t.Fatalf("expected Scan{Table:c}, got %#v", out)
	}
}

func TestNew_WrapsInSingleFixedPointBatch(t *testing.T) {
	t.Parallel()

	// The back-compat New(rules...) constructor must still work and
	// behave like a single FixedPoint batch — renaming rule should
	// converge in 2 calls (rewrite once, then verify no change).
	var calls int
	d := optimizer.New(renamingRule("rename", "a", "b", &calls))

	out := d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if calls != 2 {
		t.Fatalf("New() should wrap rules in a FixedPoint batch, got %d calls", calls)
	}
	if s, ok := out.(*chplan.Scan); !ok || s.Table != "b" {
		t.Fatalf("expected Scan{Table:b}, got %#v", out)
	}
}
