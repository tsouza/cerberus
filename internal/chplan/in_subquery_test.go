package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Equal-invariant coverage for chplan.InSubquery, mirroring the
// positive/negative pattern in in_list_test.go: structurally identical
// values compare Equal; values differing in exactly one load-bearing field
// do not, symmetrically.

func inSubqueryFixture() *chplan.InSubquery {
	return &chplan.InSubquery{
		Left: &chplan.ColumnRef{Name: "TraceId"},
		Subquery: &chplan.Project{
			Input:       &chplan.Scan{Table: "spans"},
			Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "TraceId"}}},
		},
	}
}

func TestInSubquery_Equal_Positive(t *testing.T) {
	t.Parallel()
	a, b := inSubqueryFixture(), inSubqueryFixture()
	if !a.Equal(b) || !b.Equal(a) {
		t.Errorf("structurally identical InSubquery values must compare Equal (both directions)")
	}
}

func TestInSubquery_Equal_Negative(t *testing.T) {
	t.Parallel()
	cases := map[string]chplan.Expr{
		"different left": &chplan.InSubquery{
			Left: &chplan.ColumnRef{Name: "SpanId"},
			Subquery: &chplan.Project{
				Input:       &chplan.Scan{Table: "spans"},
				Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "TraceId"}}},
			},
		},
		"nil left": &chplan.InSubquery{
			Subquery: &chplan.Project{
				Input:       &chplan.Scan{Table: "spans"},
				Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "TraceId"}}},
			},
		},
		"different subquery": &chplan.InSubquery{
			Left: &chplan.ColumnRef{Name: "TraceId"},
			Subquery: &chplan.Project{
				Input:       &chplan.Scan{Table: "other_spans"},
				Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "TraceId"}}},
			},
		},
		"nil subquery": &chplan.InSubquery{
			Left: &chplan.ColumnRef{Name: "TraceId"},
		},
		"other type": &chplan.LitString{V: "a"},
	}
	base := inSubqueryFixture()
	for name, other := range cases {
		if base.Equal(other) {
			t.Errorf("%s: Equal must be false", name)
		}
		if other.Equal(base) {
			t.Errorf("%s: Equal must be false symmetrically", name)
		}
	}
}

// TestInSubquery_CloneNode_IsolatesSubquery proves CloneNode deep-copies the
// embedded Subquery Node rather than aliasing it — mirroring
// ScalarSubquery's clone contract (see clone.go's InSubquery case).
func TestInSubquery_CloneNode_IsolatesSubquery(t *testing.T) {
	t.Parallel()

	orig := &chplan.Filter{
		Input:     &chplan.Scan{Table: "roots"},
		Predicate: inSubqueryFixture(),
	}
	clone := chplan.CloneNode(orig).(*chplan.Filter)

	origPred := orig.Predicate.(*chplan.InSubquery)
	clonePred := clone.Predicate.(*chplan.InSubquery)

	if origPred == clonePred {
		t.Fatal("CloneNode must not alias the InSubquery expression itself")
	}
	if !origPred.Equal(clonePred) {
		t.Fatal("cloned InSubquery must be structurally Equal to the original")
	}

	cloneSub := clonePred.Subquery.(*chplan.Project)
	cloneSub.Projections[0].Expr = &chplan.ColumnRef{Name: "SpanId"}

	origSub := origPred.Subquery.(*chplan.Project)
	if origSub.Projections[0].Expr.(*chplan.ColumnRef).Name != "TraceId" {
		t.Fatal("mutating the clone's embedded Subquery leaked back into the original")
	}
}
