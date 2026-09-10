package spec

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestRowShapeDivergencePredicates(t *testing.T) {
	float := &chplan.Project{Input: &chplan.OneRow{}, Roles: []chplan.Column{{Name: "value", Role: chplan.RoleValue}}, Projections: []chplan.Projection{{Expr: &chplan.LitInt{V: 1}, Alias: "value"}}}
	mixed := &chplan.Scan{Columns: []string{chplan.MixedDiscriminatorColumn}, Roles: []chplan.Column{{Name: chplan.MixedDiscriminatorColumn, Role: chplan.RoleDiscriminator}}}
	cases := []struct {
		name      string
		node      chplan.Node
		divergent bool
	}{
		{"scalar projection", float, true},
		{"unflagged mixed filter", &chplan.Filter{Input: mixed}, true},
		{"mixed filter declared", &chplan.Filter{Input: mixed, Mixed: true}, false},
		{"incorrect histogram filter flag", &chplan.Filter{Input: mixed, Histogram: true}, false},
		{"mixed projection agrees", &chplan.Project{Input: mixed, Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn}}}}, false},
		{"topk selects scalar", &chplan.TopK{Input: float, Columns: []string{"value"}}, true},
		{"incorrect mixed topk flag", &chplan.TopK{Input: float, Mixed: true}, false},
		{"orderby delegates scalar projection", &chplan.OrderBy{Input: float}, true},
		{"union delegates scalar projection", &chplan.UnionAll{Inputs: []chplan.Node{float, float}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if reason := rowShapeDivergence(tc.node); (reason != "") != tc.divergent {
				t.Fatalf("divergence reason = %q, want divergent=%v", reason, tc.divergent)
			}
		})
	}
}

func TestRowShapeNamedWindowDivergence(t *testing.T) {
	input := &chplan.Scan{Columns: []string{"name"}, Roles: []chplan.Column{{Name: "name", Role: chplan.RoleMetricName}}}
	window := &chplan.RangeWindow{Input: input, GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "name"}}, ValueColumn: "value", Func: "last_over_time"}
	if rowShapeDivergence(window) == "" {
		t.Fatal("name-preserving instant window not reconciled")
	}
	window.OuterRange = time.Minute
	window.TimestampColumn = "time"
	if rowShapeDivergence(window) == "" {
		t.Fatal("name-preserving matrix window not reconciled")
	}
	window.GroupBy = nil
	if rowShapeDivergence(window) != "" {
		t.Fatal("ordinary grid window incorrectly treated as divergent")
	}
}
