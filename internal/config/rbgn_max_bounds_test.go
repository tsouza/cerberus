package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestResolveRBGNMaxRows_ZeroFloor and its density-axis twin below mirror
// TestResolveQueryMaxSamples_ZeroFloorAndLoudDisable's own shape (issue
// #2665): 0 -> coerced to the real-evidence-calibrated default, with a
// warning (never silently disables the resource-safety bound — see
// resolveRBGNMaxRows's own doc for why there is no disable sentinel here,
// unlike CERBERUS_QUERY_MAX_SAMPLES); negative -> rejected outright.
func TestResolveRBGNMaxRows_ZeroFloor(t *testing.T) {
	cases := []struct {
		name     string
		in       int64
		want     int64
		warnSubs string
		isErr    bool
	}{
		{"positive passes through", 123, 123, "", false},
		{"zero coerces to default + warns", 0, defaultRBGNMaxRows, "does not disable", false},
		{"negative rejected", -1, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, err := resolveRBGNMaxRows(tc.in)
			if tc.isErr {
				if err == nil {
					t.Fatalf("resolveRBGNMaxRows(%d) = (%d, nil); want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRBGNMaxRows(%d): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %d; want %d", got, tc.want)
			}
			logged := buf.String()
			if tc.warnSubs == "" {
				if strings.Contains(logged, "level=WARN") {
					t.Errorf("unexpected warning for input %d: %q", tc.in, logged)
				}
				return
			}
			if !strings.Contains(logged, tc.warnSubs) {
				t.Errorf("warning %q does not contain %q", logged, tc.warnSubs)
			}
		})
	}
}

func TestResolveRBGNMaxDensityUnits_ZeroFloor(t *testing.T) {
	cases := []struct {
		name     string
		in       int64
		want     int64
		warnSubs string
		isErr    bool
	}{
		{"positive passes through", 456, 456, "", false},
		// 0 is the documented "derive from the query memory cap" sentinel
		// (#2681): the cost units this bound counts are a proxy for BYTES, so
		// a fixed default cannot fit both the 1 GiB default cap and the
		// multi-GiB caps real deployments run. No warning — deriving IS the
		// default path, not a coercion away from something the operator asked
		// for.
		{"zero derives from the memory cap", 0, rbgnDensityUnitsForMemory(testCHQueryMaxMemory), "", false},
		{"negative rejected", -5, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, err := resolveRBGNMaxDensityUnits(tc.in, testCHQueryMaxMemory)
			if tc.isErr {
				if err == nil {
					t.Fatalf("resolveRBGNMaxDensityUnits(%d) = (%d, nil); want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRBGNMaxDensityUnits(%d): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %d; want %d", got, tc.want)
			}
			logged := buf.String()
			if tc.warnSubs == "" {
				if strings.Contains(logged, "level=WARN") {
					t.Errorf("unexpected warning for input %d: %q", tc.in, logged)
				}
				return
			}
			if !strings.Contains(logged, tc.warnSubs) {
				t.Errorf("warning %q does not contain %q", logged, tc.warnSubs)
			}
		})
	}
}

// TestFromEnv_RBGNMaxBounds is an end-to-end pin: setting
// CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_ROWS /
// …MAX_DENSITY_UNITS via the environment reaches Config's own fields
// through FromEnv, and leaving them unset resolves to the same
// real-evidence-calibrated defaults chsql itself falls back to.
func TestFromEnv_RBGNMaxBounds(t *testing.T) {
	t.Run("unset resolves to defaults", func(t *testing.T) {
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.RangeBucketGridNativeMaxRows != defaultRBGNMaxRows {
			t.Errorf("RangeBucketGridNativeMaxRows = %d; want default %d", cfg.RangeBucketGridNativeMaxRows, defaultRBGNMaxRows)
		}
		// The density bound is DERIVED from the resolved query memory cap
		// rather than fixed (#2681) — see rbgnDensityUnitsForMemory for the
		// measurements that forced that, and why one constant could not serve
		// both the default cap and the caps real deployments run.
		wantDensity := rbgnDensityUnitsForMemory(defaultCHQueryMaxMemory)
		if cfg.RangeBucketGridNativeMaxDensityUnits != wantDensity {
			t.Errorf("RangeBucketGridNativeMaxDensityUnits = %d; want %d derived from the %d-byte query memory cap",
				cfg.RangeBucketGridNativeMaxDensityUnits, wantDensity, defaultCHQueryMaxMemory)
		}
	})

	t.Run("explicit values pass through", func(t *testing.T) {
		t.Setenv("CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_ROWS", "50000000")
		t.Setenv("CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_DENSITY_UNITS", "800000000")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.RangeBucketGridNativeMaxRows != 50_000_000 {
			t.Errorf("RangeBucketGridNativeMaxRows = %d; want 50000000", cfg.RangeBucketGridNativeMaxRows)
		}
		if cfg.RangeBucketGridNativeMaxDensityUnits != 800_000_000 {
			t.Errorf("RangeBucketGridNativeMaxDensityUnits = %d; want 800000000", cfg.RangeBucketGridNativeMaxDensityUnits)
		}
	})

	t.Run("negative value rejected", func(t *testing.T) {
		t.Setenv("CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_ROWS", "-1")
		_, err := FromEnv()
		if err == nil {
			t.Fatal("FromEnv accepted a negative CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_ROWS; want error")
		}
		if !strings.Contains(err.Error(), "CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_ROWS") {
			t.Errorf("error %q does not name the env var", err)
		}
	})
}

// testCHQueryMaxMemory is the per-query ClickHouse memory cap the density
// derivation is exercised against here — the 1 GiB CERBERUS_CH_QUERY_MAX_MEMORY
// default, so the table above pins the value a stock deployment actually gets.
const testCHQueryMaxMemory int64 = defaultCHQueryMaxMemory

// TestRBGNDensityUnitsForMemory_TracksTheMeasuredCliff pins the derivation
// against the real-ClickHouse sweep tabulated on rbgnDensityUnitsForMemory.
//
// The property that matters is not the exact constant but the RELATION: the
// bound must sit BELOW the measured cliff at every cap (or the guard admits
// queries that then die on ClickHouse's own limit — the #2677 production
// failure) while staying within an order of magnitude of it (or the guard
// rejects work that would have run fine). A fixed constant satisfied neither
// end simultaneously, which is the defect this replaces.
func TestRBGNDensityUnitsForMemory_TracksTheMeasuredCliff(t *testing.T) {
	const gib = int64(1) << 30
	// measuredCliff is the lowest cost-unit value at which real ClickHouse
	// 26.6 answered MEMORY_LIMIT_EXCEEDED at that cap, across the width sweep.
	cases := []struct {
		name         string
		cap          int64
		measuredClif int64
	}{
		{"1GiB default cap", gib, 11_698_720},
		{"6GiB production cap", 6 * gib, 98_269_248},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rbgnDensityUnitsForMemory(tc.cap)
			if got >= tc.measuredClif {
				t.Errorf("bound %d is at or above the measured OOM cliff %d at a %d-byte cap — "+
					"the guard would admit queries that then die on ClickHouse's own memory limit, "+
					"which is exactly the failure this derivation replaces",
					got, tc.measuredClif, tc.cap)
			}
			if got*10 < tc.measuredClif {
				t.Errorf("bound %d is more than 10x below the measured cliff %d at a %d-byte cap — "+
					"that rejects work that would have run, well past the deliberate conservatism",
					got, tc.measuredClif, tc.cap)
			}
		})
	}
}

// TestRBGNDensityUnitsForMemory_ScalesAndFloors pins the two structural
// properties the table above cannot: the bound RISES with the cap (a fixed
// constant is what this replaces), and a sub-GiB cap still gets a usable
// floor rather than collapsing to zero and rejecting everything.
func TestRBGNDensityUnitsForMemory_ScalesAndFloors(t *testing.T) {
	const gib = int64(1) << 30
	if small, large := rbgnDensityUnitsForMemory(gib), rbgnDensityUnitsForMemory(8*gib); small >= large {
		t.Errorf("bound did not grow with the memory cap: 1GiB -> %d, 8GiB -> %d", small, large)
	}
	for _, cap := range []int64{0, -1, gib / 64} {
		if got := rbgnDensityUnitsForMemory(cap); got < rbgnDensityUnitsFloor {
			t.Errorf("cap %d produced %d, below the floor %d — a tiny or unset cap must still "+
				"leave a usable bound rather than rejecting every query", cap, got, rbgnDensityUnitsFloor)
		}
	}
}
