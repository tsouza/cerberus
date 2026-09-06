package config

import (
	"strings"
	"testing"
)

// TestFromEnv_DataShardFanoutCap_MustCoverShardWidth pins the boundary
// check for the data-shard fan-out gate's cap: every dispatch acquires the
// gate with weight DataShardCount, and a semaphore never admits a weight
// above its size, so an effective cap below the width would time every
// query out. Both cap sources are covered — the explicit override and the
// MaxOpenConns default the gate falls back to — and the single-shard
// default never consults the rule at all.
func TestFromEnv_DataShardFanoutCap_MustCoverShardWidth(t *testing.T) {
	cases := []struct {
		name         string
		shards       string
		override     string
		maxOpenConns string
		wantErrNames string
	}{
		{"single shard: any cap is fine", "", "1", "", ""},
		{"single shard: tiny pool is fine", "", "", "1", ""},
		{"override equal to the width boots", "4", "4", "", ""},
		{"override above the width boots", "4", "8", "", ""},
		{"override below the width is refused, naming the override", "4", "3", "", "CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP"},
		{"derived pool cap below the width is refused, naming the pool size", "4", "", "3", "CERBERUS_CH_MAX_OPEN_CONNS"},
		{"derived pool cap equal to the width boots", "4", "", "4", ""},
		{"override rescues an undersized pool", "4", "4", "2", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_DATA_SHARDS", tc.shards)
			t.Setenv("CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP", tc.override)
			t.Setenv("CERBERUS_CH_MAX_OPEN_CONNS", tc.maxOpenConns)
			t.Setenv("CERBERUS_EXPERIMENTAL_DISTRIBUTED_MODE", "true")
			_, err := FromEnv()
			if tc.wantErrNames == "" {
				if err != nil {
					t.Fatalf("FromEnv: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("FromEnv: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrNames) {
				t.Fatalf("FromEnv error must name the cap source the operator has to raise (%s); got: %v", tc.wantErrNames, err)
			}
			if !strings.Contains(err.Error(), "CERBERUS_CH_DATA_SHARDS") {
				t.Fatalf("FromEnv error must name the shard-count variable it is compared against; got: %v", err)
			}
		})
	}
}
