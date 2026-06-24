package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// comparePlan builds a minimal MetricsCompare-rooted plan: the node shape the
// TraceQL compare() lowering produces, wrapped (optionally) so the walk has to
// descend to find it.
func comparePlan() *chplan.MetricsCompare {
	return &chplan.MetricsCompare{
		Selection: &chplan.LitBool{V: true},
		Pairs:     &chplan.FuncCall{Name: "array"},
		Inner:     &chplan.Scan{Table: "otel_traces"},
	}
}

// TestApplyCompareSpill_StampsThreshold — a plan containing a MetricsCompare
// node gets max_bytes_before_external_group_by stamped at the named spill
// threshold so the heavy GROUP BY spills instead of OOMing.
func TestApplyCompareSpill_StampsThreshold(t *testing.T) {
	t.Parallel()

	ctx := applyCompareSpill(context.Background(), comparePlan())
	settings := chclient.QuerySettingsFromContext(ctx)
	got, ok := settings[settingMaxBytesBeforeExternalGroupBy]
	if !ok {
		t.Fatalf("settings %v missing %s", settings, settingMaxBytesBeforeExternalGroupBy)
	}
	if got != compareGroupBySpillBytes {
		t.Errorf("%s = %v (%T); want %d", settingMaxBytesBeforeExternalGroupBy, got, got, compareGroupBySpillBytes)
	}
}

// TestApplyCompareSpill_NestedNode — the rule walks the tree, so a
// MetricsCompare wrapped by a RangeWindow (the matrix / query_range path) is
// still found and the spill setting stamped.
func TestApplyCompareSpill_NestedNode(t *testing.T) {
	t.Parallel()

	wrapped := &chplan.RangeWindow{Input: comparePlan()}
	ctx := applyCompareSpill(context.Background(), wrapped)
	if _, ok := chclient.QuerySettingsFromContext(ctx)[settingMaxBytesBeforeExternalGroupBy]; !ok {
		t.Errorf("nested MetricsCompare: spill setting not stamped")
	}
}

// TestApplyCompareSpill_NonComparePassThrough — a plan with no MetricsCompare
// node returns ctx unchanged: the spill setting never rides an unrelated query.
func TestApplyCompareSpill_NonComparePassThrough(t *testing.T) {
	t.Parallel()

	plan := aggOverScan("otel_traces", "ServiceName")
	ctx := applyCompareSpill(context.Background(), plan)
	if settings := chclient.QuerySettingsFromContext(ctx); settings != nil {
		t.Errorf("non-compare plan carried settings %v; want none stamped", settings)
	}
}

// TestCompareGroupBySpillBytes_BelowMemCap pins the spill threshold below the
// default per-query max_memory_usage cap so the spill triggers before the
// memory cap aborts the query. A future bump of either constant that inverts
// the ordering surfaces loudly here.
func TestCompareGroupBySpillBytes_BelowMemCap(t *testing.T) {
	t.Parallel()

	const defaultMaxMemoryUsage int64 = 1 << 30 // mirrors config.defaultCHQueryMaxMemory
	if compareGroupBySpillBytes >= defaultMaxMemoryUsage {
		t.Errorf("spill threshold %d >= default max_memory_usage %d; spill must trigger before the cap",
			compareGroupBySpillBytes, defaultMaxMemoryUsage)
	}
}
