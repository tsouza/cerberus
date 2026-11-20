package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// mergePlanFiltersStamp is the query_plan_merge_filters value the dispatch
// seam stamps on the range aggregation q, lowered and optimized with the
// WHERE-order fact set to whereOrder, under a fleet that resolves the
// merge_plan_filters rule in: nil when it stamps none.
func mergePlanFiltersStamp(t *testing.T, q string, whereOrder bool) any {
	t.Helper()
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	l := &logql.Lang{
		Schema: schema.DefaultOTelLogs(), Start: start, End: start.Add(24 * time.Hour), Step: time.Hour,
		Strategies: chplan.SchemaStrategies{PrewhereInWhereOrder: whereOrder},
	}
	dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, q)
	if err != nil {
		t.Fatalf("DryRunSQL(%q): %v", q, err)
	}
	rules := engine.SettingsRules{
		MergePlanFilters: true,
		Metrics:          schema.DefaultOTelMetrics(),
		Traces:           schema.DefaultOTelTraces(),
		Logs:             schema.DefaultOTelLogs(),
	}
	return engine.SharedQuerySettingsForTest(rules, dr.Plan)[chopt.SettingQueryPlanMergeFilters]
}

// TestMergePlanFiltersStampWithholdsLevelFiltersUnderEitherWhereOrder: a
// range aggregation whose per-row stage computes nothing but the detected
// level gets no query_plan_merge_filters, with the WHERE-order fact set and
// with it unset. cmd/cerberus sets that fact only once a fleet pass has read
// every node, so a 24.8 fleet runs with it unset first. With it unset the
// lowering leaves a detected_level filter over the scan's scope, on the
// normalised level; with it set the scope answers the filter on the level
// source. A stage that also parses the line keeps the stamp under either
// fact, its level filter included, and so does #4431's shape 1, whose status
// filter reads the labels its unwrap computes.
func TestMergePlanFiltersStampWithholdsLevelFiltersUnderEitherWhereOrder(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  any
	}{
		{`sum by (detected_level) (count_over_time({service_name="tempo"} | detected_level=~"warn|error" [5m]))`, nil},
		{`sum by (detected_level) (count_over_time({service_name="tempo"} | detected_level!~"info" [5m]))`, nil},
		{`sum(count_over_time({service_name="tempo"} | detected_level="error" |= "status=500" [5m]))`, nil},
		{`sum(rate({service_name="tempo"} | detected_level=~"error|fatal" |= "status=500" [1m]))`, nil},
		{`sum(count_over_time({service_name="tempo"} | logfmt | detected_level="error" [5m]))`, nil},
		{`sum(count_over_time({service_name="tempo"} | detected_level="error" | json | status >= 400 [5m]))`, 1},
		{`sum(count_over_time({service_name="tempo"} | detected_level="error" | logfmt | status >= 400 [5m]))`, 1},
		{`sum(count_over_time({service_name="tempo"} |= "x" | detected_level="error" | json | status >= 400 [5m]))`, 1},
		{`sum(count_over_time({service_name="tempo"} | json | status >= 400 | detected_level="error" [5m]))`, 1},
		{`count(sum_over_time({service_name="tempo"} | logfmt | unwrap spans | status > 100 [5m]))`, 1},
		{`count(sum_over_time({service_name="tempo"} | detected_level="error" | logfmt | unwrap spans | status > 100 [5m]))`, 1},
	} {
		for _, whereOrder := range []bool{false, true} {
			if got := mergePlanFiltersStamp(t, tc.query, whereOrder); got != tc.want {
				t.Errorf("%s with the WHERE-order fact %v: query_plan_merge_filters = %v; want %v", tc.query, whereOrder, got, tc.want)
			}
		}
	}
}
