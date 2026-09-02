package ast

import "testing"

// Mutation-coverage tests for expr.go: the type-inference contract of the
// composite field/scalar expression nodes.

// TestBinaryOperationImpliedTypePrefersConcreteLHS pins BinaryOperation's
// inference rule: a non-boolean operator takes its type from the LHS whenever
// the LHS resolves to something concrete, and only falls through to the RHS
// when the LHS is an unresolved attribute reference. Negating that guard
// (`t != TypeAttribute` -> `t == TypeAttribute`) swaps the two arms, so each
// case below is asserted with an LHS and an RHS of DIFFERENT types.
func TestBinaryOperationImpliedTypePrefersConcreteLHS(t *testing.T) {
	t.Parallel()
	attr := NewScopedAttribute(AttributeScopeSpan, false, "http.status_code")
	tests := []struct {
		name string
		op   *BinaryOperation
		want StaticType
	}{
		{
			name: "concrete LHS wins over a differently typed RHS",
			op:   &BinaryOperation{Op: OpAdd, LHS: NewStaticInt(1), RHS: NewStaticString("a")},
			want: TypeInt,
		},
		{
			name: "attribute LHS falls through to the RHS",
			op:   &BinaryOperation{Op: OpAdd, LHS: attr, RHS: NewStaticString("a")},
			want: TypeString,
		},
		{
			name: "boolean operator short-circuits both operands",
			op:   &BinaryOperation{Op: OpEqual, LHS: NewStaticInt(1), RHS: NewStaticInt(2)},
			want: TypeBoolean,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.op.impliedType(); got != tc.want {
				t.Fatalf("(%s).impliedType() = %v; want %v", tc.op.String(), got, tc.want)
			}
		})
	}
}

// TestScalarOperationImpliedTypePrefersConcreteLHS is the ScalarOperation
// twin of the test above. The two nodes carry the same inference rule in
// duplicated code, and each copy is its own mutation site.
func TestScalarOperationImpliedTypePrefersConcreteLHS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		op   ScalarOperation
		want StaticType
	}{
		{
			name: "concrete LHS wins over a differently typed RHS",
			op:   ScalarOperation{Op: OpAdd, LHS: NewStaticInt(1), RHS: NewStaticString("a")},
			want: TypeInt,
		},
		{
			name: "attribute-typed LHS falls through to the RHS",
			// An Aggregate over a bare attribute is the reachable scalar
			// whose implied type is TypeAttribute.
			op:   ScalarOperation{Op: OpAdd, LHS: newAggregate(AggregateMax, NewScopedAttribute(AttributeScopeSpan, false, "x")), RHS: NewStaticString("a")},
			want: TypeString,
		},
		{
			name: "boolean operator short-circuits both operands",
			op:   ScalarOperation{Op: OpGreater, LHS: NewStaticInt(1), RHS: NewStaticInt(2)},
			want: TypeBoolean,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.op.impliedType(); got != tc.want {
				t.Fatalf("(%s).impliedType() = %v; want %v", tc.op.String(), got, tc.want)
			}
		})
	}
}
