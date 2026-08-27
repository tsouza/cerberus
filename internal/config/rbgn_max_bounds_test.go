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
		{"zero coerces to default + warns", 0, defaultRBGNMaxDensityUnits, "does not disable", false},
		{"negative rejected", -5, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, err := resolveRBGNMaxDensityUnits(tc.in)
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
		if cfg.RangeBucketGridNativeMaxDensityUnits != defaultRBGNMaxDensityUnits {
			t.Errorf("RangeBucketGridNativeMaxDensityUnits = %d; want default %d", cfg.RangeBucketGridNativeMaxDensityUnits, defaultRBGNMaxDensityUnits)
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
