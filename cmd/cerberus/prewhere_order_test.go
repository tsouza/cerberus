package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// Builds on either side of chopt.PrewhereReorderedSince.
var (
	whereOrderBuild = chopt.Version{Major: 24, Minor: 8, Patch: 14, Build: 39}
	reorderingBuild = chopt.Version{Major: 26, Minor: 8, Patch: 36, Build: 3}
)

// TestPrewhereInWhereOrderOnFleet pins the decision table: a reached node
// that reorders PREWHERE conditions clears it; a complete pass with every
// node on a build that keeps WHERE order sets it; a pass that missed a node,
// or read no version, and saw no reordering build leaves the decision in
// force.
func TestPrewhereInWhereOrderOnFleet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fv      chclient.FleetVersions
		current bool
		want    bool
	}{
		{"every node keeps WHERE order", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, whereOrderBuild}, Complete: true}, false, true},
		{"one node reorders", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, reorderingBuild}, Complete: true}, true, false},
		{"reordering node seen, others unreachable", chclient.FleetVersions{Versions: []chopt.Version{reorderingBuild}}, true, false},
		{"WHERE-order nodes seen, one unreachable, set in force", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild}}, true, true},
		{"WHERE-order nodes seen, one unreachable, unset in force", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild}}, false, false},
		{"nothing reachable, unset in force", chclient.FleetVersions{}, false, false},
		{"nothing reachable, set in force", chclient.FleetVersions{}, true, true},
		{"complete pass with no version", chclient.FleetVersions{Complete: true}, false, false},
	} {
		if got := prewhereInWhereOrderOnFleet(tc.fv, tc.current); got != tc.want {
			t.Errorf("%s: prewhereInWhereOrderOnFleet = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolveCHOptimizations_PrewhereInWhereOrderAtBoot pins the boot half:
// the resolved set carries the fact only for a complete pass over builds that
// keep WHERE order, and a pass that reaches no node — the floor fallback,
// whose 24.8 floor would itself keep WHERE order — or misses one leaves it
// unset.
func TestResolveCHOptimizations_PrewhereInWhereOrderAtBoot(t *testing.T) {
	t.Parallel()

	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	client, err := chclient.New(chclient.Config{Addr: cfg.ClickHouse.Addr, Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for _, tc := range []struct {
		name  string
		fleet chclient.FleetVersions
		want  bool
	}{
		{"WHERE-order fleet", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, whereOrderBuild}, Complete: true}, true},
		{"mixed fleet", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, reorderingBuild}, Complete: true}, false},
		{"a node unreachable", chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild}}, false},
		{"no node reachable", chclient.FleetVersions{}, false},
	} {
		probe := func(context.Context) chclient.FleetVersions { return tc.fleet }
		res, err := resolveCHOptimizations(context.Background(), quietLogger(), client, &cfg, probe)
		if err != nil {
			t.Fatalf("%s: resolveCHOptimizations: %v", tc.name, err)
		}
		if got := res.Set.PrewhereInWhereOrder(); got != tc.want {
			t.Errorf("%s: PrewhereInWhereOrder() = %v at boot; want %v", tc.name, got, tc.want)
		}
	}
}

// TestReprobe_PrewhereInWhereOrderFollowsTheFleet pins the re-probe half: a
// complete pass over WHERE-order builds sets the fact on a resolution that
// did not carry it, and a pass that reaches a reordering node clears it,
// with the feature ids unchanged.
func TestReprobe_PrewhereInWhereOrderFollowsTheFleet(t *testing.T) {
	t.Parallel()
	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}

	boot := resolutionAt(t, whereOrderBuild)
	if boot.Set.PrewhereInWhereOrder() {
		t.Fatal("premise: a set resolved without a fleet pass must not carry the fact")
	}
	whereOrder := chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, whereOrderBuild}, Complete: true}
	set := reprobeUntilPrewhereInWhereOrder(t, cfg, boot, whereOrder, true)
	if !set.Set.Equal(boot.Set.WithPrewhereInWhereOrder(true)) {
		t.Fatalf("the re-probe moved more than the PREWHERE order fact: got %+v, boot %+v", set, boot)
	}

	upgraded := chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild, reorderingBuild}, Complete: true}
	cleared := reprobeUntilPrewhereInWhereOrder(t, cfg, set, upgraded, false)
	if cleared.Set.PrewhereInWhereOrder() {
		t.Fatal("a pass that reached a reordering node kept the fact")
	}
}

// TestReprobe_PrewhereInWhereOrderHeldOnAnIncompletePass pins that a
// re-probe pass that misses a node and reads only WHERE-order builds leaves
// the decision in force as it is, set or unset.
func TestReprobe_PrewhereInWhereOrderHeldOnAnIncompletePass(t *testing.T) {
	t.Parallel()
	const passes = 3
	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	incomplete := chclient.FleetVersions{Versions: []chopt.Version{whereOrderBuild}}
	for _, inForce := range []bool{true, false} {
		boot := resolutionAt(t, whereOrderBuild)
		boot.Set = boot.Set.WithPrewhereInWhereOrder(inForce)
		live := newCHOptLive(boot)

		// Once the probe answers a pass past passes, the passes before it
		// have published.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		published := make(chan chOptResolution, 1)
		var probes atomic.Int32
		probe := func(context.Context) chclient.FleetVersions {
			if probes.Add(1) == passes+1 {
				published <- live.get()
			}
			return incomplete
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{}, testReprobeTiming, nil, "", probe)
		}()
		var got chOptResolution
		select {
		case got = <-published:
		case <-ctx.Done():
			t.Fatalf("in force %v: the re-probe did not run %d passes", inForce, passes)
		}
		cancel()
		<-done
		if got.Set.PrewhereInWhereOrder() != inForce {
			t.Errorf("in force %v: after %d incomplete passes over WHERE-order builds, PrewhereInWhereOrder() = %v", inForce, passes, got.Set.PrewhereInWhereOrder())
		}
	}
}

// reprobeUntilPrewhereInWhereOrder runs the re-probe from boot against a
// fixed fleet until the live resolution's PrewhereInWhereOrder is want, and
// returns that resolution.
func reprobeUntilPrewhereInWhereOrder(t *testing.T, cfg config.Config, boot chOptResolution, fleet chclient.FleetVersions, want bool) chOptResolution {
	t.Helper()
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	live := newCHOptLive(boot)
	probe := func(context.Context) chclient.FleetVersions { return fleet }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{}, testReprobeTiming, nil, "", probe)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for live.get().Set.PrewhereInWhereOrder() != want {
		if time.Now().After(deadline) {
			t.Fatalf("the re-probe never reported PrewhereInWhereOrder=%v for fleet %+v", want, fleet)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return live.get()
}
