package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// balancedWorkload is the operator-configured CERBERUS_CH_QUERY_WORKLOAD.
const balancedWorkload = "cerberus_queries"

// balancedPhasePasses is how many passes each phase of the canary test
// alternates between the two replicas: enough alternations that a verdict
// following the last pass would have flipped back several times over. Even,
// so every phase starts on replica A.
const balancedPhasePasses = 8

// balancedCanaryConfig selects auto plus the opt-in query_log_union and
// configures the workload, so the probe runs all four canaries.
var balancedCanaryConfig = config.Config{
	CHOptimizations:     chopt.SelectionAuto + "," + chopt.FeatureQueryLogUnion,
	CHOptimizationsMode: chopt.Permissive,
	CHQueryWorkload:     balancedWorkload,
}

// replicaVerdicts is every canary permitting except refused, which forbids
// ("" refuses none).
func replicaVerdicts(refused string) map[string]chopt.Capability {
	verdicts := map[string]chopt.Capability{}
	for _, c := range fleetCanaries(balancedCanaryConfig.CHOptimizations, balancedCanaryConfig.CHQueryWorkload) {
		verdicts[c.Name] = chopt.CapabilityAvailable
	}
	if refused != "" {
		verdicts[refused] = chopt.CapabilityForbidden
	}
	return verdicts
}

// TestReprobeCHOptimizations_LoadBalancedCanaryHoldsTheRefusingReplica is two
// replicas on one build behind one load-balanced address, with no cluster
// named. Every pass reaches the one replica its connection landed on and
// reports itself complete, and the passes alternate between them. Replica A
// permits every canary. Replica B forbids one canary per phase — the ts_grid
// setting, the result cache settings, the query-log union table, then the
// workload (its CREATE WORKLOAD missing) — and none in the last phase.
//
// From the first pass of a phase that reaches B until the phase ends, the
// capability B refuses must stay off on every pass, including the passes that
// land on A, and every other capability must be on: each canary's verdict
// decides its own capability and no other. Every pass that lands on A rests
// on B's remembered refusal and runs the next pass at the fast cadence; a
// pass that lands on B reached the refusal and is not held.
func TestReprobeCHOptimizations_LoadBalancedCanaryHoldsTheRefusingReplica(t *testing.T) {
	t.Parallel()

	cfg := balancedCanaryConfig
	permitted, _, err := chopt.Resolve(fleetResolveConfig(cfg, chclient.FleetVersions{Verdicts: replicaVerdicts("")}), rollbackFromBuild)
	if err != nil {
		t.Fatalf("resolve against %s with every canary permitting: %v", rollbackFromBuild, err)
	}
	// on reports whether the capability canary gates is in force in res.
	on := map[string]func(res chOptResolution) bool{
		chclient.CanaryTSGrid:        func(res chOptResolution) bool { return res.Set.Has(chopt.FeatureTSGridRange) },
		chclient.CanaryResultCache:   func(res chOptResolution) bool { return res.Set.Has(chopt.FeatureResultCache) },
		chclient.CanaryQueryLogUnion: func(res chOptResolution) bool { return res.Set.Has(chopt.FeatureQueryLogUnion) },
		chclient.CanaryQueryWorkload: func(res chOptResolution) bool { return res.QueryWorkload == balancedWorkload },
	}
	initial := chOptResolution{Set: permitted, ResolvedVersion: rollbackFromBuild, QueryWorkload: balancedWorkload}
	for canary, isOn := range on {
		if !isOn(initial) {
			t.Fatalf("premise: with every canary permitting on %s, %s's capability is off: %v", rollbackFromBuild, canary, permitted.IDs())
		}
	}
	phases := []string{chclient.CanaryTSGrid, chclient.CanaryResultCache, chclient.CanaryQueryLogUnion, chclient.CanaryQueryWorkload, ""}
	live := newCHOptLive(initial)

	// Pass n (from 1) is in phase (n-1)/balancedPhasePasses and lands on B
	// when n is even. Each pass records the resolution in force when it
	// starts, which is the one the previous pass left.
	lastPass := len(phases) * balancedPhasePasses
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inForceAt := map[int]chOptResolution{}
	pass := 0
	probe := func(context.Context, chclient.CanaryOrder) chclient.FleetPass {
		pass++
		inForceAt[pass] = live.get()
		if pass == lastPass+1 {
			cancel()
		}
		node := chclient.FleetNode{ID: balancedReplicaA, Version: rollbackFromBuild, Verdicts: replicaVerdicts("")}
		if pass%2 == 0 {
			phase := min((pass-1)/balancedPhasePasses, len(phases)-1)
			node = chclient.FleetNode{ID: balancedReplicaB, Version: rollbackFromBuild, Verdicts: replicaVerdicts(phases[phase])}
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
	if pass <= lastPass {
		t.Fatalf("the re-probe ran %d passes before the deadline; want %d", pass, lastPass+1)
	}

	for m := 1; m <= lastPass; m++ {
		within := (m - 1) % balancedPhasePasses
		if within == 0 {
			// The phase's first pass lands on A and may still rest on the
			// previous phase's refusal.
			continue
		}
		refused := phases[(m-1)/balancedPhasePasses]
		res := inForceAt[m+1]
		landedOnB := m%2 == 0
		for canary, isOn := range on {
			if want := canary != refused; isOn(res) != want {
				t.Errorf("after pass %d (B refuses %q, landed on B: %v): %s capability on = %v; want %v",
					m, refused, landedOnB, canary, isOn(res), want)
			}
		}
		wantHeld := refused != "" && !landedOnB
		if res.FleetHeld != wantHeld || (nextReprobeDelay(res, chOptReprobeInterval) == chOptFloorRetryInterval) != wantHeld {
			t.Errorf("after pass %d (B refuses %q, landed on B: %v): held=%v, next re-probe in %s; want held=%v",
				m, refused, landedOnB, res.FleetHeld, nextReprobeDelay(res, chOptReprobeInterval), wantHeld)
		}
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
		{chopt.SelectionAuto, "", []string{chclient.CanaryResultCache, chclient.CanaryTSGrid}},
		{chopt.SelectionAuto + "," + chopt.FeatureQueryLogUnion, "", []string{chclient.CanaryQueryLogUnion, chclient.CanaryResultCache, chclient.CanaryTSGrid}},
		{chopt.SelectionAuto, balancedWorkload, []string{chclient.CanaryQueryWorkload, chclient.CanaryResultCache, chclient.CanaryTSGrid}},
	}
	for _, tc := range cases {
		names := make([]string, 0, len(tc.want))
		for _, c := range fleetCanaries(tc.selection, tc.workload) {
			names = append(names, c.Name)
		}
		if slices.Sort(names); !slices.Equal(names, tc.want) {
			t.Errorf("fleetCanaries(%q, %q) = %v; want %v", tc.selection, tc.workload, names, tc.want)
		}
	}
}
