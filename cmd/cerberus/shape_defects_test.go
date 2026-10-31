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
	"github.com/tsouza/cerberus/internal/schema"
)

// Builds on either side of the shape-defect ranges: below the condition
// cache, a 26.3 LTS build carrying ClickHouse#119168 and #120286, a 25.11
// build carrying all three, and a build outside every range.
var (
	beforeShapeDefectsBuild = chopt.Version{Major: 25, Minor: 2, Patch: 2, Build: 1}
	lts263Build             = chopt.Version{Major: 26, Minor: 3, Patch: 17, Build: 56}
	line2511Build           = chopt.Version{Major: 25, Minor: 11, Patch: 9, Build: 34}
	shapeCleanBuild         = chopt.Version{Major: 26, Minor: 10, Patch: 2, Build: 103}
)

var (
	lts263Defects   = []string{chopt.ShapeDefectConversionConditionCache, chopt.ShapeDefectNegatedConjunctionProjection}
	line2511Defects = []string{
		chopt.ShapeDefectConversionConditionCache,
		chopt.ShapeDefectNegatedConjunctionProjection,
		chopt.ShapeDefectTraceIDProjectionRead,
	}
)

// Builds below each gate's setting floor: the supported floor, which
// predates use_query_condition_cache, and a 25.3 build, which accepts it but
// predates optimize_use_projection_filtering.
var (
	supportedFloorBuild = chopt.Version{Major: 24, Minor: 8, Patch: 14, Build: 39}
	preProjectionBuild  = chopt.Version{Major: 25, Minor: 3, Patch: 14, Build: 14}
)

// gates maps each id to g, the shape shapeDefectsOnFleet returns.
func gates(g chopt.DefectGate, ids ...string) map[string]chopt.DefectGate {
	out := make(map[string]chopt.DefectGate, len(ids))
	for _, id := range ids {
		out[id] = g
	}
	return out
}

// TestShapeDefectsOnFleet_ConservativeAcrossTheFleet pins the decision
// table: a defect any reached node carries is in force — gated while every
// reached node accepts its gate's setting, refused while one is too old to;
// only a complete pass with no node carrying it clears it; an incomplete pass
// keeps the decision in force for the defects no reached node carries.
func TestShapeDefectsOnFleet_ConservativeAcrossTheFleet(t *testing.T) {
	t.Parallel()

	none := chopt.EnabledSet{}
	in263 := none.WithShapeDefects(gates(chopt.DefectGated, lts263Defects...))
	refused263 := none.WithShapeDefects(gates(chopt.DefectRefused, lts263Defects...))
	cases := []struct {
		name    string
		fv      chclient.FleetVersions
		current chopt.EnabledSet
		want    map[string]chopt.DefectGate
	}{
		{"single affected node", chclient.FleetVersions{Versions: []chopt.Version{lts263Build}, Complete: true}, none, gates(chopt.DefectGated, lts263Defects...)},
		{"union across a mixed fleet", chclient.FleetVersions{Versions: []chopt.Version{lts263Build, line2511Build}, Complete: true}, none, gates(chopt.DefectGated, line2511Defects...)},
		{"one affected node beside a clean one", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, lts263Build}, Complete: true}, none, gates(chopt.DefectGated, lts263Defects...)},
		{"affected node seen, others unreachable", chclient.FleetVersions{Versions: []chopt.Version{lts263Build}}, none, gates(chopt.DefectGated, lts263Defects...)},
		{"every node clean", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, beforeShapeDefectsBuild}, Complete: true}, in263, gates(chopt.DefectGated)},
		{"clean nodes seen, one unreachable, defects in force", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}}, in263, gates(chopt.DefectGated, lts263Defects...)},
		{"clean nodes seen, one unreachable, none in force", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}}, none, gates(chopt.DefectGated)},
		{"nothing reachable, defects in force", chclient.FleetVersions{}, in263, gates(chopt.DefectGated, lts263Defects...)},
		{"affected node beside the supported floor", chclient.FleetVersions{Versions: []chopt.Version{supportedFloorBuild, lts263Build}, Complete: true}, none, gates(chopt.DefectRefused, lts263Defects...)},
		{
			"25.11 node beside a node predating projection filtering",
			chclient.FleetVersions{Versions: []chopt.Version{preProjectionBuild, line2511Build}, Complete: true},
			none,
			map[string]chopt.DefectGate{
				chopt.ShapeDefectConversionConditionCache:     chopt.DefectGated,
				chopt.ShapeDefectNegatedConjunctionProjection: chopt.DefectRefused,
				chopt.ShapeDefectTraceIDProjectionRead:        chopt.DefectRefused,
			},
		},
		{"refused, old node upgraded, affected node unreachable", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}}, refused263, gates(chopt.DefectRefused, lts263Defects...)},
		{"refused, old node upgraded", chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild, lts263Build}, Complete: true}, refused263, gates(chopt.DefectGated, lts263Defects...)},
		{"refused, affected node upgraded", chclient.FleetVersions{Versions: []chopt.Version{supportedFloorBuild, shapeCleanBuild}, Complete: true}, refused263, gates(chopt.DefectGated)},
	}
	for _, tc := range cases {
		got := shapeDefectsOnFleet(tc.fv, tc.current)
		if !maps.Equal(got, tc.want) {
			t.Errorf("%s: shapeDefectsOnFleet = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// setGates returns every shape defect set has in force, with its gate.
func setGates(set chopt.EnabledSet) map[string]chopt.DefectGate {
	out := map[string]chopt.DefectGate{}
	for _, id := range set.ShapeDefectIDs() {
		out[id] = set.ShapeDefectGate(id)
	}
	return out
}

// TestResolveCHOptimizations_MixedFleetCarriesShapeDefects is the boot half
// of a rolling upgrade into an affected build: the ids resolve against the
// older node, which carries no shape defect, but the set must already carry
// the upgraded node's, so no query of the defective shape reaches it with the
// mechanism on before the first re-probe. The older node predates every
// gate's setting, so each defect is refused rather than gated; a fleet of
// 25.6 or later nodes gates them.
func TestResolveCHOptimizations_MixedFleetCarriesShapeDefects(t *testing.T) {
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
		want  map[string]chopt.DefectGate
	}{
		{"mixed fleet", chclient.FleetVersions{Versions: []chopt.Version{beforeShapeDefectsBuild, line2511Build}, Complete: true}, gates(chopt.DefectRefused, line2511Defects...)},
		{"older fleet", chclient.FleetVersions{Versions: []chopt.Version{beforeShapeDefectsBuild, beforeShapeDefectsBuild}, Complete: true}, gates(chopt.DefectRefused)},
	} {
		probe := func(context.Context) chclient.FleetVersions { return tc.fleet }
		res, err := resolveCHOptimizations(context.Background(), quietLogger(), client, &cfg, probe)
		if err != nil {
			t.Fatalf("%s: resolveCHOptimizations: %v", tc.name, err)
		}
		if res.ResolvedVersion != beforeShapeDefectsBuild {
			t.Fatalf("%s: resolved against %s; want the oldest node, %s", tc.name, res.ResolvedVersion, beforeShapeDefectsBuild)
		}
		if got := setGates(res.Set); !maps.Equal(got, tc.want) {
			t.Errorf("%s: shape defects %v at boot; want %v", tc.name, got, tc.want)
		}
	}
}

// TestReprobe_ShapeDefectsFollowTheFleet pins both directions on the
// re-probe: a node moving onto an affected build brings its defects into
// force although the ids, resolved against the older node, do not move; and
// a complete pass with every node off the affected builds clears them.
func TestReprobe_ShapeDefectsFollowTheFleet(t *testing.T) {
	t.Parallel()

	cfg := config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}
	boot := resolutionAt(t, beforeShapeDefectsBuild)
	if len(boot.Set.ShapeDefectIDs()) != 0 {
		t.Fatal("premise: a set resolved below the condition cache must carry no shape defect")
	}
	upgrading := chclient.FleetVersions{Versions: []chopt.Version{beforeShapeDefectsBuild, lts263Build}, Complete: true}
	refused := gates(chopt.DefectRefused, lts263Defects...)
	got := reprobeUntilShapeDefects(t, cfg, boot, upgrading, refused)
	if got.ResolvedVersion != beforeShapeDefectsBuild || !got.Set.Equal(boot.Set.WithShapeDefects(refused)) {
		t.Fatalf("the re-probe moved more than the shape defects: got %+v, boot %+v", got, boot)
	}

	affected := resolutionAt(t, lts263Build)
	if !slices.Equal(affected.Set.ShapeDefectIDs(), lts263Defects) {
		t.Fatalf("premise: a set resolved on %s must carry %v; got %v", lts263Build, lts263Defects, affected.Set.ShapeDefectIDs())
	}
	upgraded := chclient.FleetVersions{Versions: []chopt.Version{shapeCleanBuild}, Complete: true}
	reprobeUntilShapeDefects(t, cfg, affected, upgraded, gates(chopt.DefectGated))
}

// TestReprobe_ShapeDefectsWhenResolveFails pins that the shape defects follow
// the fleet even on a pass whose resolution is refused: an explicit
// join_spill under enforcing, then a node below join_spill's floor and a node
// on an affected build both answer. The re-probe keeps the set in force, but
// not its shape-defect decision.
func TestReprobe_ShapeDefectsWhenResolveFails(t *testing.T) {
	t.Parallel()

	belowJoinSpill := chopt.Version{Major: 26, Minor: 2}
	cfg := config.Config{
		CHOptimizations:     chopt.FeatureJoinSpill,
		CHOptimizationsMode: chopt.Enforcing,
		Schema:              schema.DefaultOTelMetrics(),
	}
	if _, _, err := chopt.Resolve(chopt.Config{Optimizations: cfg.CHOptimizations, Mode: cfg.CHOptimizationsMode}, belowJoinSpill); err == nil {
		t.Fatal("premise: an explicit join_spill under enforcing must be refused below its floor")
	}
	boot := resolutionAt(t, shapeCleanBuild, chopt.FeatureJoinSpill)
	fleet := chclient.FleetVersions{Versions: []chopt.Version{belowJoinSpill, line2511Build}, Complete: true}
	got := reprobeUntilShapeDefects(t, cfg, boot, fleet, gates(chopt.DefectGated, line2511Defects...))
	if got.ResolvedVersion != shapeCleanBuild || !got.Set.Has(chopt.FeatureJoinSpill) {
		t.Fatalf("premise: the refused re-resolve should have kept the set in force; got %+v", got)
	}
}

// reprobeUntilShapeDefects runs the re-probe from boot against a fixed fleet
// until the live resolution carries exactly want, and returns it.
func reprobeUntilShapeDefects(t *testing.T, cfg config.Config, boot chOptResolution, fleet chclient.FleetVersions, want map[string]chopt.DefectGate) chOptResolution {
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
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{}, time.Millisecond, "", probe)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !maps.Equal(setGates(live.get().Set), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the shape defects never became %v; last %v", want, setGates(live.get().Set))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return live.get()
}
