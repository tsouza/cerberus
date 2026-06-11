package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Equal-invariant coverage for chplan.InList, mirroring the
// positive/negative pattern in equal_invariants_test.go: structurally
// identical values compare Equal; values differing in exactly one
// load-bearing field do not, symmetrically.

func inListFixture() *chplan.InList {
	return &chplan.InList{
		Left: &chplan.ColumnRef{Name: "TraceId"},
		List: []chplan.Expr{
			&chplan.LitString{V: "a"},
			&chplan.LitString{V: "b"},
		},
	}
}

func TestInList_Equal_Positive(t *testing.T) {
	t.Parallel()
	a, b := inListFixture(), inListFixture()
	if !a.Equal(b) || !b.Equal(a) {
		t.Errorf("structurally identical InList values must compare Equal (both directions)")
	}
}

func TestInList_Equal_Negative(t *testing.T) {
	t.Parallel()
	cases := map[string]chplan.Expr{
		"different left": &chplan.InList{
			Left: &chplan.ColumnRef{Name: "SpanId"},
			List: []chplan.Expr{&chplan.LitString{V: "a"}, &chplan.LitString{V: "b"}},
		},
		"nil left": &chplan.InList{
			List: []chplan.Expr{&chplan.LitString{V: "a"}, &chplan.LitString{V: "b"}},
		},
		"different list element": &chplan.InList{
			Left: &chplan.ColumnRef{Name: "TraceId"},
			List: []chplan.Expr{&chplan.LitString{V: "a"}, &chplan.LitString{V: "z"}},
		},
		"different list length": &chplan.InList{
			Left: &chplan.ColumnRef{Name: "TraceId"},
			List: []chplan.Expr{&chplan.LitString{V: "a"}},
		},
		"other type": &chplan.LitString{V: "a"},
	}
	base := inListFixture()
	for name, other := range cases {
		if base.Equal(other) {
			t.Errorf("%s: Equal must be false", name)
		}
		if other.Equal(base) {
			t.Errorf("%s: Equal must be false symmetrically", name)
		}
	}
}
