package optimizer_test

// Regression coverage for foldExprWith's expression-position walker
// (internal/optimizer/constant_fold.go). The walker must recurse into
// every chplan Expr kind that holds a child Expr — a missing case
// silently strands a pure-literal Binary subtree unfolded when it sits
// under a Lambda body, an InList element, a FieldAccess/Subscript/
// LineContent source, a LabelJoin/LabelReplace/MapWithoutKeys/
// MapWithoutEmptyValues map, or a NestedArrayExists value. Each of
// these is a real lowering shape (e.g. LogQL lowers predicates through
// Lambda bodies), so an unfolded literal here reaches the emitter as a
// live Binary instead of the collapsed Lit downstream rules assume.

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// literalSum is the canonical pure-literal Binary every case embeds:
// `1 + 2`, which ConstantFoldSemantic collapses to LitInt(3).
func literalSum() *chplan.Binary {
	return &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}}
}

func TestConstantFoldSemantic_RecursesEveryExprPosition(t *testing.T) {
	t.Parallel()

	want := &chplan.LitInt{V: 3}

	cases := []struct {
		name    string
		wrap    func(inner chplan.Expr) chplan.Expr
		extract func(e chplan.Expr) chplan.Expr
	}{
		{
			name: "Lambda.Body",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.Lambda{Params: []string{"x"}, Body: inner}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.Lambda).Body },
		},
		{
			name: "InList element",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.InList{Left: &chplan.ColumnRef{Name: "c"}, List: []chplan.Expr{inner}}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.InList).List[0] },
		},
		{
			name: "FieldAccess.Source",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.FieldAccess{Source: inner, Path: "http.method"}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.FieldAccess).Source },
		},
		{
			name: "Subscript.Container",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.Subscript{Container: inner, Key: &chplan.LitString{V: "k"}}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.Subscript).Container },
		},
		{
			name: "LabelJoin.Map",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.LabelJoin{Map: inner, Dst: "d", Separator: "-", Srcs: []string{"a"}}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.LabelJoin).Map },
		},
		{
			name: "LabelReplace.Map",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.LabelReplace{Map: inner, Dst: "d", Src: "s", Regex: ".*", Replacement: "r"}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.LabelReplace).Map },
		},
		{
			name: "MapWithoutKeys.Map",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.MapWithoutKeys{Map: inner, Keys: []string{"k"}}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.MapWithoutKeys).Map },
		},
		{
			name: "MapWithoutEmptyValues.Map",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.MapWithoutEmptyValues{Map: inner}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.MapWithoutEmptyValues).Map },
		},
		{
			name: "LineContent.Source",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.LineContent{Source: inner, Pattern: "p"}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.LineContent).Source },
		},
		{
			name: "NestedArrayExists.Value",
			wrap: func(inner chplan.Expr) chplan.Expr {
				return &chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: inner}
			},
			extract: func(e chplan.Expr) chplan.Expr { return e.(*chplan.NestedArrayExists).Value },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filter := &chplan.Filter{Input: &chplan.Scan{Table: "t"}, Predicate: tc.wrap(literalSum())}

			out, changed := optimizer.ConstantFoldSemantic{}.Apply(filter)
			if !changed {
				t.Fatalf("ConstantFoldSemantic did not report a change; the literal Binary under %s was never reached", tc.name)
			}
			got := tc.extract(out.(*chplan.Filter).Predicate)
			if !got.Equal(want) {
				t.Fatalf("literal Binary under %s folded to %#v; want %#v", tc.name, got, want)
			}
		})
	}
}
