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

// TestFromEnv_CHDataShardCount_ThreadsIntoClickHouseConfig — cerberus issue
// #3122: cfg.ClickHouse.DataShardCount (the field Client.querySettings reads
// to apportion MaxQueryMemoryBytes for route A) must mirror
// cfg.ClusterTopology.DataShardCount exactly — the SAME
// CERBERUS_CH_DATA_SHARDS value FromEnv already threads into
// solver.Config.DataShardCount (cerberus issue #3081) via buildSolver.
// Without this, chclient.Client never learns the real shard count and
// route A's own statement stays unapportioned even when the solver's own
// K-shard apportionment is wired correctly.
func TestFromEnv_CHDataShardCount_ThreadsIntoClickHouseConfig(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 1}, // default (chopt.DefaultClusterTopology)
		{"1", 1},
		{"2", 2},
		{"4", 4},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("CERBERUS_CH_DATA_SHARDS", tc.env)
			}
			// Multi-shard routing is EXPERIMENTAL and off by default: a count
			// above 1 only boots with the explicit opt-in (pinned by
			// experimental_distributed_mode_test.go). This test is about the
			// threading, not the gate, so opt in unconditionally.
			t.Setenv("CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE", "true")
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClusterTopology.DataShardCount != tc.want {
				t.Fatalf("ClusterTopology.DataShardCount = %d; want %d (fixture assumption broken)",
					cfg.ClusterTopology.DataShardCount, tc.want)
			}
			if cfg.ClickHouse.DataShardCount != tc.want {
				t.Errorf("ClickHouse.DataShardCount = %d; want %d (must mirror ClusterTopology.DataShardCount)",
					cfg.ClickHouse.DataShardCount, tc.want)
			}
		})
	}
}

// TestFromEnv_DataShardFanoutCapOverride_DefaultsNil — cerberus issue #3128
// moved the data-shard fan-out admission gate's cap-override knob from
// internal/solver's own env parsing to this package (chclient.Config now
// carries it, resolved by chclient.NewDataShardFanoutGate). The historical
// env var name (CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP) is unset by default,
// which must resolve to a nil override, never a duplicated copy of
// MaxOpenConns — that "defaults to MaxOpenConns" behavior lives in
// chclient.NewDataShardFanoutGate, not here.
func TestFromEnv_DataShardFanoutCapOverride_DefaultsNil(t *testing.T) {
	t.Setenv("CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.DataShardFanoutCapOverride != nil {
		t.Errorf("DataShardFanoutCapOverride = %v, want nil (unset means \"use MaxOpenConns\")",
			cfg.ClickHouse.DataShardFanoutCapOverride)
	}
}

// TestFromEnv_DataShardFanoutCapOverride_Threads confirms an explicit
// CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP threads through to
// chclient.Config.DataShardFanoutCapOverride verbatim.
func TestFromEnv_DataShardFanoutCapOverride_Threads(t *testing.T) {
	t.Setenv("CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP", "23")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.DataShardFanoutCapOverride == nil || *cfg.ClickHouse.DataShardFanoutCapOverride != 23 {
		t.Errorf("DataShardFanoutCapOverride = %v; want a pointer to 23", cfg.ClickHouse.DataShardFanoutCapOverride)
	}
}

// TestFromEnv_DataShardFanoutCapOverride_Invalid confirms a non-integer or
// non-positive override fails fast at startup rather than silently
// defaulting or wiring a permanently-empty semaphore.
func TestFromEnv_DataShardFanoutCapOverride_Invalid(t *testing.T) {
	for _, val := range []string{"not-a-number", "0", "-1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
