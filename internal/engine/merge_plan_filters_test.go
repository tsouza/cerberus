package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// eqString is the predicate column = value.
func eqString(column, value string) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: column}, Right: &chplan.LitString{V: value}}
}

// project is a Project over input defining alias as the column source.
func project(input chplan.Node, alias, source string) *chplan.Project {
	return &chplan.Project{Input: input, Projections: []chplan.Projection{{Alias: alias, Expr: &chplan.ColumnRef{Name: source}}}}
}

// rowStage is the part of a LogQL range aggregation's per-row stage the
// table is read in: the scan's scope defines _logql_level_source, and the
// Project marked chplan.Project.OneScopeRows reads it into
// _logql_detected_level.
func rowStage(oneScope bool) *chplan.Project {
	scope := project(&chplan.Scan{Table: "otel_logs"}, "_logql_level_source", "SeverityText")
	marked := project(scope, "_logql_detected_level", "_logql_level_source")
	marked.OneScopeRows = oneScope
	return marked
}

// levelOverParsedScope is a per-row stage whose scan scope defines a
// parser's labels, whose marked Project defines the level source, and
// whose next Project normalises it.
func levelOverParsedScope() chplan.Node {
	scope := project(&chplan.Scan{Table: "otel_logs"}, "_logql_labels_1", "Body")
	marked := project(scope, chplan.LogLevelSourceAlias, "SeverityText")
	marked.OneScopeRows = true
	return project(marked, chplan.DetectedLevelAlias, chplan.LogLevelSourceAlias)
}

// levelOverLineFilter is a per-row stage whose scan scope filters on the
// line and defines the level source, and whose marked Project normalises it.
func levelOverLineFilter() chplan.Node {
	scope := project(&chplan.Filter{Input: &chplan.Scan{Table: "otel_logs"}, Predicate: eqString("Body", "status=500")}, chplan.LogLevelSourceAlias, "SeverityText")
	marked := project(scope, chplan.DetectedLevelAlias, chplan.LogLevelSourceAlias)
	marked.OneScopeRows = true
	return marked
}

// unwrapFilterPlan is #4431's shape 1: the unwrap source is computed over
// the marked Project, and a Filter above reads it.
func unwrapFilterPlan(oneScope bool) chplan.Node {
	source := project(rowStage(oneScope), "_logql_unwrap_source", "Body")
	return &chplan.Filter{Input: source, Predicate: &chplan.Binary{Op: chplan.OpNe, Left: &chplan.ColumnRef{Name: "_logql_unwrap_source"}, Right: &chplan.LitString{V: ""}}}
}

// TestApply_MergePlanFilters_StampsAFilterOnAValueAboveTheScanScope: with
// the rule on, which auto resolves on the releases that have
// query_plan_merge_filters off by default, a plan whose per-row stage leaves
// a filter on a value computed above the scan's scope — over the marked
// Project, or by the marked Project itself — gets the setting at 1. So does a
// filter on a level value alone where the stage under it also computes a
// parser's labels.
func TestApply_MergePlanFilters_StampsAFilterOnAValueAboveTheScanScope(t *testing.T) {
	for name, plan := range map[string]chplan.Node{
		"value over the marked project": unwrapFilterPlan(true),
		// A parser's labels stay in the scope and the level source is the
		// marked Project's, so the normalised level sits over it.
		"normalised level over a parsed scope": &chplan.Filter{Input: levelOverParsedScope(), Predicate: eqString(chplan.DetectedLevelAlias, "error")},
		"level source over a parsed scope":     &chplan.Filter{Input: levelOverParsedScope(), Predicate: eqString(chplan.LogLevelSourceAlias, "error")},
		"value of the marked project": func() chplan.Node {
			marked := project(&chplan.Scan{Table: "otel_logs"}, "_logql_unwrap_source", "Body")
			marked.OneScopeRows = true
			return &chplan.Filter{Input: marked, Predicate: eqString("_logql_unwrap_source", "500")}
		}(),
		// A filter on a scan-scope value, which on its own gets no stamp,
		// sits between the status filter and the marked Project.
		"value over another filter": &chplan.Filter{
			Input: project(
				&chplan.Filter{Input: rowStage(true), Predicate: eqString("_logql_level_source", "error")},
				"_logql_unwrap_labels", "_logql_level_source",
			),
			Predicate: eqString("_logql_unwrap_labels", "500"),
		},
		"value a replacement computes": &chplan.Filter{
			Input: &chplan.Project{
				Input:        rowStage(true),
				Replacements: []chplan.Projection{{Alias: "LogAttributes", Expr: &chplan.ColumnRef{Name: "_logql_detected_level"}}},
			},
			Predicate: eqString("LogAttributes", "error"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := SettingsRules{MergePlanFilters: true}.apply(context.Background(), plan)
			if got := settingValue(ctx, settingQueryPlanMergeFilters); got != 1 {
				t.Errorf("query_plan_merge_filters = %v; want 1", got)
			}
		})
	}
}

// TestApply_MergePlanFilters_LeavesOtherPlansAlone: the rule stamps nothing
// when it is off, on a plan with no LogQL per-row stage, on a per-row stage
// whose filters read only values the scan's scope defines (#4469), or the
// level in a stage that computes nothing else, or on a per-row stage with no
// filter above its scan scope: the range window's own
// time bound is the emitter's, never a plan Filter, so such a plan has none.
func TestApply_MergePlanFilters_LeavesOtherPlansAlone(t *testing.T) {
	cases := []struct {
		name  string
		rules SettingsRules
		plan  chplan.Node
	}{
		{name: "rule off", rules: SettingsRules{}, plan: unwrapFilterPlan(true)},
		{name: "no per-row stage", rules: SettingsRules{MergePlanFilters: true}, plan: unwrapFilterPlan(false)},
		{
			name:  "filter on a scan-scope value",
			rules: SettingsRules{MergePlanFilters: true},
			plan:  &chplan.Filter{Input: rowStage(true), Predicate: eqString("_logql_level_source", "error")},
		},
		{
			// The detected_level filter a range aggregation leaves over the
			// scope while the WHERE-order fact is unset.
			name:  "filter on the normalised level of the marked project",
			rules: SettingsRules{MergePlanFilters: true},
			plan:  &chplan.Filter{Input: rowStage(true), Predicate: eqString(chplan.DetectedLevelAlias, "error")},
		},
		{
			// The same matcher over a line filter in the scan's scope.
			name:  "filter on the normalised level over a line filter",
			rules: SettingsRules{MergePlanFilters: true},
			plan:  &chplan.Filter{Input: levelOverLineFilter(), Predicate: eqString(chplan.DetectedLevelAlias, "error")},
		},
		{
			name:  "filter on a value passed through the marked project",
			rules: SettingsRules{MergePlanFilters: true},
			plan: &chplan.Filter{
				Input:     &chplan.Project{Input: rowStage(true), Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "_logql_level_source"}}}},
				Predicate: eqString("_logql_level_source", "error"),
			},
		},
		{name: "no filter above the per-row stage", rules: SettingsRules{MergePlanFilters: true}, plan: project(rowStage(true), "Value", "_logql_detected_level")},
		{
			name:  "filter above the aggregation",
			rules: SettingsRules{MergePlanFilters: true},
			plan: &chplan.Filter{
				Input:     &chplan.Aggregate{Input: project(rowStage(true), "Value", "_logql_detected_level")},
				Predicate: eqString("Value", "error"),
			},
		},
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
