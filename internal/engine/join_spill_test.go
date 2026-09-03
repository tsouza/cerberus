package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Join-carrier detection itself (which chplan.Node kinds count, and the
// WalkDeep-reaches-a-ScalarSubquery proof) is chplan.HasJoin's own contract,
// pinned by internal/chplan/join_test.go — applyJoinSpillSettings only needs
// to prove it GATES on that verdict correctly, not re-prove the verdict.

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
