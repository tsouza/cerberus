package config

import (
	"strings"
	"testing"
)

// TestFromEnv_ExperimentalDistributedMode_GatesDataShards pins the opt-in
// contract for the EXPERIMENTAL multi-shard path (epic #3074): a data-shard
// count above 1 refuses to boot unless CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE
// is explicitly true, so nobody lands on Distributed-table routing by setting
// the shard count alone; the default count (1) never consults the flag, and
// the flag alone changes nothing.
func TestFromEnv_ExperimentalDistributedMode_GatesDataShards(t *testing.T) {
	cases := []struct {
		name        string
		shards      string
		flag        string
		wantErr     bool
		wantEnabled bool
		wantShards  int
	}{
		{"default: one shard, flag unset", "", "", false, false, 1},
		{"two shards without the flag is refused", "2", "", true, false, 0},
		{"two shards with flag=false is refused", "2", "false", true, false, 0},
		{"two shards with flag=true boots", "2", "true", false, true, 2},
		{"four shards with flag=true boots", "4", "true", false, true, 4},
		{"flag=true alone (one shard) is harmless", "", "true", false, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_DATA_SHARDS", tc.shards)
			t.Setenv("CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE", tc.flag)
			cfg, err := FromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatal("FromEnv: want error, got nil")
				}
				if !strings.Contains(err.Error(), "CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE") {
					t.Fatalf("FromEnv error must name the opt-in flag so the operator knows how to proceed; got: %v", err)
				}
				if !strings.Contains(err.Error(), "EXPERIMENTAL") {
					t.Fatalf("FromEnv error must say the path is EXPERIMENTAL; got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ExperimentalDistributedMode != tc.wantEnabled {
				t.Errorf("ExperimentalDistributedMode = %v; want %v", cfg.ExperimentalDistributedMode, tc.wantEnabled)
			}
			if cfg.ClusterTopology.DataShardCount != tc.wantShards {
				t.Errorf("ClusterTopology.DataShardCount = %d; want %d", cfg.ClusterTopology.DataShardCount, tc.wantShards)
			}
		})
	}
}

// TestFromEnv_ExperimentalDistributedMode_Invalid confirms a malformed
// boolean fails fast at startup rather than silently defaulting — the same
// contract CERBERUS_EXPERIMENTAL_TS_GRID_RANGE already has.
func TestFromEnv_ExperimentalDistributedMode_Invalid(t *testing.T) {
	t.Setenv("CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE", "maybe")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
	}
}
