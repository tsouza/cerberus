package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

const (
	mib int64 = 1 << 20
	gib int64 = 1 << 30
)

// TestApplySpillSettings_StampsBothThresholds — every data-plane query gets
// BOTH the external-group-by and external-sort spill thresholds stamped at the
// cap-relative spill threshold, so a heavy GROUP BY or sort spills instead of
// OOMing. Reverting the cap-relative derivation stamps 512 MiB here instead of
// half the 2 GiB cap and this fails.
func TestApplySpillSettings_StampsBothThresholds(t *testing.T) {
	t.Parallel()

	const (
		cap2GiB   = 2 * gib
		wantStamp = gib // half the cap
	)
	ctx := applySpillSettings(context.Background(), cap2GiB)
	settings := chclient.QuerySettingsFromContext(ctx)
	for _, key := range []string{settingMaxBytesBeforeExternalGroupBy, settingMaxBytesBeforeExternalSort} {
		got, ok := settings[key]
		if !ok {
			t.Fatalf("settings %v missing %s", settings, key)
		}
		if got != wantStamp {
			t.Errorf("%s = %v (%T); want %d (half the %d-byte cap)", key, got, got, wantStamp, cap2GiB)
		}
	}
}

// TestApplySpillSettings_Unconditional — the spill settings ride EVERY query
// (any head can lower a high-card GROUP BY / large sort), not just compare.
// Even a bare cap with no plan stamps both settings.
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

// TestSpillThresholdBytes_BelowMemCap pins the no-cap fallback below the
// per-query max_memory_usage cerberus configures by default, so a deployment
// whose Client reports no cap still spills before that default aborts the
// query. A future bump of either constant that inverts the ordering surfaces
// loudly here.
func TestSpillThresholdBytes_BelowMemCap(t *testing.T) {
	t.Parallel()

	const defaultMaxMemoryUsage = gib // mirrors config.defaultCHQueryMaxMemory
	if spillThresholdBytes >= defaultMaxMemoryUsage {
		t.Errorf("spill threshold %d >= default max_memory_usage %d; spill must trigger before the cap",
			spillThresholdBytes, defaultMaxMemoryUsage)
	}
}

// TestSpillThreshold_CapRelative pins spillThreshold across the cap regimes.
//
// The load-bearing case is the LARGE cap: a 6 GiB cap must yield half the cap
// (3 GiB), not the 512 MiB no-cap constant. Reverting to a min() against
// spillThresholdBytes stamps 536870912 there and this test fails. Over-spilling
// is not a free safety margin — it forces a disk-backed merge on aggregations
// the cap had ample room for.
//
// The lowered cap (256 MiB -> 128 MiB) pins the other end: the threshold still
// lands strictly below a cap smaller than the no-cap constant, so the spill
// fires before MEMORY_LIMIT_EXCEEDED. That is the safety property, and
// cap/spillCapDenominator satisfies it on its own.
func TestSpillThreshold_CapRelative(t *testing.T) {
	t.Parallel()

	const (
		largeCap      = 6 * gib
		largeExpect   = 3 * gib   // 3221225472 — half of 6 GiB
		loweredCap    = 256 * mib // cap smaller than the no-cap constant
		loweredExpect = 128 * mib // 256 MiB / spillCapDenominator
		boundaryCap   = gib       // cap/2 == spillThresholdBytes exactly
		boundaryWant  = 512 * mib
	)

	cases := []struct {
		name      string
		maxMemory int64
		want      int64
	}{
		{"large cap spills at half the cap, not the no-cap constant", largeCap, largeExpect},
		{"lowered cap spills at half the cap", loweredCap, loweredExpect},
		{"cap where half coincides with the no-cap constant", boundaryCap, boundaryWant},
		{"no cap configured uses the absolute fallback", 0, spillThresholdBytes},
		{"negative cap uses the absolute fallback (spill never disabled)", -1, spillThresholdBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := spillThreshold(tc.maxMemory); got != tc.want {
				t.Errorf("spillThreshold(%d) = %d; want %d", tc.maxMemory, got, tc.want)
			}
		})
	}
}

// TestSpillThreshold_StrictlyBelowEveryCap asserts the invariant the whole
// function exists for, as a property over a range of caps rather than at the
// enumerated points: for EVERY positive cap the stamped threshold sits strictly
// below the cap, so the operation always reaches the spill before it reaches
// MEMORY_LIMIT_EXCEEDED. It also pins the threshold at exactly half the cap, so
// any re-introduction of a fixed ceiling (which would flatten the sweep to a
// constant above 1 GiB) fails here rather than silently under-spilling.
//
// The sweep spans five orders of magnitude around the deployed range, plus an
// odd cap where integer division truncates. Its floor is sweepFloorCap: below a
// couple of bytes cap/2 truncates to 0, and 0 is ClickHouse's encoding for
// "spill disabled" rather than a threshold, so the invariant is only meaningful
// for caps a deployment could plausibly configure.
func TestSpillThreshold_StrictlyBelowEveryCap(t *testing.T) {
	t.Parallel()

	const (
		// An offset that makes a cap non-power-of-two so cap/2 truncates.
		oddCapOffset  int64 = 12345
		sweepFloorCap       = mib
	)

	caps := []int64{
		sweepFloorCap, 3 * mib,
		64 * mib, 128 * mib, 256 * mib, 512 * mib,
		gib, 2 * gib, 3 * gib, 4 * gib, 6 * gib, 8 * gib,
		16 * gib, 32 * gib, 64 * gib, 128 * gib,
		6*gib + oddCapOffset,
	}
	for _, maxMemory := range caps {
		got := spillThreshold(maxMemory)
		if got >= maxMemory {
			t.Errorf("spillThreshold(%d) = %d; must be strictly < the cap or the spill never fires", maxMemory, got)
		}
		if got <= 0 {
			t.Errorf("spillThreshold(%d) = %d; 0 disables the spill outright", maxMemory, got)
		}
		if want := maxMemory / spillCapDenominator; got != want {
			t.Errorf("spillThreshold(%d) = %d; want %d (cap/%d)", maxMemory, got, want, spillCapDenominator)
		}
	}
}

// TestApplySpillSettings_LoweredCapStampsBelowCap drives the full stamp path
// with a runtime cap LOWERED below the no-cap 512 MiB constant (256 MiB) and
// asserts BOTH effective stamped thresholds are the cap-relative 128 MiB —
// strictly below the cap. A regression to a fixed 512 MiB stamp surfaces here
// as a value at/above the cap.
func TestApplySpillSettings_LoweredCapStampsBelowCap(t *testing.T) {
	t.Parallel()

	const (
		capBytes  = 256 * mib
		wantStamp = 128 * mib
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
