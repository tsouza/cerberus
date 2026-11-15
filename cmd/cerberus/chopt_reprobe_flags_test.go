package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// argAndMaxFloorBuild is a build on arg_and_max_fusion's 25.11 floor.
var argAndMaxFloorBuild = chopt.Version{Major: 25, Minor: 11, Patch: 9, Build: 34}

// TestReprobeCHOptimizations_UnchangedPassRecordsItsFlags: a pass whose set,
// version and workload equal the ones in force swaps no consumer, but its own
// flags replace the ones in force. Otherwise a floor fallback whose next probe
// answers on the floor's own build keeps /readyz failing, and a resolution
// from an incomplete or held pass keeps claiming so after a pass that reached
// every node and every remembered build. The same holds when the pass's resolution
// fails and the set in force is kept: under enforcing, an explicit feature the
// reached build is below the floor of.
func TestReprobeCHOptimizations_UnchangedPassRecordsItsFlags(t *testing.T) {
	t.Parallel()

	auto := config.Config{CHOptimizations: chopt.SelectionAuto, CHOptimizationsMode: chopt.Permissive}
	explicit := config.Config{CHOptimizations: chopt.FeatureArgAndMaxFusion, CHOptimizationsMode: chopt.Enforcing}
	cases := []struct {
		name string
		cfg  config.Config
		// inForce is the build the resolution in force was made against,
		// and pass the build the pass reaches.
		inForce, pass chopt.Version
		flag          func(*chOptResolution)
	}{
		{"floor fallback answered on the floor build", auto, supportedFloorVersion, supportedFloorVersion, func(r *chOptResolution) { r.VersionFallback = true }},
		{"incomplete pass followed by a complete one", auto, rollbackFromBuild, rollbackFromBuild, func(r *chOptResolution) { r.FleetIncomplete = true }},
		{"held pass followed by one that reached every build", auto, rollbackFromBuild, rollbackFromBuild, func(r *chOptResolution) { r.FleetHeld = true }},
		{"incomplete pass followed by a complete one whose resolve fails", explicit, argAndMaxFloorBuild, balancedOldBuild, func(r *chOptResolution) { r.FleetIncomplete = true }},
		{"held pass followed by one whose resolve fails", explicit, argAndMaxFloorBuild, balancedOldBuild, func(r *chOptResolution) { r.FleetHeld = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.ClickHouse.Addr = unreachableAddr(t)
			cfg.ClickHouse.DialTimeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// The resolution in force is what resolving against tc.inForce
			// produces, with the analyzer and shape-defect decisions the
			// pass's fleet implies and the flags of an earlier pass on top.
			fleet := chclient.FleetVersions{Versions: []chopt.Version{tc.pass}, Complete: true}
			inForce, ok := resolveCHOptimizationsOnce(ctx, quietLogger(), cfg, "",
				chclient.FleetVersions{Versions: []chopt.Version{tc.inForce}, Complete: true}, nil)
			if !ok {
				t.Fatalf("premise: resolve against %s failed", tc.inForce)
			}
			inForce.Set = inForce.Set.WithAnalyzerMandatory(analyzerMandatoryOnFleet(fleet, false)).
				WithShapeDefects(shapeDefectsOnFleet(fleet, inForce.Set))
			tc.flag(&inForce)
			live := newCHOptLive(inForce)

			// The second probe starts after the first pass has published,
			// so what is in force then is what that pass left.
			afterFirst := make(chan chOptResolution, 1)
			var probes atomic.Int32
			probe := func(context.Context) chclient.FleetVersions {
				if probes.Add(1) == 2 {
					afterFirst <- live.get()
				}
				return fleet
			}
			go reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{},
				reprobeTiming{steady: time.Millisecond, earlySpacing: time.Hour}, newReprobeTrigger(), "", probe)
			var got chOptResolution
			select {
			case got = <-afterFirst:
			case <-ctx.Done():
				t.Fatal("the re-probe did not run a second pass")
			}
			if !got.Set.Equal(inForce.Set) || got.ResolvedVersion != inForce.ResolvedVersion || got.QueryWorkload != inForce.QueryWorkload {
				t.Fatalf("premise: the pass moved the capabilities: %+v; want only the flags of %+v to change", got, inForce)
			}
			if got.VersionFallback || got.FleetIncomplete || got.FleetHeld {
				t.Errorf("after a complete pass that reached %s: fallback=%v incomplete=%v held=%v; want all false",
					tc.pass, got.VersionFallback, got.FleetIncomplete, got.FleetHeld)
			}
			if ready, reason := capabilitiesResolved(newCHOptLive(got))(); !ready {
				t.Errorf("/readyz capability condition = not ready (%s); want ready once a probe answered", reason)
			}
		})
	}
}
