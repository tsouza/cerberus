package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMixedVVHistMergeInputProjections_SubtractionNegatesByMinusOne pins
// [mixedVVHistMergeInputProjections]'s own doc: for `-`, R's count-bearing
// fields enter the merge array pre-negated via a literal `* -1` scale —
// NOT `* 1` (a sign bug that would silently turn subtraction into
// addition for the merged Count/Sum/ZeroCount fields).
func TestMixedVVHistMergeInputProjections_SubtractionNegatesByMinusOne(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	projs := mixedVVHistMergeInputProjections(chplan.OpSub, s)

	var countsExpr chplan.Expr
	for _, p := range projs {
		if p.Alias == hqMergeCountsArrayAlias {
			countsExpr = p.Expr
			break
		}
	}
	if countsExpr == nil {
		t.Fatalf("no projection aliased %q found", hqMergeCountsArrayAlias)
	}
	call, ok := countsExpr.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnArray || len(call.Args) != 2 {
		t.Fatalf("%s projection = %#v, want a two-arg array() FuncCall", hqMergeCountsArrayAlias, countsExpr)
	}
	negated, ok := call.Args[1].(*chplan.Binary)
	if !ok || negated.Op != chplan.OpMul {
		t.Fatalf("R's own array() element = %#v, want a Mul Binary (the negation fold)", call.Args[1])
	}
	lit, ok := negated.Right.(*chplan.LitFloat)
	if !ok || lit.V != -1 {
		t.Fatalf("negation scalar = %#v, want LitFloat{-1} (subtraction must negate, not double, R's own counts)", negated.Right)
	}
}

// TestMixedVVHistMergeInputProjections_AdditionForwardsUnnegated pins the
// `+` complement: R's own fields enter the merge array UNCHANGED (no Mul
// wrapper at all), so a mistaken op check can't silently negate an
// addition either.
func TestMixedVVHistMergeInputProjections_AdditionForwardsUnnegated(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	projs := mixedVVHistMergeInputProjections(chplan.OpAdd, s)

	var countsExpr chplan.Expr
	for _, p := range projs {
		if p.Alias == hqMergeCountsArrayAlias {
			countsExpr = p.Expr
			break
		}
	}
	if countsExpr == nil {
		t.Fatalf("no projection aliased %q found", hqMergeCountsArrayAlias)
	}
	call, ok := countsExpr.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnArray || len(call.Args) != 2 {
		t.Fatalf("%s projection = %#v, want a two-arg array() FuncCall", hqMergeCountsArrayAlias, countsExpr)
	}
	if _, wrapped := call.Args[1].(*chplan.Binary); wrapped {
		t.Fatalf("R's own array() element = %#v, want the bare column ref unwrapped (addition must not negate)", call.Args[1])
	}
}

// TestMixedVVOneSide pins the Card->side mapping [mixedVVOneSide] (the
// counterpart of the "many" side, mixedVVOneSide's own doc): CardOneToMany
// keeps L as the "one" side; every other Card (CardManyToOne here) keeps R.
func TestMixedVVOneSide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		card chplan.VectorCard
		want string
	}{
		{"one_to_many", chplan.CardOneToMany, mixedVVJoinSideL},
		{"many_to_one", chplan.CardManyToOne, mixedVVJoinSideR},
	}
	for _, tc := range cases {
		if got := mixedVVOneSide(tc.card); got != tc.want {
			t.Errorf("mixedVVOneSide(%v) = %q, want %q", tc.card, got, tc.want)
		}
	}
}

// TestMixedVVOutputAttributesExpr_IncludeBranch pins the `len(include) ==
// 0` fork for a non-one-to-one Card: no Include labels forwards the many
// side's Attributes column bare; any Include labels wrap it in the
// mapMerge/mapFilter overlay instead.
func TestMixedVVOutputAttributesExpr_IncludeBranch(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	match := chplan.VectorMatch{}

	bare := mixedVVOutputAttributesExpr(match, chplan.CardManyToOne, nil, s.AttributesColumn)
	if _, ok := bare.(*chplan.ColumnRef); !ok {
		t.Fatalf("mixedVVOutputAttributesExpr(no Include) = %#v, want a bare *chplan.ColumnRef", bare)
	}

	overlaid := mixedVVOutputAttributesExpr(match, chplan.CardManyToOne, []string{"region"}, s.AttributesColumn)
	call, ok := overlaid.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnMapMerge {
		t.Fatalf("mixedVVOutputAttributesExpr(Include=[region]) = %#v, want a mapMerge FuncCall", overlaid)
	}
}
