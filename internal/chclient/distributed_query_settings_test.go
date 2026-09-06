package chclient

import (
	"context"
	"testing"
)

// TestDistributedQuerySettings_ExactNames pins the exact ClickHouse setting
// spellings distributed_query_settings.go relies on, so a future
// server-side rename (or a typo introduced here) surfaces loudly rather
// than silently becoming a no-op unknown setting.
func TestDistributedQuerySettings_ExactNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"settingSkipUnavailableShards", settingSkipUnavailableShards, "skip_unavailable_shards"},
		{"settingFallbackToStaleReplicasForDistributedQueries", settingFallbackToStaleReplicasForDistributedQueries, "fallback_to_stale_replicas_for_distributed_queries"},
		{"settingLoadBalancing", settingLoadBalancing, "load_balancing"},
		{"settingLoadBalancingFirstOffset", settingLoadBalancingFirstOffset, "load_balancing_first_offset"},
		{"loadBalancingFirstOrRandom", loadBalancingFirstOrRandom, "first_or_random"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q; want %q", c.name, c.got, c.want)
		}
	}
}

// TestQuerySettings_LoadBalancingPin — cerberus issue #3086: every
// querySettings() call stamps load_balancing=first_or_random and
// load_balancing_first_offset=0 UNCONDITIONALLY, alongside (never
// clobbering) the pre-existing #3078 distributed-query pins, on both a
// bare client and one with a configured memory cap.
func TestQuerySettings_LoadBalancingPin(t *testing.T) {
	t.Parallel()

	for _, c := range []*Client{{}, {maxMemory: 1 << 30}} {
		s := c.querySettings(context.Background())
		if got := s[settingLoadBalancing]; got != loadBalancingFirstOrRandom {
			t.Errorf("%s = %v; want %q", settingLoadBalancing, got, loadBalancingFirstOrRandom)
		}
		if got := s[settingLoadBalancingFirstOffset]; got != 0 {
			t.Errorf("%s = %v; want 0", settingLoadBalancingFirstOffset, got)
		}
		if got := s[settingSkipUnavailableShards]; got != 0 {
			t.Errorf("%s = %v; want 0 (must not be clobbered)", settingSkipUnavailableShards, got)
		}
		if got := s[settingFallbackToStaleReplicasForDistributedQueries]; got != 0 {
			t.Errorf("%s = %v; want 0 (must not be clobbered)", settingFallbackToStaleReplicasForDistributedQueries, got)
		}
	}
}
