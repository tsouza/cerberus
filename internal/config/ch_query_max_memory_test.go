package config

import (
	"strings"
	"testing"
)

// TestFromEnv_CHQueryMaxMemory_Default confirms the per-query ClickHouse
// memory cap defaults to 1 GiB (1073741824 bytes) when
// CERBERUS_CH_QUERY_MAX_MEMORY is unset — the bound chosen after k3d
// run 27277793810, where a 24h/15s matrix query demanded 2.12 GiB and
// tripped ClickHouse's server-total cap mid-stream.
func TestFromEnv_CHQueryMaxMemory_Default(t *testing.T) {
	t.Setenv("CERBERUS_CH_QUERY_MAX_MEMORY", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxQueryMemoryBytes != 1073741824 {
		t.Errorf("MaxQueryMemoryBytes = %d; want 1073741824 (1 GiB default)",
			cfg.ClickHouse.MaxQueryMemoryBytes)
	}
}

// TestFromEnv_CHQueryMaxMemory_Override confirms the env var threads
// through to chclient.Config, including the documented 0 = don't-set
// opt-out, the exact raw-integer (BWC) form, and the humanized
// Kubernetes-style sizes (2Gi / 512Mi / 1G / 1k).
func TestFromEnv_CHQueryMaxMemory_Override(t *testing.T) {
	cases := []struct {
		val  string
		want int64
	}{
		// Raw-integer (backward-compatible) form — exact, no float round-trip.
		{"1073741824", 1_073_741_824},
		{"536870912", 536_870_912},
		{"0", 0},
		{"1", 1},
		// Humanized forms (k8s BinarySI): binary Ki/Mi/Gi, decimal k/K/M/G.
		{"2Gi", 2_147_483_648},
		{"512Mi", 536_870_912},
		{"1Gi", 1_073_741_824},
		{"1G", 1_000_000_000},
		{"1k", 1_000},
		{"500Mi", 524_288_000},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_QUERY_MAX_MEMORY", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.MaxQueryMemoryBytes != tc.want {
				t.Errorf("MaxQueryMemoryBytes = %d; want %d",
					cfg.ClickHouse.MaxQueryMemoryBytes, tc.want)
			}
		})
	}
}

// TestFromEnv_CHQueryMaxMemory_Invalid confirms non-integer and negative
// values fail fast at startup rather than silently defaulting.
func TestFromEnv_CHQueryMaxMemory_Invalid(t *testing.T) {
	for _, val := range []string{"1GiB", "1.5", "-1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_QUERY_MAX_MEMORY", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_CH_QUERY_MAX_MEMORY") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
