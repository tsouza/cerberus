package ast

import "testing"

// The cases in this file exercise the recursive arms of the
// ReferencesIntrinsic walk that references_test.go's query table does not
// reach: the two-sided recursions (spanset operations nested inside a
// spanset operation, scalar filters, scalar arithmetic) and the metrics
// first-stage nodes.
//
// Every two-sided case deliberately puts the intrinsic on exactly ONE side.
// A walk that folded its two recursive calls together with `&&` instead of
// `||` — or that compared the intrinsic with `!=` instead of `==` — would
// answer false where the whole-query scope demands true, and each test
// below is the case that tells those two implementations apart. That is
// also what makes them worth keeping: a one-sided reference is the common
// shape in practice (`{ name = "a" } >> { .x = 1 }`), and it is exactly the
// shape a naive "both sides must agree" walk gets wrong.

// TestReferencesIntrinsic_ScalarFilterSide pins that a scalar filter's two
// operands are searched independently. `max(name) > 1s` names the intrinsic
// on the aggregate side only; the literal side names nothing.
func TestReferencesIntrinsic_ScalarFilterSide(t *testing.T) {
	expr := mustParse(t, `{ } | max(name) > 1s`)
	if !expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("`{ } | max(name) > 1s` should reference name: the aggregate's inner expression is the intrinsic")
	}
	// The same shape without the intrinsic must stay false, so the case
	// above cannot pass by returning true unconditionally.
	other := mustParse(t, `{ } | max(duration) > 1s`)
	if other.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("`{ } | max(duration) > 1s` must not reference name")
	}
}

// TestReferencesIntrinsic_ScalarOperationSide pins the recursion through
// scalar arithmetic: `max(name) + 1` is a ScalarOperation whose left operand
// is the aggregate naming the intrinsic and whose right operand is a literal.
func TestReferencesIntrinsic_ScalarOperationSide(t *testing.T) {
	expr := mustParse(t, `{ } | max(name) + 1 > 2`)
	if !expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("`max(name) + 1 > 2` should reference name through the scalar operation's left operand")
	}
}

// TestReferencesIntrinsic_NestedSpansetOperation pins the recursion through a
// spanset operation that is itself an operand of a spanset operation. Only
// the innermost left spanset names the intrinsic.
func TestReferencesIntrinsic_NestedSpansetOperation(t *testing.T) {
	expr := mustParse(t, `{ name = "a" } >> { .x = 1 } >> { .y = 2 }`)
	if !expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("a chained structural query should reference name from its innermost left spanset")
	}
	other := mustParse(t, `{ .a = 1 } >> { .x = 1 } >> { .y = 2 }`)
	if other.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("a chained structural query naming no intrinsic must stay false")
	}
}

// TestReferencesIntrinsic_ScalarFilterAsSpansetOperand pins the scalar-filter
// arm of the spanset recursion. A parenthesised aggregate comparison parses
// to a bare ScalarFilter — parsePipelineStage routes a leading aggregate
// token to parseScalarStage, and parseParenPipeline hands the result back
// unwrapped because ScalarFilter satisfies SpansetExpression. So it lands on
// one side of a structural operator with no enclosing Pipeline to walk
// through. Note the parens must NOT contain a `|` chain: `({ } | max(name) >
// 1s)` would parse to a Pipeline and exercise a different arm.
func TestReferencesIntrinsic_ScalarFilterAsSpansetOperand(t *testing.T) {
	expr := mustParse(t, `(max(name) > 1s) >> { .x = 1 }`)
	if _, ok := expr.Pipeline.Elements[0].(SpansetOperation); !ok {
		t.Fatalf("expected a SpansetOperation, got %T — the query no longer exercises the intended arm", expr.Pipeline.Elements[0])
	}
	if !expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("a scalar filter on one side of a structural operator should be searched")
	}
}

// TestReferencesIntrinsic_AverageOverTimeAttribute pins that
// avg_over_time's aggregated attribute is compared against the intrinsic
// being searched for, not merely inspected. The `by(...)` list is empty
// here, so the aggregated attribute is the only thing that can match.
func TestReferencesIntrinsic_AverageOverTimeAttribute(t *testing.T) {
	expr := mustParse(t, `{ } | avg_over_time(name)`)
	if !expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("avg_over_time(name) should reference name through its aggregated attribute")
	}
	other := mustParse(t, `{ } | avg_over_time(duration)`)
	if other.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("avg_over_time(duration) must not reference name")
	}
}

// TestReferencesIntrinsic_MetricsCompareNilFilter pins that a compare stage
// with no filter answers false instead of dereferencing it. MetricsCompare
// holds its filter by pointer, so the nil is representable; the walk has to
// check the filter separately from the stage itself rather than assume a
// non-nil stage implies a non-nil filter.
func TestReferencesIntrinsic_MetricsCompareNilFilter(t *testing.T) {
	expr := &RootExpr{MetricsPipeline: &MetricsCompare{}}
	if expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("a compare stage with a nil filter references nothing")
	}
}

// TestReferencesIntrinsic_NilReceiverWithRealIntrinsic pins the nil-receiver
// guard against a genuine intrinsic. The sibling case in references_test.go
// passes IntrinsicNone, which the second half of the same guard already
// rejects, so only this case distinguishes the two halves.
func TestReferencesIntrinsic_NilReceiverWithRealIntrinsic(t *testing.T) {
	var expr *RootExpr
	if expr.ReferencesIntrinsic(IntrinsicName) {
		t.Errorf("a nil query references no intrinsic")
	}
}
