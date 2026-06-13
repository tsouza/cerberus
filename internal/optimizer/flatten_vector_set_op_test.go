package optimizer_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// cols is the shared canonical-column setter every VectorSetOp /
// NaryVectorSetOp in this file uses, so the flatten rule's
// same-shape guard is satisfied across chain links.
func setOpCols(metric, attrs, ts, val string) func(op chplan.VectorSetOpKind, l, r chplan.Node) *chplan.VectorSetOp {
	return func(op chplan.VectorSetOpKind, l, r chplan.Node) *chplan.VectorSetOp {
		return &chplan.VectorSetOp{
			Left: l, Right: r, Op: op,
			MetricNameColumn: metric, AttributesColumn: attrs,
			TimestampColumn: ts, ValueColumn: val,
		}
	}
}

func tableScan(name string) *chplan.Scan { return &chplan.Scan{Table: name} }

// TestFlattenVectorSetOp_OrChainOfFour linearises
// `((a or b) or c) or d` into one NaryVectorSetOp with four arms in
// source order.
func TestFlattenVectorSetOp_OrChainOfFour(t *testing.T) {
	t.Parallel()
	mk := setOpCols("MetricName", "Attributes", "TimeUnix", "Value")
	a, b, c, d := tableScan("a"), tableScan("b"), tableScan("c"), tableScan("d")
	in := mk(chplan.VectorSetOr,
		mk(chplan.VectorSetOr,
			mk(chplan.VectorSetOr, a, b),
			c),
		d)

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), in)

	nary, ok := out.(*chplan.NaryVectorSetOp)
	if !ok {
		t.Fatalf("expected *NaryVectorSetOp, got %T", out)
	}
	if nary.Op != chplan.VectorSetOr {
		t.Errorf("Op = %q, want or", nary.Op)
	}
	if len(nary.Arms) != 4 {
		t.Fatalf("Arms = %d, want 4", len(nary.Arms))
	}
	want := []string{"a", "b", "c", "d"}
	for i, arm := range nary.Arms {
		s, ok := arm.(*chplan.Scan)
		if !ok || s.Table != want[i] {
			t.Errorf("arm[%d] = %#v, want Scan(%s)", i, arm, want[i])
		}
	}
}

// TestFlattenVectorSetOp_AndChain linearises an `and` chain the same
// way; `and` is associative so it flattens too.
func TestFlattenVectorSetOp_AndChain(t *testing.T) {
	t.Parallel()
	mk := setOpCols("MetricName", "Attributes", "TimeUnix", "Value")
	a, b, c := tableScan("a"), tableScan("b"), tableScan("c")
	in := mk(chplan.VectorSetAnd, mk(chplan.VectorSetAnd, a, b), c)

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), in)

	nary, ok := out.(*chplan.NaryVectorSetOp)
	if !ok {
		t.Fatalf("expected *NaryVectorSetOp, got %T", out)
	}
	if nary.Op != chplan.VectorSetAnd || len(nary.Arms) != 3 {
		t.Fatalf("got op=%q arms=%d, want and/3", nary.Op, len(nary.Arms))
	}
}

// TestFlattenVectorSetOp_UnlessNeverFlattens proves `unless` keeps its
// binary shape — it is NOT associative, so flattening would change
// results.
func TestFlattenVectorSetOp_UnlessNeverFlattens(t *testing.T) {
	t.Parallel()
	mk := setOpCols("MetricName", "Attributes", "TimeUnix", "Value")
	a, b, c := tableScan("a"), tableScan("b"), tableScan("c")
	in := mk(chplan.VectorSetUnless, mk(chplan.VectorSetUnless, a, b), c)

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), in)

	if _, ok := out.(*chplan.NaryVectorSetOp); ok {
		t.Fatal("unless chain must NOT flatten into NaryVectorSetOp")
	}
	if _, ok := out.(*chplan.VectorSetOp); !ok {
		t.Fatalf("unless chain should stay a binary VectorSetOp, got %T", out)
	}
}

// TestFlattenVectorSetOp_MixedOpsDoNotMerge keeps a chain whose links
// disagree on the operator un-merged across the boundary: `(a and b) or
// c` flattens the inner `and` and the outer `or` but never fuses the
// two different operators into one node.
func TestFlattenVectorSetOp_MixedOpsDoNotMerge(t *testing.T) {
	t.Parallel()
	mk := setOpCols("MetricName", "Attributes", "TimeUnix", "Value")
	a, b, c := tableScan("a"), tableScan("b"), tableScan("c")
	in := mk(chplan.VectorSetOr, mk(chplan.VectorSetAnd, a, b), c)

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), in)

	nary, ok := out.(*chplan.NaryVectorSetOp)
	if !ok {
		t.Fatalf("expected outer *NaryVectorSetOp(or), got %T", out)
	}
	if nary.Op != chplan.VectorSetOr || len(nary.Arms) != 2 {
		t.Fatalf("outer got op=%q arms=%d, want or/2", nary.Op, len(nary.Arms))
	}
	// arm[0] is the flattened inner `and`; arm[1] is c.
	innerAnd, ok := nary.Arms[0].(*chplan.NaryVectorSetOp)
	if !ok || innerAnd.Op != chplan.VectorSetAnd || len(innerAnd.Arms) != 2 {
		t.Fatalf("arm[0] should be a flattened and-node, got %#v", nary.Arms[0])
	}
	if s, ok := nary.Arms[1].(*chplan.Scan); !ok || s.Table != "c" {
		t.Errorf("arm[1] = %#v, want Scan(c)", nary.Arms[1])
	}
}

// TestFlattenVectorSetOp_DifferingMatchDoesNotMerge leaves a chain
// whose links use different match modifiers (default vs on(job))
// un-flattened across the mismatch — they are not one associative
// chain.
func TestFlattenVectorSetOp_DifferingMatchDoesNotMerge(t *testing.T) {
	t.Parallel()
	a, b, c := tableScan("a"), tableScan("b"), tableScan("c")
	inner := &chplan.VectorSetOp{
		Left: a, Right: b, Op: chplan.VectorSetOr,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
	outerNode := &chplan.VectorSetOp{
		Left: inner, Right: c, Op: chplan.VectorSetOr,
		Match:            chplan.VectorMatch{}, // default, differs from inner
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), outerNode)

	nary, ok := out.(*chplan.NaryVectorSetOp)
	if !ok {
		t.Fatalf("expected outer *NaryVectorSetOp, got %T", out)
	}
	// The outer default-match node has two arms: the inner on(job) chain
	// (itself flattened) and c — they must NOT be spliced flat because the
	// match modifiers differ.
	if len(nary.Arms) != 2 {
		t.Fatalf("outer arms = %d, want 2 (no cross-match splice)", len(nary.Arms))
	}
	if _, ok := nary.Arms[0].(*chplan.NaryVectorSetOp); !ok {
		t.Errorf("arm[0] should be the separately-flattened on(job) chain, got %T", nary.Arms[0])
	}
}

// TestFlattenVectorSetOp_SingleBinaryFlattensToTwoArms confirms a lone
// `a or b` becomes a 2-arm N-ary node (the emitter handles >= 2 arms
// uniformly, so even the degenerate binary collapses cleanly).
func TestFlattenVectorSetOp_SingleBinaryFlattensToTwoArms(t *testing.T) {
	t.Parallel()
	mk := setOpCols("MetricName", "Attributes", "TimeUnix", "Value")
	in := mk(chplan.VectorSetOr, tableScan("a"), tableScan("b"))

	out := optimizer.New(optimizer.FlattenVectorSetOp{}).Run(context.Background(), in)

	nary, ok := out.(*chplan.NaryVectorSetOp)
	if !ok {
		t.Fatalf("expected *NaryVectorSetOp, got %T", out)
	}
	if len(nary.Arms) != 2 {
		t.Fatalf("Arms = %d, want 2", len(nary.Arms))
	}
}
