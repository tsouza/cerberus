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

	// High cap (2 GiB) leaves the fixed 512 MiB threshold the smaller of the
	// two, so it is stamped verbatim.
	const highCap int64 = 2 << 30
	ctx := applyCompareSpill(context.Background(), comparePlan(), highCap)
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
	ctx := applyCompareSpill(context.Background(), wrapped, 0)
	if _, ok := chclient.QuerySettingsFromContext(ctx)[settingMaxBytesBeforeExternalGroupBy]; !ok {
		t.Errorf("nested MetricsCompare: spill setting not stamped")
	}
}

// TestApplyCompareSpill_NonComparePassThrough — a plan with no MetricsCompare
// node returns ctx unchanged: the spill setting never rides an unrelated query.
func TestApplyCompareSpill_NonComparePassThrough(t *testing.T) {
	t.Parallel()

	plan := aggOverScan("otel_traces", "ServiceName")
	ctx := applyCompareSpill(context.Background(), plan, 0)
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

// TestCompareSpillThreshold_CapRelative pins compareSpillThreshold across the
// cap regimes. The load-bearing case is the lowered cap (256 MiB, at/below the
// fixed 512 MiB threshold): the effective threshold MUST drop to a cap-relative
// fraction (128 MiB with denominator 2) so the spill still triggers strictly
// below the cap. Without the cap-relative clamp the stamped value would be the
// fixed 512 MiB — at or above the cap — and the GROUP BY would OOM instead of
// spilling. This case kills the mutant that reverts compareSpillThreshold to a
// constant return of compareGroupBySpillBytes.
func TestCompareSpillThreshold_CapRelative(t *testing.T) {
	t.Parallel()

	const (
		mib           int64 = 1 << 20
		loweredCap          = 256 * mib // the lowered-cap regime
		loweredExpect       = 128 * mib // 256 MiB / compareSpillCapDenominator
		highCap             = 2 << 30   // 2 GiB — comfortably above the fixed threshold
		atFixedCap          = 1 << 30   // 1 GiB — cap/2 == fixed 512 MiB, so fixed wins (strict <)
	)

	cases := []struct {
		name      string
		maxMemory int64
		want      int64
	}{
		{"lowered cap below fixed threshold spills cap-relative", loweredCap, loweredExpect},
		{"high cap keeps fixed threshold", highCap, compareGroupBySpillBytes},
		{"cap/2 equals fixed threshold keeps fixed", atFixedCap, compareGroupBySpillBytes},
		{"no cap configured keeps fixed threshold", 0, compareGroupBySpillBytes},
		{"negative cap keeps fixed threshold (spill never disabled)", -1, compareGroupBySpillBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := compareSpillThreshold(tc.maxMemory); got != tc.want {
				t.Errorf("compareSpillThreshold(%d) = %d; want %d", tc.maxMemory, got, tc.want)
			}
			// Whenever a positive cap is configured the threshold must sit
			// strictly below it, or the spill can't fire before the OOM.
			if tc.maxMemory > 0 {
				if got := compareSpillThreshold(tc.maxMemory); got >= tc.maxMemory {
					t.Errorf("compareSpillThreshold(%d) = %d; must be strictly < cap", tc.maxMemory, got)
				}
			}
		})
	}
}

// TestApplyCompareSpill_LoweredCapStampsBelowCap drives the full stamp path with
// a runtime cap LOWERED below the fixed 512 MiB threshold (256 MiB) and asserts
// the EFFECTIVE stamped max_bytes_before_external_group_by is the cap-relative
// 128 MiB — strictly below the cap. This is the end-to-end guard for the
// blocker: a regression to a fixed 512 MiB stamp would surface here as a value
// at/above the cap.
func TestApplyCompareSpill_LoweredCapStampsBelowCap(t *testing.T) {
	t.Parallel()

	const (
		mib       int64 = 1 << 20
		capBytes        = 256 * mib
		wantStamp       = 128 * mib
	)
	ctx := applyCompareSpill(context.Background(), comparePlan(), capBytes)
	got, ok := chclient.QuerySettingsFromContext(ctx)[settingMaxBytesBeforeExternalGroupBy]
	if !ok {
		t.Fatalf("lowered-cap compare plan: %s not stamped", settingMaxBytesBeforeExternalGroupBy)
	}
	gotBytes, ok := got.(int64)
	if !ok {
		t.Fatalf("%s = %v (%T); want int64", settingMaxBytesBeforeExternalGroupBy, got, got)
	}
	if gotBytes != wantStamp {
		t.Errorf("%s = %d; want %d (cap-relative, cap/2)", settingMaxBytesBeforeExternalGroupBy, gotBytes, wantStamp)
	}
	if gotBytes >= capBytes {
		t.Errorf("%s = %d; must be strictly < lowered cap %d", settingMaxBytesBeforeExternalGroupBy, gotBytes, capBytes)
	}
}
