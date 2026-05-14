package optimizer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// nonAnalyzerCountingRule is a plain Rule (no isAnalyzerRule marker).
// Passing it into an Analyzer batch must trigger the type-contract
// panic at runtime — the marker interface is the gate at compile time
// for AnalyzerBatch(), and the Driver's runBatch is the runtime gate
// for hand-rolled Batch{Strategy: Analyzer()}.
type nonAnalyzerCountingRule struct{ calls *int }

func (r nonAnalyzerCountingRule) Name() string { return "non-analyzer" }

func (r nonAnalyzerCountingRule) Apply(n chplan.Node) (chplan.Node, bool) {
	*r.calls++
	return n, false
}

func TestAnalyzerBatch_RunsOnceAndVerifies(t *testing.T) {
	t.Parallel()

	// IdempotentTestAnalyzerRule rewrites once, then reports no
	// change. The Analyzer strategy runs the rule once over the tree
	// (produces the canonical form) + a verification walk (must
	// report no change) = 2 Apply invocations *per node visited*.
	// With a single Scan node, that's 2 calls total.
	var calls int
	d := optimizer.NewWithBatches(
		optimizer.AnalyzerBatch("analyzer.test", optimizer.IdempotentTestAnalyzerRule{Calls: &calls}),
	)

	out := d.Run(context.Background(), &chplan.Scan{Table: "raw"})

	if s, ok := out.(*chplan.Scan); !ok || s.Table != "canon" {
		t.Fatalf("expected Scan{Table:canon}, got %#v", out)
	}
	if calls != 2 {
		t.Fatalf("Analyzer strategy should run rule + verification (2 calls per node), got %d", calls)
	}
}

func TestAnalyzerBatch_NoOpRuleStillVerifies(t *testing.T) {
	t.Parallel()

	// When the rule's first pass reports no change, the verification
	// pass still runs (defensively — we don't trust the rule's
	// changed flag to imply idempotence). 2 Apply calls per node.
	var calls int
	d := optimizer.NewWithBatches(
		optimizer.AnalyzerBatch("analyzer.test", optimizer.IdempotentTestAnalyzerRule{Calls: &calls}),
	)

	d.Run(context.Background(), &chplan.Scan{Table: "already-canonical"})

	if calls != 2 {
		t.Fatalf("Analyzer strategy should always verify (2 calls per node), got %d", calls)
	}
}

func TestAnalyzerBatch_PanicsOnNonIdempotentRule(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on non-idempotent analyzer rule")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "non-idempotent-analyzer") {
			t.Fatalf("panic message should name the offending rule, got %q", msg)
		}
		if !strings.Contains(msg, "idempotent") {
			t.Fatalf("panic message should mention the idempotence contract, got %q", msg)
		}
	}()

	d := optimizer.NewWithBatches(
		optimizer.AnalyzerBatch("analyzer.non-idempotent", optimizer.NonIdempotentTestAnalyzerRule{}),
	)
	d.Run(context.Background(), &chplan.Scan{Table: "a"})
}

func TestAnalyzerBatch_PanicsWhenStrategyHasNonAnalyzerRule(t *testing.T) {
	t.Parallel()

	// AnalyzerBatch() enforces the type contract at compile time
	// (parameter is ...AnalyzerRule). But a caller might construct a
	// Batch by hand with Strategy: Analyzer() and a plain Rule — the
	// Driver's runBatch must catch that at execution time and panic
	// with a helpful message pointing at AnalyzerBatch().
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on analyzer batch containing non-AnalyzerRule")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "non-analyzer") {
			t.Fatalf("panic message should name the offending rule, got %q", msg)
		}
		if !strings.Contains(msg, "AnalyzerBatch") {
			t.Fatalf("panic message should point at AnalyzerBatch(), got %q", msg)
		}
	}()

	var calls int
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "analyzer.handcrafted",
		Strategy: optimizer.Analyzer(),
		Rules:    []optimizer.Rule{nonAnalyzerCountingRule{calls: &calls}},
	})
	d.Run(context.Background(), &chplan.Scan{Table: "t"})
}

func TestConstantFoldSemantic_FoldsLiteralArithmetic(t *testing.T) {
	t.Parallel()

	// `1 + 2` collapses to `LitInt(3)`, then `3 = 3` collapses to
	// `LitBool(true)`. ConstantFoldSemantic must fold this without
	// help from the heuristic flavour.
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
			Right: &chplan.LitInt{V: 3},
		},
	}

	out, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if !changed {
		t.Fatalf("ConstantFoldSemantic should have reduced `1+2 = 3` → `true`")
	}

	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected *Filter, got %T", out)
	}
	lit, ok := f.Predicate.(*chplan.LitBool)
	if !ok {
		t.Fatalf("expected predicate LitBool, got %T", f.Predicate)
	}
	if !lit.V {
		t.Fatalf("expected predicate LitBool(true), got LitBool(%v)", lit.V)
	}
}

func TestConstantFoldSemantic_LeavesBoolIdentityAlone(t *testing.T) {
	t.Parallel()

	// `true AND X` is a *heuristic* fold — ConstantFoldSemantic must
	// NOT collapse it. The boolean identity is the heuristic rule's
	// territory; mixing the two flavours is exactly the bug the
	// analyzer/optimizer split fixes.
	pred := &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: &chplan.LitBool{V: true},
		Right: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	}
	plan := &chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: pred}

	_, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if changed {
		t.Fatalf("ConstantFoldSemantic must not apply boolean identity (`true AND X → X`)")
	}
}

func TestConstantFoldHeuristic_AppliesBoolIdentity(t *testing.T) {
	t.Parallel()

	// `true AND X → X` is the heuristic flavour's job. Verify it
	// fires.
	inner := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "up"},
	}
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: inner,
		},
	}

	out, changed := optimizer.ConstantFoldHeuristic{}.Apply(plan)
	if !changed {
		t.Fatalf("ConstantFoldHeuristic should have applied `true AND X → X`")
	}
	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected *Filter, got %T", out)
	}
	if f.Predicate != chplan.Expr(inner) {
		t.Fatalf("expected predicate to collapse to inner Binary, got %#v", f.Predicate)
	}
}

func TestConstantFoldHeuristic_LeavesLiteralArithmeticAlone(t *testing.T) {
	t.Parallel()

	// `1 + 2 = 3` is the *semantic* flavour's job. The heuristic
	// must not touch pure-literal arithmetic — that's the whole
	// point of the split.
	pred := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
		Right: &chplan.LitInt{V: 3},
	}
	plan := &chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: pred}

	_, changed := optimizer.ConstantFoldHeuristic{}.Apply(plan)
	if changed {
		t.Fatalf("ConstantFoldHeuristic must not fold pure-literal arithmetic (`1+2=3`)")
	}
}

func TestDefault_AnalyzerRunsBeforeOptimizer(t *testing.T) {
	t.Parallel()

	// Composition test: `(1+2 = 3) AND X` should reduce to `X` after
	// Default()'s full pipeline — the semantic batch folds `1+2=3` →
	// `true`, and the heuristic batch then collapses `true AND X` →
	// `X`. Verifies analyzer-before-optimizer ordering.
	inner := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "MetricName"},
		Right: &chplan.LitString{V: "up"},
	}
	plan := chplan.Node(&chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
				Right: &chplan.LitInt{V: 3},
			},
			Right: inner,
		},
	})

	out := optimizer.Default().Run(context.Background(), plan)
	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected *Filter, got %T", out)
	}
	// After the analyzer + heuristic batches, the predicate should
	// have collapsed to the inner Binary (column = "up").
	got, ok := f.Predicate.(*chplan.Binary)
	if !ok {
		t.Fatalf("expected predicate *Binary, got %T", f.Predicate)
	}
	if got.Op != chplan.OpEq {
		t.Fatalf("expected predicate Op = OpEq, got %v", got.Op)
	}
}
