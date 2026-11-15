package main

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// Two replicas behind one load-balanced address. The fleet ran
// rollbackFromBuild, and replica B rolls back to balancedOldBuild, a build
// that knows neither argAndMax (arg_and_max_fusion, 25.11) nor
// timeSeriesThrowDuplicateSeriesIf (ts_throw_duplicate_series_if, 26.2).
var balancedOldBuild = chopt.Version{Major: 25, Minor: 10, Patch: 7, Build: 6}

const (
	balancedReplicaA = "5d0c7f3a-0000-4000-8000-00000000000a"
	balancedReplicaB = "5d0c7f3a-0000-4000-8000-00000000000b"
)

// balancedRollbackPasses is how many passes the probe alternates between the
// two replicas while B is rolled back: enough alternations that a resolution
// following the last pass would have risen many times over.
const balancedRollbackPasses = 40

// versionGatedFunctionFeatures are features whose SQL calls a function the
// rolled-back build does not know.
var versionGatedFunctionFeatures = []string{chopt.FeatureArgAndMaxFusion, chopt.FeatureTSThrowDuplicateSeriesIf}

// TestReprobeCHOptimizations_LoadBalancedRollbackConvergesOnTheOldestReplica
// is a rolling rollback behind one load-balanced address, with no cluster
// named. Every pass reaches the one replica its connection landed on and
// reports itself complete, and the passes alternate between replica A on the
// newer build and replica B rolled back to the older one.
//
// Once a pass has reached B, the resolution in force must be B's build on
// every later pass for as long as B serves that build — never the build of
// whichever replica the last pass landed on — so the functions B does not
// know stay out of the set. Every such pass rests on the build of the replica
// it did not land on, which runs the next pass at the fast cadence. When B
// rolls forward, the first pass that reaches it raises the resolution to the
// newer build, and the fleet no longer rests on an unreached build.
func TestReprobeCHOptimizations_LoadBalancedRollbackConvergesOnTheOldestReplica(t *testing.T) {
	t.Parallel()

	cfg := config.Config{CHOptimizations: chopt.SelectionAuto, CHOptimizationsMode: chopt.Permissive}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	resolve := func(v chopt.Version) chopt.EnabledSet {
		t.Helper()
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: cfg.CHOptimizations, Mode: cfg.CHOptimizationsMode}, v)
		if err != nil {
			t.Fatalf("resolve auto against %s: %v", v, err)
		}
		return set
	}
	for _, id := range versionGatedFunctionFeatures {
		if !resolve(rollbackFromBuild).Has(id) || resolve(balancedOldBuild).Has(id) {
			t.Fatalf("premise: auto enables %s on %s and not on %s", id, rollbackFromBuild, balancedOldBuild)
		}
	}
	live := newCHOptLive(chOptResolution{Set: resolve(rollbackFromBuild), ResolvedVersion: rollbackFromBuild})

	// Pass n (from 1) lands on A when n is odd and on B when it is even. B
	// runs the older build through pass balancedRollbackPasses and the newer
	// one after. Each pass records the resolution in force when it starts,
	// which is the one the previous pass left.
	const rolledForwardCheck = balancedRollbackPasses + 3
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inForceAt := map[int]chOptResolution{}
	pass := 0
	probe := func(context.Context) chclient.FleetPass {
		pass++
		inForceAt[pass] = live.get()
		if pass == rolledForwardCheck {
			cancel()
		}
		node := chclient.FleetNode{ID: balancedReplicaA, Version: rollbackFromBuild}
		if pass%2 == 0 {
			node = chclient.FleetNode{ID: balancedReplicaB, Version: balancedOldBuild}
			if pass > balancedRollbackPasses {
				node.Version = rollbackFromBuild
			}
		}
		return chclient.FleetPass{Nodes: []chclient.FleetNode{node}, Complete: true}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{},
			reprobeTiming{steady: time.Millisecond, earlySpacing: time.Hour}, newReprobeTrigger(), "", rememberingFleetProber(probe))
	}()
	<-done
	if pass < rolledForwardCheck {
		t.Fatalf("the re-probe ran %d passes before the deadline; want %d", pass, rolledForwardCheck)
	}

	if got := inForceAt[2].ResolvedVersion; got != rollbackFromBuild {
		t.Errorf("after pass 1 reached only A: resolved against %s; want %s", got, rollbackFromBuild)
	}
	for n := 3; n <= balancedRollbackPasses+2; n++ {
		res := inForceAt[n]
		lastOnA := (n-1)%2 == 1
		if res.ResolvedVersion != balancedOldBuild || !res.FleetHeld || res.FleetIncomplete {
			t.Errorf("after pass %d (landed on A: %v): resolved against %s, held=%v, incomplete=%v; want %s, held, complete",
				n-1, lastOnA, res.ResolvedVersion, res.FleetHeld, res.FleetIncomplete, balancedOldBuild)
		}
		for _, id := range versionGatedFunctionFeatures {
			if res.Set.Has(id) {
				t.Errorf("after pass %d: %s is in force while replica B runs %s", n-1, id, balancedOldBuild)
			}
		}
		if nextReprobeDelay(res, chOptReprobeInterval) != chOptFloorRetryInterval {
			t.Errorf("after pass %d: next re-probe in %s; want the fast %s while the fleet rests on an unreached build",
				n-1, nextReprobeDelay(res, chOptReprobeInterval), chOptFloorRetryInterval)
		}
	}
	if res := inForceAt[rolledForwardCheck]; res.ResolvedVersion != rollbackFromBuild || res.FleetHeld {
		t.Errorf("after B rolled forward and pass %d reached it: resolved against %s, held=%v; want %s, not held",
			rolledForwardCheck-1, res.ResolvedVersion, res.FleetHeld, rollbackFromBuild)
	}
}
