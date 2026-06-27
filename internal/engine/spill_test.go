package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestApplySpillSettings_StampsBothThresholds — every data-plane query gets
// BOTH the external-group-by and external-sort spill thresholds stamped at the
// spill threshold, so a heavy GROUP BY or sort spills instead of OOMing.
func TestApplySpillSettings_StampsBothThresholds(t *testing.T) {
	t.Parallel()

	// High cap (2 GiB) leaves the fixed 512 MiB threshold the smaller of the
	// two, so it is stamped verbatim.
	const highCap int64 = 2 << 30
	ctx := applySpillSettings(context.Background(), highCap)
	settings := chclient.QuerySettingsFromContext(ctx)
	for _, key := range []string{settingMaxBytesBeforeExternalGroupBy, settingMaxBytesBeforeExternalSort} {
		got, ok := settings[key]
		if !ok {
			t.Fatalf("settings %v missing %s", settings, key)
		}
		if got != spillThresholdBytes {
			t.Errorf("%s = %v (%T); want %d", key, got, got, spillThresholdBytes)
		}
	}
}

// TestApplySpillSettings_Unconditional — unlike the old MetricsCompare-only
// scope, the spill settings ride EVERY query (any head can lower a high-card
// GROUP BY / large sort), not just compare. Even a bare cap with no plan stamps
// both settings.
func TestApplySpillSettings_Unconditional(t *testing.T) {
	t.Parallel()

	ctx := applySpillSettings(context.Background(), 0)
	settings := chclient.QuerySettingsFromContext(ctx)
	if _, ok := settings[settingMaxBytesBeforeExternalGroupBy]; !ok {
		t.Errorf("group-by spill setting not stamped on a no-cap query")
	}
	if _, ok := settings[settingMaxBytesBeforeExternalSort]; !ok {
		t.Errorf("sort spill setting not stamped on a no-cap query")
	}
}

// TestSpillThresholdBytes_BelowMemCap pins the spill threshold below the default
// per-query max_memory_usage cap so the spill triggers before the memory cap
// aborts the query. A future bump of either constant that inverts the ordering
// surfaces loudly here.
func TestSpillThresholdBytes_BelowMemCap(t *testing.T) {
	t.Parallel()

	const defaultMaxMemoryUsage int64 = 1 << 30 // mirrors config.defaultCHQueryMaxMemory
	if spillThresholdBytes >= defaultMaxMemoryUsage {
		t.Errorf("spill threshold %d >= default max_memory_usage %d; spill must trigger before the cap",
			spillThresholdBytes, defaultMaxMemoryUsage)
	}
}

// TestSpillThreshold_CapRelative pins spillThreshold across the cap regimes. The
// load-bearing case is the lowered cap (256 MiB, at/below the fixed 512 MiB
// threshold): the effective threshold MUST drop to a cap-relative fraction
// (128 MiB with denominator 2) so the spill still triggers strictly below the
// cap. Without the cap-relative clamp the stamped value would be the fixed
// 512 MiB — at or above the cap — and the operation would OOM instead of
// spilling. This case kills the mutant that reverts spillThreshold to a
// constant return of spillThresholdBytes.
func TestSpillThreshold_CapRelative(t *testing.T) {
	t.Parallel()

	const (
		mib           int64 = 1 << 20
		loweredCap          = 256 * mib // the lowered-cap regime
		loweredExpect       = 128 * mib // 256 MiB / spillCapDenominator
		highCap             = 2 << 30   // 2 GiB — comfortably above the fixed threshold
		atFixedCap          = 1 << 30   // 1 GiB — cap/2 == fixed 512 MiB, so fixed wins (strict <)
	)

	cases := []struct {
		name      string
		maxMemory int64
		want      int64
	}{
		{"lowered cap below fixed threshold spills cap-relative", loweredCap, loweredExpect},
		{"high cap keeps fixed threshold", highCap, spillThresholdBytes},
		{"cap/2 equals fixed threshold keeps fixed", atFixedCap, spillThresholdBytes},
		{"no cap configured keeps fixed threshold", 0, spillThresholdBytes},
		{"negative cap keeps fixed threshold (spill never disabled)", -1, spillThresholdBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := spillThreshold(tc.maxMemory); got != tc.want {
				t.Errorf("spillThreshold(%d) = %d; want %d", tc.maxMemory, got, tc.want)
			}
			// Whenever a positive cap is configured the threshold must sit
			// strictly below it, or the spill can't fire before the OOM.
			if tc.maxMemory > 0 {
				if got := spillThreshold(tc.maxMemory); got >= tc.maxMemory {
					t.Errorf("spillThreshold(%d) = %d; must be strictly < cap", tc.maxMemory, got)
				}
			}
		})
	}
}

// TestApplySpillSettings_LoweredCapStampsBelowCap drives the full stamp path
// with a runtime cap LOWERED below the fixed 512 MiB threshold (256 MiB) and
// asserts BOTH effective stamped thresholds are the cap-relative 128 MiB —
// strictly below the cap. A regression to a fixed 512 MiB stamp would surface
// here as a value at/above the cap.
func TestApplySpillSettings_LoweredCapStampsBelowCap(t *testing.T) {
	t.Parallel()

	const (
		mib       int64 = 1 << 20
		capBytes        = 256 * mib
		wantStamp       = 128 * mib
	)
	settings := chclient.QuerySettingsFromContext(applySpillSettings(context.Background(), capBytes))
	for _, key := range []string{settingMaxBytesBeforeExternalGroupBy, settingMaxBytesBeforeExternalSort} {
		got, ok := settings[key]
		if !ok {
			t.Fatalf("lowered-cap query: %s not stamped", key)
		}
		gotBytes, ok := got.(int64)
		if !ok {
			t.Fatalf("%s = %v (%T); want int64", key, got, got)
		}
		if gotBytes != wantStamp {
			t.Errorf("%s = %d; want %d (cap-relative, cap/2)", key, gotBytes, wantStamp)
		}
		if gotBytes >= capBytes {
			t.Errorf("%s = %d; must be strictly < lowered cap %d", key, gotBytes, capBytes)
		}
	}
}
