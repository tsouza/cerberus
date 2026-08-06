package main

import (
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
)

// The /readyz per-head report must name exactly the heads THIS process serves.
// A pod that reports a head it never built would be evicted for a breaker no
// request can reach, and a pod that omits a head it does serve hides the very
// fault the report exists to surface.

// headBreakerKeys reports the sorted head names the reporter emits.
func headBreakerKeys(fn func() map[string]string) []string {
	if fn == nil {
		return nil
	}
	var keys []string
	for k := range fn() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lazyClient builds a client that never dials — chclient.New constructs the
// breaker registry without touching the network, and the reporter only reads
// stored breaker phases.
func lazyClient(t *testing.T) *chclient.Client {
	t.Helper()
	client, err := chclient.New(chclient.Config{Addr: unreachableAddr(t)})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestEnabledHeadBreakers_ReportsExactlyTheEnabledHeads is the split-mode gate:
// under CERBERUS_ENABLED_HEADS the reporter must shrink with the deployment. A
// reporter that always names all three would keep a single-head pod ready while
// its own head is dead (two phantom heads read as "not exhausted"), which is
// precisely the eviction the per-head probe exists to perform.
func TestEnabledHeadBreakers_ReportsExactlyTheEnabledHeads(t *testing.T) {
	for _, tc := range []struct {
		enabled string
		want    []string
	}{
		{"prom", []string{"prom"}},
		{"loki", []string{"loki"}},
		{"tempo", []string{"tempo"}},
		{"prom,tempo", []string{"prom", "tempo"}},
		{"", []string{"loki", "prom", "tempo"}},
	} {
		t.Run("enabled="+tc.enabled, func(t *testing.T) {
			t.Setenv("CERBERUS_ENABLED_HEADS", tc.enabled)
			cfg, err := config.FromEnv()
			if err != nil {
				t.Fatalf("config.FromEnv: %v", err)
			}

			got := headBreakerKeys(enabledHeadBreakers(lazyClient(t), cfg))
			if len(got) != len(tc.want) {
				t.Fatalf("heads = %v; want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("heads = %v; want %v", got, tc.want)
				}
			}
		})
	}
}

// TestEnabledHeadBreakers_ReportsLivePhases — the reporter reads breaker state
// per probe, and it must report the head's STORED phase (the same one the
// cerberus_ch_breaker_state gauge exports) rather than a backoff-evaluating
// peek that would silently consume the HALF-OPEN recovery slot.
func TestEnabledHeadBreakers_ReportsLivePhases(t *testing.T) {
	t.Setenv("CERBERUS_ENABLED_HEADS", "prom,loki,tempo")
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}

	fn := enabledHeadBreakers(lazyClient(t), cfg)
	for head, phase := range fn() {
		if phase != "closed" {
			t.Errorf("fresh %s breaker = %q; want closed", head, phase)
		}
	}
}

// TestEnabledHeadBreakers_NoHeadsMeansNoReporter — a process serving no query
// head at all (the migrate CLI shape) wires no reporter, so /readyz omits the
// heads object entirely instead of reporting an empty map the probe would have
// to decide how to read.
func TestEnabledHeadBreakers_NoHeadsMeansNoReporter(t *testing.T) {
	if got := enabledHeadBreakers(lazyClient(t), config.Config{}); got != nil {
		t.Errorf("enabledHeadBreakers with no enabled heads = non-nil; want nil")
	}
}
