package main

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// balancedWorkload is the operator-configured CERBERUS_CH_QUERY_WORKLOAD whose
// CREATE WORKLOAD exists on replica A only.
const balancedWorkload = "cerberus_queries"

// balancedRefusalPasses is how many passes the probe alternates between the
// two replicas while B refuses: enough alternations that a verdict following
// the last pass would have flipped back many times over.
const balancedRefusalPasses = 40

// canaryVerdicts is the verdict a replica gives on every canary
// fleetCanaries runs for an auto selection with a workload configured.
func canaryVerdicts(v chopt.Capability) map[string]chopt.Capability {
	return map[string]chopt.Capability{tsGridCanary: v, resultCacheCanary: chopt.CapabilityAvailable, queryWorkloadCanary: v}
}

// TestReprobeCHOptimizations_LoadBalancedCanaryHoldsTheRefusingReplica is two
// replicas on one build behind one load-balanced address, with no cluster
// named. Every pass reaches the one replica its connection landed on and
// reports itself complete. Replica A permits the experimental ts_grid setting
// and has the configured workload defined; replica B forbids the setting and
// lacks the workload, so its canaries answer forbidden, and the passes
// alternate between them.
//
// Once a pass has reached B, every later pass must leave the workload unset
// and the native ts_grid features out of the set for as long as B refuses —
// never the verdict of whichever replica the last pass landed on. Every such
// pass that lands on A rests on B's remembered verdict, which runs the next
// pass at the fast cadence. When B starts permitting both, the first pass that
// reaches it restores the workload and the features.
func TestReprobeCHOptimizations_LoadBalancedCanaryHoldsTheRefusingReplica(t *testing.T) {
	t.Parallel()

	cfg := config.Config{CHOptimizations: chopt.SelectionAuto, CHOptimizationsMode: chopt.Permissive, CHQueryWorkload: balancedWorkload}
	resolve := func(tsGrid chopt.Capability) chopt.EnabledSet {
		t.Helper()
		set, _, err := chopt.Resolve(chopt.Config{
			Optimizations:         cfg.CHOptimizations,
			Mode:                  cfg.CHOptimizationsMode,
			Capability:            tsGrid,
			ResultCacheCapability: chopt.CapabilityAvailable,
		}, rollbackFromBuild)
		if err != nil {
			t.Fatalf("resolve auto against %s with ts_grid %s: %v", rollbackFromBuild, tsGrid, err)
		}
		return set
	}
	permitted, refused := resolve(chopt.CapabilityAvailable), resolve(chopt.CapabilityForbidden)
	var gated []string
	for _, id := range permitted.IDs() {
		if !refused.Has(id) {
			gated = append(gated, id)
		}
	}
	if !slices.Contains(gated, chopt.FeatureTSGridRange) {
		t.Fatalf("premise: auto on %s enables %s only when the ts_grid canary permits it; gated = %v",
			rollbackFromBuild, chopt.FeatureTSGridRange, gated)
	}
	if names := canaryNames(fleetCanaries(cfg.CHOptimizations, cfg.CHQueryWorkload)); !slices.Equal(names, slices.Sorted(maps.Keys(canaryVerdicts(chopt.CapabilityAvailable)))) {
		t.Fatalf("premise: the probe runs canaries %v; the replicas answer %v", names, canaryVerdicts(chopt.CapabilityAvailable))
	}
	live := newCHOptLive(chOptResolution{Set: permitted, ResolvedVersion: rollbackFromBuild, QueryWorkload: balancedWorkload})

	// Pass n (from 1) lands on A when n is odd and on B when it is even. B
	// refuses through pass balancedRefusalPasses and permits after. Each pass
	// records the resolution in force when it starts, which is the one the
	// previous pass left.
	const permittedCheck = balancedRefusalPasses + 3
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inForceAt := map[int]chOptResolution{}
	pass := 0
	probe := func(context.Context) chclient.FleetPass {
		pass++
		inForceAt[pass] = live.get()
		if pass == permittedCheck {
			cancel()
		}
		node := chclient.FleetNode{ID: balancedReplicaA, Version: rollbackFromBuild, Verdicts: canaryVerdicts(chopt.CapabilityAvailable)}
		if pass%2 == 0 {
			node = chclient.FleetNode{ID: balancedReplicaB, Version: rollbackFromBuild, Verdicts: canaryVerdicts(chopt.CapabilityForbidden)}
			if pass > balancedRefusalPasses {
				node.Verdicts = canaryVerdicts(chopt.CapabilityAvailable)
			}
		}
		return chclient.FleetPass{Nodes: []chclient.FleetNode{node}, Complete: true}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{},
			reprobeTiming{steady: time.Millisecond, earlySpacing: time.Hour}, newReprobeTrigger(), balancedWorkload, rememberingFleetProber(probe))
	}()
	<-done
	if pass < permittedCheck {
		t.Fatalf("the re-probe ran %d passes before the deadline; want %d", pass, permittedCheck)
	}

	if got := inForceAt[2]; got.QueryWorkload != balancedWorkload || !got.Set.Has(chopt.FeatureTSGridRange) {
		t.Errorf("after pass 1 reached only A: workload %q, %s in set = %v; want %q and in the set",
			got.QueryWorkload, chopt.FeatureTSGridRange, got.Set.Has(chopt.FeatureTSGridRange), balancedWorkload)
	}
	for n := 3; n <= balancedRefusalPasses+2; n++ {
		res := inForceAt[n]
		lastOnA := (n-1)%2 == 1
		if res.QueryWorkload != "" {
			t.Errorf("after pass %d (landed on A: %v): workload %q in force while replica B lacks it; want none",
				n-1, lastOnA, res.QueryWorkload)
		}
		for _, id := range gated {
			if res.Set.Has(id) {
				t.Errorf("after pass %d (landed on A: %v): %s is in force while replica B forbids the ts_grid setting",
					n-1, lastOnA, id)
			}
		}
		if lastOnA && (!res.FleetHeld || nextReprobeDelay(res, chOptReprobeInterval) != chOptFloorRetryInterval) {
			t.Errorf("after pass %d landed on A: held=%v, next re-probe in %s; want held and the fast %s while B's refusal is remembered",
				n-1, res.FleetHeld, nextReprobeDelay(res, chOptReprobeInterval), chOptFloorRetryInterval)
		}
	}
	if res := inForceAt[permittedCheck]; res.QueryWorkload != balancedWorkload || !res.Set.Has(chopt.FeatureTSGridRange) || res.FleetHeld {
		t.Errorf("after B permitted and pass %d reached it: workload %q, %s in set = %v, held=%v; want %q, in the set, not held",
			permittedCheck-1, res.QueryWorkload, chopt.FeatureTSGridRange, res.Set.Has(chopt.FeatureTSGridRange), res.FleetHeld, balancedWorkload)
	}
}

// TestFleetCanaries_RunsWhatTheConfigurationAsksFor: the ts_grid and
// result_cache canaries always run; the query-log union canary only when the
// selection names query_log_union, and the workload canary only when a
// workload is configured, so an unrequested verdict costs no round trip.
func TestFleetCanaries_RunsWhatTheConfigurationAsksFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		selection, workload string
		want                []string
	}{
		{chopt.SelectionAuto, "", []string{resultCacheCanary, tsGridCanary}},
		{chopt.SelectionAuto + "," + chopt.FeatureQueryLogUnion, "", []string{queryLogUnionCanary, resultCacheCanary, tsGridCanary}},
		{chopt.SelectionAuto, balancedWorkload, []string{queryWorkloadCanary, resultCacheCanary, tsGridCanary}},
	}
	for _, tc := range cases {
		if got := canaryNames(fleetCanaries(tc.selection, tc.workload)); !slices.Equal(got, tc.want) {
			t.Errorf("fleetCanaries(%q, %q) = %v; want %v", tc.selection, tc.workload, got, tc.want)
		}
	}
}

// canaryNames is the sorted names of canaries.
func canaryNames(canaries []chclient.FleetCanary) []string {
	names := make([]string, 0, len(canaries))
	for _, c := range canaries {
		names = append(names, c.Name)
	}
	slices.Sort(names)
	return names
}
