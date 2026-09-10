package spec

import (
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestFormatQuerySettings(t *testing.T) {
	const queryLimit = int64(20)
	plan := &chplan.Limit{
		Count: queryLimit,
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_traces"},
			Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
		},
	}
	got := FormatQuerySettings(plan)
	const enabledOptions = "enabled_opts=aggregation_in_order,condition_cache,exp_histogram_two_level,join_spill,lazy_materialization,result_cache,trace_id_bitmap_filter\n"
	if !strings.Contains(got, enabledOptions) {
		t.Errorf("settings do not record the resolved feature profile:\n%s", got)
	}
	if !strings.Contains(got, "query_plan_optimize_lazy_materialization=1\n") {
		t.Fatalf("settings probe did not activate on the eligible shape:\n%s", got)
	}
	if lines := strings.Split(strings.TrimSpace(got), "\n"); !sort.StringsAreSorted(lines) {
		t.Errorf("settings are not sorted:\n%s", got)
	}
	if again := FormatQuerySettings(plan); again != got {
		t.Errorf("settings changed between probes:\n%s\n%s", got, again)
	}
}
