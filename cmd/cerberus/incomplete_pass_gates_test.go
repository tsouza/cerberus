package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// betweenGateSettingsBuild accepts use_query_condition_cache (25.3) but not
// optimize_use_projection_filtering (25.6). Like every build in that band, it
// carries ClickHouse#119168 and the condition cache's own defects itself.
var betweenGateSettingsBuild = chopt.Version{Major: 25, Minor: 4, Patch: 1, Build: 2}

// everyShapeDefect is every shape-defect id, sorted as ShapeDefectIDs sorts.
func everyShapeDefect() []string {
	var ids []string
	for _, d := range chopt.ShapeDefects() {
		ids = append(ids, d.ID)
	}
	slices.Sort(ids)
	return ids
}

// bootOn runs the boot resolution against fleet with a fresh client, and
// returns the resolution and that client.
func bootOn(t *testing.T, fleet chclient.FleetVersions) (chOptResolution, *chclient.Client) {
	t.Helper()
	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	client, err := chclient.New(chclient.Config{Addr: cfg.ClickHouse.Addr, Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	res, err := resolveCHOptimizations(context.Background(), quietLogger(), client, &cfg, func(context.Context) chclient.FleetVersions { return fleet })
	if err != nil {
		t.Fatalf("resolveCHOptimizations: %v", err)
	}
	return res, client
}

// TestResolveCHOptimizations_IncompleteBootStartsGatesOn pins the boot of a
// fleet whose older node the probe could not reach: that node may run a
// build inside any wrong-result range, so every defect gate the reached nodes
// accept starts on, the re-probe runs at the fast cadence, and the first
// complete pass clears every gate the complete fleet does not need.
func TestResolveCHOptimizations_IncompleteBootStartsGatesOn(t *testing.T) {
	t.Parallel()

	olderUnreachable := chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}}
	res, client := bootOn(t, olderUnreachable)
	if res.VersionFallback || res.ResolvedVersion != shapeCleanBuild {
		t.Fatalf("premise: the boot resolved against %s (fallback %v); want the reached node, %s", res.ResolvedVersion, res.VersionFallback, shapeCleanBuild)
	}
	if got, want := res.Set.ShapeDefectIDs(), everyShapeDefect(); !slices.Equal(got, want) {
		t.Errorf("ShapeDefectIDs() = %v at boot; want every shape defect, %v", got, want)
	}
	if !client.QueryConditionCacheDisabled() {
		t.Error("condition-cache override off at boot; want it on while a node is unaccounted for")
	}
	if got := nextReprobeDelay(res, chOptReprobeInterval); got != chOptFloorRetryInterval {
		t.Errorf("next re-probe in %v after an incomplete boot; want the fast cadence, %v", got, chOptFloorRetryInterval)
	}

	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	live := newCHOptLive(res)
	complete := chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, beforeShapeDefectsBuild}, Complete: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{client: client}, testReprobeTiming, nil, "",
			func(context.Context) chclient.FleetVersions { return complete })
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(live.get().Set.ShapeDefectIDs()) != 0 || client.QueryConditionCacheDisabled() || live.get().ResolvedVersion != beforeShapeDefectsBuild {
		if time.Now().After(deadline) {
			t.Fatalf("the complete pass never cleared the gates: shape defects %v, override %v, resolved %s",
				live.get().Set.ShapeDefectIDs(), client.QueryConditionCacheDisabled(), live.get().ResolvedVersion)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := live.get(); got.FleetIncomplete || nextReprobeDelay(got, chOptReprobeInterval) != chOptReprobeInterval {
		t.Errorf("after a complete pass: FleetIncomplete %v; want false, and the steady cadence", got.FleetIncomplete)
	}
}

// TestResolveCHOptimizations_IncompleteBootGatesFollowTheLowestReachedBuild
// pins which gates an incomplete boot starts on: only those whose setting the
// lowest reached build accepts, since every reached node on that build would
// reject a query carrying any other. A complete boot, and a boot that reached
// no node, start none beyond what the reached builds carry.
func TestResolveCHOptimizations_IncompleteBootGatesFollowTheLowestReachedBuild(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		fleet            chclient.FleetVersions
		wantShapeDefects []string
		wantConditionOff bool
		wantFastReprobe  bool
	}{
		{
			"incomplete, lowest reached accepts every gate and carries no defect",
			chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}},
			everyShapeDefect(), true, true,
		},
		{
			"incomplete, lowest reached accepts use_query_condition_cache only, so no projection gate",
			chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, betweenGateSettingsBuild}},
			[]string{chopt.ShapeDefectConversionConditionCache},
			true, true,
		},
		{
			"incomplete, lowest reached below 25.3",
			chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, beforeShapeDefectsBuild}},
			nil, false, true,
		},
		{
			"complete, no defect",
			chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}, Complete: true},
			nil, false, false,
		},
		{
			"nothing reached",
			chclient.FleetVersions{},
			nil, false, true,
		},
	} {
		res, client := bootOn(t, tc.fleet)
		if got := res.Set.ShapeDefectIDs(); !slices.Equal(got, tc.wantShapeDefects) {
			t.Errorf("%s: ShapeDefectIDs() = %v; want %v", tc.name, got, tc.wantShapeDefects)
		}
		if got := client.QueryConditionCacheDisabled(); got != tc.wantConditionOff {
			t.Errorf("%s: condition-cache override = %v; want %v", tc.name, got, tc.wantConditionOff)
		}
		if got := nextReprobeDelay(res, chOptReprobeInterval) == chOptFloorRetryInterval; got != tc.wantFastReprobe {
			t.Errorf("%s: fast re-probe = %v; want %v", tc.name, got, tc.wantFastReprobe)
		}
	}
}

// TestReprobe_IncompletePassAfterAFloorFallbackStartsGatesOn: a floor
// fallback records that no node answered, so — like boot — it says nothing
// about a node the first answering pass misses. That pass starts every gate
// the reached nodes accept, as boot would have.
func TestReprobe_IncompletePassAfterAFloorFallbackStartsGatesOn(t *testing.T) {
	t.Parallel()

	floor, client := bootOn(t, chclient.FleetVersions{})
	if !floor.VersionFallback || len(floor.Set.ShapeDefectIDs()) != 0 || client.QueryConditionCacheDisabled() {
		t.Fatalf("premise: a boot that reached no node falls back to the floor with no gate on; got %+v, override %v", floor, client.QueryConditionCacheDisabled())
	}
	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
	live := newCHOptLive(floor)
	partial := chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{client: client}, testReprobeTiming, nil, "",
			func(context.Context) chclient.FleetVersions { return partial })
	}()
	deadline := time.Now().Add(20 * time.Second)
	for live.get().VersionFallback {
		if time.Now().After(deadline) {
			t.Fatal("the re-probe never lifted the floor fallback")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got, want := live.get().Set.ShapeDefectIDs(), everyShapeDefect(); !slices.Equal(got, want) {
		t.Errorf("ShapeDefectIDs() = %v after the first incomplete pass; want %v", got, want)
	}
	if !client.QueryConditionCacheDisabled() {
		t.Error("condition-cache override off after the first incomplete pass; want it on")
	}
	if !live.get().FleetIncomplete {
		t.Error("FleetIncomplete = false after an incomplete pass")
	}
}
