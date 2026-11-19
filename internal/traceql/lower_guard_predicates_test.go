package traceql

import (
	"testing"

	tempoql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The tests in this file pin both sides of the small guard predicates in
// lower.go: for each one, an input the guard accepts and an input that
// differs from it in exactly the part the guard tests, which it rejects.

// orFilterPipeline parses q and returns its pipeline with the first spanset
// filter in the pointer form withoutFetchlessOrOperands reads.
func orFilterPipeline(t *testing.T, q string) tempoql.Pipeline {
	t.Helper()
	p := mustParseTraceQL(t, q).Pipeline
	switch f := p.Elements[0].(type) {
	case tempoql.SpansetFilter:
		p.Elements[0] = &f
	case *tempoql.SpansetFilter:
	default:
		t.Fatalf("Parse(%q).Pipeline[0] = %T, want a spanset filter", q, p.Elements[0])
	}
	return p
}

// filterExpression returns the expression of q's first spanset filter.
func filterExpression(t *testing.T, q string) tempoql.FieldExpression {
	t.Helper()
	return orFilterPipeline(t, q).Elements[0].(*tempoql.SpansetFilter).Expression
}

// droppedFetchlessOperand reports whether withoutFetchlessOrOperands
// rewrote p's first filter, dropping the OR operand that fetches nothing.
func droppedFetchlessOperand(p tempoql.Pipeline) bool {
	before := p.Elements[0].(*tempoql.SpansetFilter).Expression
	after := withoutFetchlessOrOperands(p).Elements[0].(*tempoql.SpansetFilter).Expression
	return after != before
}

// TestWithoutFetchlessOrOperands_PointerScalarFilterStage pins the
// `*ScalarFilter` arm of stagesAddNoFetchCondition. A scalar filter adds
// no fetch condition only when neither side does: `avg(duration) > 1`
// fetches every span carrying a duration through its left side, so the
// OR keeps its static operand, while `count() > 1` adds nothing and the
// static is dropped.
func TestWithoutFetchlessOrOperands_PointerScalarFilterStage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query   string
		dropped bool
	}{
		{`{ span.a = "x" || true } | count() > 1`, true},
		{`{ span.a = "x" || true } | avg(duration) > 1`, false},
	} {
		p := orFilterPipeline(t, tc.query)
		sf, ok := p.Elements[1].(tempoql.ScalarFilter)
		if !ok {
			t.Fatalf("Parse(%q).Pipeline[1] = %T, want ScalarFilter", tc.query, p.Elements[1])
		}
		p.Elements[1] = &sf
		if got := droppedFetchlessOperand(p); got != tc.dropped {
			t.Errorf("%s with a *ScalarFilter stage: dropped the static operand = %v, want %v", tc.query, got, tc.dropped)
		}
	}
}

// TestScalarAddsNoFetchCondition_PointerAggregate pins the `*Aggregate` arm
// of scalarAddsNoFetchCondition: an aggregate over no field (count())
// adds no fetch condition, an aggregate over a field does.
func TestScalarAddsNoFetchCondition_PointerAggregate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		none  bool
	}{
		{`{ } | count() > 1`, true},
		{`{ } | avg(duration) > 1`, false},
	} {
		sf, ok := mustParseTraceQL(t, tc.query).Pipeline.Elements[1].(tempoql.ScalarFilter)
		if !ok {
			t.Fatalf("Parse(%q).Pipeline[1] is not a ScalarFilter", tc.query)
		}
		agg, ok := sf.LHS.(tempoql.Aggregate)
		if !ok {
			t.Fatalf("Parse(%q) scalar filter LHS = %T, want Aggregate", tc.query, sf.LHS)
		}
		if got := scalarAddsNoFetchCondition(&agg); got != tc.none {
			t.Errorf("scalarAddsNoFetchCondition(&%s) = %v, want %v", agg.String(), got, tc.none)
		}
	}
}

// TestWithoutFetchlessOrOperands_ExistenceOperands pins singleFetchCondition's
// unary arm. `= nil` and `!= nil` on a span-filtered attribute are one
// fetch condition each, so the OR drops its static operand. An existence
// test on an attribute the fetch layer does not filter per span (an event
// attribute) is not, and neither is a unary operator other than the two
// existence tests: the OR is left whole.
func TestWithoutFetchlessOrOperands_ExistenceOperands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query   string
		dropped bool
	}{
		{`{ span.a != nil || true }`, true},
		{`{ span.a = nil || true }`, true},
		{`{ resource.a = nil || true }`, true},
		{`{ event.a != nil || true }`, false},
		{`{ event.a = nil || true }`, false},
	} {
		if got := droppedFetchlessOperand(orFilterPipeline(t, tc.query)); got != tc.dropped {
			t.Errorf("%s: dropped the static operand = %v, want %v", tc.query, got, tc.dropped)
		}
	}
	notAttr := tempoql.UnaryOperation{Op: tempoql.OpNot, Expression: tempoql.NewScopedAttribute(tempoql.AttributeScopeSpan, false, "a")}
	if singleFetchCondition(notAttr) {
		t.Errorf("singleFetchCondition(!span.a) = true, want false: only = nil and != nil are one fetch condition")
	}
}

// TestAsUnaryNot_OnlyTheNotOperator pins asUnaryNot to the `!` operator:
// another unary operation is not a NOT and hands back no operand.
func TestAsUnaryNot_OnlyTheNotOperator(t *testing.T) {
	t.Parallel()
	if operand, ok := asUnaryNot(filterExpression(t, `{ !(span.a = "x") }`)); !ok || operand == nil {
		t.Errorf("asUnaryNot(!(span.a = \"x\")) = %v, %v; want its operand, true", operand, ok)
	}
	exists := filterExpression(t, `{ span.a != nil }`)
	if _, isUnary := asUnaryOperation(exists); !isUnary {
		t.Fatalf("span.a != nil parsed to %T, want a unary operation", exists)
	}
	if operand, ok := asUnaryNot(exists); ok || operand != nil {
		t.Errorf("asUnaryNot(span.a != nil) = %v, %v; want nil, false", operand, ok)
	}
	if operand, ok := asUnaryNot(filterExpression(t, `{ span.a = "x" }`)); ok || operand != nil {
		t.Errorf("asUnaryNot(span.a = \"x\") = %v, %v; want nil, false", operand, ok)
	}
}

// TestGuardAbsentAttribute_EveryOperand pins that guardAbsentAttribute
// guards each attribute operand however the operands are ordered: an
// operand with no existence probe (a literal) ahead of an attribute read
// does not stop the attribute's guard being added.
func TestGuardAbsentAttribute_EveryOperand(t *testing.T) {
	t.Parallel()
	attr := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "a"}
	lit := &chplan.LitString{V: "x"}
	cmp := &chplan.Binary{Op: chplan.OpNe, Left: attr, Right: lit}
	want, ok := attributeExistsPredicate(attr)
	if !ok {
		t.Fatalf("attributeExistsPredicate(SpanAttributes['a']) found no probe")
	}
	for _, operands := range [][]chplan.Expr{{attr, lit}, {lit, attr}} {
		got, isAnd := guardAbsentAttribute(cmp, operands...).(*chplan.Binary)
		if !isAnd || got.Op != chplan.OpAnd || !got.Left.Equal(want) || got.Right != cmp {
			t.Errorf("guardAbsentAttribute(cmp, %T, %T) = %#v, want the attribute's probe AND cmp", operands[0], operands[1], got)
		}
	}
	if got := guardAbsentAttribute(cmp, lit, lit); got != cmp {
		t.Errorf("guardAbsentAttribute(cmp, literals only) = %#v, want cmp unchanged", got)
	}
}

// TestIsTraceRootIdentityIntrinsic_OnlyRootIdentity pins that the
// trace-scoped intrinsic resolved from every span, traceDuration, is not a
// root-identity one, and that an intrinsic outside the trace-scoped set is
// not either.
func TestIsTraceRootIdentityIntrinsic_OnlyRootIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		i    tempoql.Intrinsic
		want bool
	}{
		{tempoql.IntrinsicTraceRootSpan, true},
		{tempoql.ScopedIntrinsicTraceRootService, true},
		{tempoql.IntrinsicTraceDuration, false},
		{tempoql.ScopedIntrinsicTraceDuration, false},
		{tempoql.IntrinsicName, false},
	} {
		if got := isTraceRootIdentityIntrinsic(tc.i); got != tc.want {
			t.Errorf("isTraceRootIdentityIntrinsic(%v) = %v, want %v", tc.i, got, tc.want)
		}
	}
}

// TestIsTraceScopedLookupColumn_OnlyLookupColumns pins that every lookup
// column is recognised and a spans-table column is not.
func TestIsTraceScopedLookupColumn_OnlyLookupColumns(t *testing.T) {
	t.Parallel()
	for _, l := range traceScopedLookups {
		if !isTraceScopedLookupColumn(l.column) {
			t.Errorf("isTraceScopedLookupColumn(%q) = false, want true", l.column)
		}
	}
	s := schema.DefaultOTelTraces()
	for _, name := range []string{s.TraceIDColumn, s.SpanNameColumn, ""} {
		if isTraceScopedLookupColumn(name) {
			t.Errorf("isTraceScopedLookupColumn(%q) = true, want false", name)
		}
	}
}

// TestStringifyNumericMaterializedForStringOp_Shapes pins which comparisons
// read a numeric materialized column as a string: every regex match, even
// against a pattern that parses as an Int32, and a comparison against a
// string literal that does not parse as one, on either side. A comparison
// against an Int32-shaped string keeps the bare column.
func TestStringifyNumericMaterializedForStringOp_Shapes(t *testing.T) {
	t.Parallel()
	column := &chplan.FieldAccess{
		Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "http.status_code",
		MaterializedColumn: "http_status_code", MaterializedColumnNumeric: true,
	}
	stringified := func(e chplan.Expr) bool {
		call, ok := e.(*chplan.FuncCall)
		return ok && call.Fn == chplan.FnToString && len(call.Args) == 1 && call.Args[0] == column
	}
	int32Lit, textLit := &chplan.LitString{V: "5"}, &chplan.LitString{V: "abc"}
	for _, tc := range []struct {
		name     string
		op       chplan.BinaryOp
		lhs, rhs chplan.Expr
		want     bool
	}{
		{"=~ an Int32-shaped pattern", chplan.OpMatch, column, int32Lit, true},
		{"!~ an Int32-shaped pattern", chplan.OpNotMatch, column, int32Lit, true},
		{"= a non-Int32 string, column on the left", chplan.OpEq, column, textLit, true},
		{"= a non-Int32 string, column on the right", chplan.OpEq, textLit, column, true},
		{"= an Int32-shaped string", chplan.OpEq, column, int32Lit, false},
	} {
		lhs, rhs := stringifyNumericMaterializedForStringOp(tc.op, tc.lhs, tc.rhs)
		side := lhs
		if tc.lhs != column {
			side = rhs
		}
		if got := stringified(side); got != tc.want {
			t.Errorf("%s: the column is read as a string = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAttributeReadArms_UnscopedShapeOnly pins attributeReadArms's FuncCall
// arm to the unscoped read's exact shape: `if(cond, span read, resource
// read)`. Another three-argument function, or an if whose either arm is
// not an attribute read, is not an attribute read.
func TestAttributeReadArms_UnscopedShapeOnly(t *testing.T) {
	t.Parallel()
	span := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "a"}
	resource := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "ResourceAttributes"}, Path: "a"}
	cond := &chplan.FuncCall{Fn: chplan.FnMapContainsKey, Args: []chplan.Expr{&chplan.ColumnRef{Name: "SpanAttributes"}, &chplan.LitString{V: "a"}}}
	lit := &chplan.LitString{V: "x"}

	arms, _, ok := attributeReadArms(&chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{cond, span, resource}})
	if !ok || len(arms) != 2 || arms[0] != span || arms[1] != resource {
		t.Errorf("attributeReadArms(if(cond, span, resource)) = %v, %v; want [span resource], true", arms, ok)
	}
	for _, tc := range []struct {
		name string
		e    chplan.Expr
	}{
		{"multiIf(cond, span, resource)", &chplan.FuncCall{Fn: chplan.FnMultiIf, Args: []chplan.Expr{cond, span, resource}}},
		{"if(cond, span, literal)", &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{cond, span, lit}}},
		{"if(cond, literal, resource)", &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{cond, lit, resource}}},
	} {
		if arms, _, ok := attributeReadArms(tc.e); ok {
			t.Errorf("attributeReadArms(%s) = %v, true; want not an attribute read", tc.name, arms)
		}
	}
}

// TestNestedSetIntrinsicOperand_OnlyUnaryMinus pins that only a unary
// minus is seen through: another unary operator over a nested-set
// intrinsic is not a nested-set operand.
func TestNestedSetIntrinsicOperand_OnlyUnaryMinus(t *testing.T) {
	t.Parallel()
	parent := tempoql.NewIntrinsic(tempoql.IntrinsicNestedSetParent)
	if a, negated, ok := nestedSetIntrinsicOperand(tempoql.UnaryOperation{Op: tempoql.OpSub, Expression: parent}); !ok || !negated || a.Intrinsic != tempoql.IntrinsicNestedSetParent {
		t.Errorf("nestedSetIntrinsicOperand(-nestedSetParent) = %v, %v, %v; want nestedSetParent, true, true", a, negated, ok)
	}
	if a, negated, ok := nestedSetIntrinsicOperand(tempoql.UnaryOperation{Op: tempoql.OpNot, Expression: parent}); ok || negated {
		t.Errorf("nestedSetIntrinsicOperand(!nestedSetParent) = %v, %v, %v; want not a nested-set operand", a, negated, ok)
	}
}

// TestLowerNestedSetBinary_FloatLiteralTakesTheGeneralPath pins that only
// an integer literal reduces a nestedSetParent comparison to a rootness
// test. `nestedSetParent < 0.5` holds for a root (-1) and for an unnumbered
// span (0), so it is not `ParentSpanId = ”`: it compares the nested-set
// column.
func TestLowerNestedSetBinary_FloatLiteralTakesTheGeneralPath(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	b, ok := filterExpression(t, `{ nestedSetParent < 0.5 }`).(*tempoql.BinaryOperation)
	if !ok {
		t.Fatalf("nestedSetParent < 0.5 did not parse to a binary operation")
	}
	col, ok := nestedSetColumn(tempoql.IntrinsicNestedSetParent)
	if !ok {
		t.Fatalf("nestedSetParent has no nested-set column")
	}
	expr, handled, err := lowerNestedSetBinary(b, chplan.OpLt, s)
	if err != nil || !handled {
		t.Fatalf("lowerNestedSetBinary(nestedSetParent < 0.5) = %v, %v, %v", expr, handled, err)
	}
	cmp, ok := expr.(*chplan.Binary)
	if !ok {
		t.Fatalf("lowerNestedSetBinary(nestedSetParent < 0.5) = %#v, want a comparison", expr)
	}
	if left, ok := cmp.Left.(*chplan.ColumnRef); !ok || left.Name != col {
		t.Errorf("nestedSetParent < 0.5 compares %#v, want the nested-set column %s", cmp.Left, col)
	}

	// An integer literal with the same answer on every domain does reduce.
	b, ok = filterExpression(t, `{ nestedSetParent < 0 }`).(*tempoql.BinaryOperation)
	if !ok {
		t.Fatalf("nestedSetParent < 0 did not parse to a binary operation")
	}
	expr, _, err = lowerNestedSetBinary(b, chplan.OpLt, s)
	if err != nil {
		t.Fatalf("lowerNestedSetBinary(nestedSetParent < 0): %v", err)
	}
	if cmp, ok := expr.(*chplan.Binary); !ok || !cmp.Left.Equal(&chplan.ColumnRef{Name: s.ParentSpanIDColumn}) {
		t.Errorf("nestedSetParent < 0 = %#v, want the ParentSpanId rootness test", expr)
	}
}
