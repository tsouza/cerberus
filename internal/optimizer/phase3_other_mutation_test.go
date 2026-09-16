package optimizer

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func phase3RangeLWRRoleProject(roles ...chplan.Column) chplan.Node {
	projections := make([]chplan.Projection, len(roles))
	for i, column := range roles {
		projections[i] = chplan.Projection{Expr: &chplan.LitFloat{V: 1}, Alias: column.Name}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Projections: projections, Roles: roles}
}

// TestMutation_RangeLWRColumns_MissingSingleRoleDeclines kills both
// INVERT_LOGICAL mutants on projection_pushdown.go:rangeLWRColumns's
// required-role guard:
//
//	if metricName == "" || attributes == "" || timestamp == "" || value == "" {
//
// With exactly one role missing and the other three present, the original
// `||` returns nil (declining pushdown). Each `||` -> `&&` rewrite
// re-parenthesises the guard so the lone missing role folds away against
// the other three false operands, and the function instead returns a
// non-nil column set.
func TestMutation_RangeLWRColumns_MissingSingleRoleDeclines(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		roles []chplan.Column
	}{
		{
			name: "missing metric-name",
			roles: []chplan.Column{
				{Name: "attrs", Role: chplan.RoleAttributes},
				{Name: "ts", Role: chplan.RoleTimestamp},
				{Name: "value", Role: chplan.RoleValue},
			},
		},
		{
			name: "missing attributes",
			roles: []chplan.Column{
				{Name: "metric", Role: chplan.RoleMetricName},
				{Name: "ts", Role: chplan.RoleTimestamp},
				{Name: "value", Role: chplan.RoleValue},
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := phase3RangeLWRRoleProject(tc.roles...)
			if got := rangeLWRColumns(&chplan.RangeLWR{Input: input}); got != nil {
				t.Fatalf("rangeLWRColumns with a missing required role = %v, want nil (declined)", got)
			}
		})
	}
}
