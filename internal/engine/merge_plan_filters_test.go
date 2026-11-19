package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// logRowStagePlan is the shape a LogQL range aggregation's per-row stage
// lowers to: a Filter on a value computed above the Project that reads the
// stage in the SELECT over the table (chplan.Project.OneScopeRows).
func logRowStagePlan(oneScope bool) chplan.Node {
	stage := &chplan.Project{
		Input:        &chplan.Scan{Table: "otel_logs"},
		Projections:  []chplan.Projection{{Alias: "_logql_unwrap_source", Expr: &chplan.ColumnRef{Name: "Body"}}},
		OneScopeRows: oneScope,
	}
	return &chplan.Filter{
		Input:     stage,
		Predicate: &chplan.Binary{Op: chplan.OpNe, Left: &chplan.ColumnRef{Name: "_logql_unwrap_source"}, Right: &chplan.LitString{V: ""}},
	}
}

// TestApply_MergePlanFilters_StampsALogRowStage: with the rule on, which
// auto resolves on the releases that have query_plan_merge_filters off by
// default, a plan carrying a LogQL per-row stage gets the setting at 1.
func TestApply_MergePlanFilters_StampsALogRowStage(t *testing.T) {
	ctx := SettingsRules{MergePlanFilters: true}.apply(context.Background(), logRowStagePlan(true))
	if got := settingValue(ctx, settingQueryPlanMergeFilters); got != 1 {
		t.Errorf("query_plan_merge_filters = %v; want 1", got)
	}
}

// TestApply_MergePlanFilters_LeavesOtherPlansAlone: the rule stamps nothing
// when it is off or on a plan with no LogQL per-row stage.
func TestApply_MergePlanFilters_LeavesOtherPlansAlone(t *testing.T) {
	cases := []struct {
		name  string
		rules SettingsRules
		plan  chplan.Node
	}{
		{name: "rule off", rules: SettingsRules{}, plan: logRowStagePlan(true)},
		{name: "no per-row stage", rules: SettingsRules{MergePlanFilters: true}, plan: logRowStagePlan(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.rules.apply(context.Background(), tc.plan)
			if got := settingValue(ctx, settingQueryPlanMergeFilters); got != nil {
				t.Errorf("query_plan_merge_filters = %v; want absent", got)
			}
		})
	}
}
