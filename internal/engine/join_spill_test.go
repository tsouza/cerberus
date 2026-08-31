package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestPlanHasJoin_DetectsEveryJoinNode covers every join-bearing chplan node
// planHasJoin claims to find (see its own doc for the list and the tracked
// late-materialisation gap, #2816): each one alone is enough to trip the
// detector, and a plain non-join plan does not.
func TestPlanHasJoin_DetectsEveryJoinNode(t *testing.T) {
	cases := []struct {
		name string
		plan chplan.Node
	}{
		{"VectorJoin (PromQL vector matching)", &chplan.VectorJoin{}},
		{"HistogramVectorJoin (group_left/group_right)", &chplan.HistogramVectorJoin{}},
		{"HistogramFloatVectorJoin", &chplan.HistogramFloatVectorJoin{}},
		{"MixedVectorJoin", &chplan.MixedVectorJoin{}},
		{"InfoJoin (info())", &chplan.InfoJoin{}},
		{"StructuralJoin (TraceQL)", &chplan.StructuralJoin{}},
		{"CrossJoin", &chplan.CrossJoin{}},
		{
			"RangeWindow with DeltaPrefixAggregateInput (delta-prefix LEFT JOIN)",
			&chplan.RangeWindow{
				Input:                     &chplan.Scan{Table: "otel_metrics_sum"},
				DeltaPrefixAggregateInput: &chplan.Scan{Table: "otel_metrics_sum_delta_prefix"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !planHasJoin(tc.plan) {
				t.Errorf("planHasJoin(%s) = false; want true", tc.name)
			}
		})
	}
}

// TestPlanHasJoin_NonJoinPlansUnaffected pins the negative side: a bare scan,
// an aggregation, and a RangeWindow with NO delta-prefix side feed all report
// no join, so join_spill never fires on an ordinary query.
func TestPlanHasJoin_NonJoinPlansUnaffected(t *testing.T) {
	cases := []struct {
		name string
		plan chplan.Node
	}{
		{"bare Scan", &chplan.Scan{Table: "otel_metrics_sum"}},
		{"Aggregate over Scan", aggOverScan("otel_metrics_sum", "MetricName")},
		{
			"RangeWindow with no delta-prefix side feed",
			&chplan.RangeWindow{Input: &chplan.Scan{Table: "otel_metrics_sum"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if planHasJoin(tc.plan) {
				t.Errorf("planHasJoin(%s) = true; want false", tc.name)
			}
		})
	}
}

// TestPlanHasJoin_ReachesJoinNestedInScalarSubquery pins the WalkDeep (not
// Walk) choice: a join buried inside a Filter predicate's ScalarSubquery is
// still found, the same total-traversal reasoning planHasMetricsCompare
// already relies on.
func TestPlanHasJoin_ReachesJoinNestedInScalarSubquery(t *testing.T) {
	plan := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.ScalarSubquery{Input: &chplan.VectorJoin{}},
	}
	if !planHasJoin(plan) {
		t.Error("planHasJoin: join nested inside a ScalarSubquery predicate not found; want true")
	}
}

// TestApplyJoinSpillSettings_StampsOnlyWhenEnabledAndJoinBearing exercises the
// full gate: BOTH the resolved join_spill verdict AND a join-bearing plan are
// required, mirroring applyCompareMemoryBound's plan-shape-gated sibling.
func TestApplyJoinSpillSettings_StampsOnlyWhenEnabledAndJoinBearing(t *testing.T) {
	// validatedJoinSpillBytes is half of testQueryMemoryCap, the same
	// cap-relative arithmetic applySpillSettings' group_by/sort stamps use.
	const validatedJoinSpillBytes int64 = 1 << 30

	joinPlan := &chplan.VectorJoin{}
	nonJoinPlan := &chplan.Scan{Table: "otel_metrics_sum"}

	cases := []struct {
		name       string
		plan       chplan.Node
		enabled    bool
		wantStamp  bool
		wantReason string
	}{
		{"enabled + join-bearing: stamped", joinPlan, true, true, ""},
		{"enabled + non-join: not stamped (no join to spill)", nonJoinPlan, true, false, ""},
		{"disabled (server < 26.4) + join-bearing: not stamped", joinPlan, false, false, ""},
		{"disabled + non-join: not stamped", nonJoinPlan, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := applyJoinSpillSettings(context.Background(), tc.plan, testQueryMemoryCap, tc.enabled)
			got := settingValue(ctx, settingMaxBytesBeforeExternalJoin)
			if tc.wantStamp {
				if got != validatedJoinSpillBytes {
					t.Errorf("max_bytes_before_external_join = %v; want %v (half the %d-byte cap)", got, validatedJoinSpillBytes, testQueryMemoryCap)
				}
			} else if got != nil {
				t.Errorf("max_bytes_before_external_join = %v; want absent", got)
			}
		})
	}
}

// TestApplyJoinSpillSettings_CapRelative pins the threshold itself to
// spillThreshold's cap-relative arithmetic (not a literal), so a future
// re-derivation of spillThreshold is automatically reflected here too.
func TestApplyJoinSpillSettings_CapRelative(t *testing.T) {
	const cap6GiB = 6 * gib
	ctx := applyJoinSpillSettings(context.Background(), &chplan.VectorJoin{}, cap6GiB, true)
	want := spillThreshold(cap6GiB)
	if got := settingValue(ctx, settingMaxBytesBeforeExternalJoin); got != want {
		t.Errorf("max_bytes_before_external_join = %v; want %v (spillThreshold(%d))", got, want, cap6GiB)
	}
}
