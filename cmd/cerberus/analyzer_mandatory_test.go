package main

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// Builds on either side of chopt.AnalyzerMandatorySince.
var (
	analyzerOptionalBuild  = chopt.Version{Major: 26, Minor: 8, Patch: 10, Build: 6}
	analyzerMandatoryBuild = chopt.Version{Major: 26, Minor: 9, Patch: 25, Build: 15}
)

// TestAnalyzerMandatoryOnFleet_ConservativeAcrossTheFleet pins the decision
// table: any reached node on a mandatory-analyzer build makes it mandatory;
// only a complete pass with every node on an older build clears it; an
// incomplete pass with no mandatory-analyzer node leaves the decision in force.
func TestAnalyzerMandatoryOnFleet_ConservativeAcrossTheFleet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fv      chclient.FleetVersions
		current bool
		want    bool
	}{
		{"single mandatory node", chclient.FleetVersions{Versions: []chopt.Version{analyzerMandatoryBuild}, Complete: true}, false, true},
		{"rolling upgrade, one node upgraded", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerMandatoryBuild}, Complete: true}, false, true},
		{"mandatory node seen, others unreachable", chclient.FleetVersions{Versions: []chopt.Version{analyzerMandatoryBuild}}, false, true},
		{"every node older", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerOptionalBuild}, Complete: true}, true, false},
		{"older nodes seen, one unreachable, mandatory in force", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild}}, true, true},
		{"older nodes seen, one unreachable, not mandatory in force", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild}}, false, false},
		{"nothing reachable, not mandatory in force", chclient.FleetVersions{}, false, false},
		{"nothing reachable, mandatory in force", chclient.FleetVersions{}, true, true},
	}
	for _, tc := range cases {
		if got := analyzerMandatoryOnFleet(tc.fv, tc.current); got != tc.want {
			t.Errorf("%s: analyzerMandatoryOnFleet = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolveCHOptimizations_MixedFleetMakesAnalyzerMandatory is the boot
// half of the rolling-upgrade case: the process starts while one node already
// runs a mandatory-analyzer build and another does not. The ids resolve
// against the older node, but the set must already report the analyzer
// mandatory, so no query carries enable_analyzer=0 to the upgraded node before
// the first re-probe.
func TestResolveCHOptimizations_MixedFleetMakesAnalyzerMandatory(t *testing.T) {
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
		{"mixed fleet", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerMandatoryBuild}, Complete: true}, true},
		{"older fleet", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerOptionalBuild}, Complete: true}, false},
	} {
		probe := func(context.Context) chclient.FleetVersions { return tc.fleet }
		res, err := resolveCHOptimizations(context.Background(), quietLogger(), client, &cfg, probe)
		if err != nil {
			t.Fatalf("%s: resolveCHOptimizations: %v", tc.name, err)
		}
		if res.ResolvedVersion != analyzerOptionalBuild {
			t.Fatalf("%s: resolved against %s; want the oldest node, %s", tc.name, res.ResolvedVersion, analyzerOptionalBuild)
		}
		if got := res.Set.AnalyzerMandatory(); got != tc.want {
			t.Errorf("%s: AnalyzerMandatory() = %v at boot; want %v", tc.name, got, tc.want)
		}
	}
}

// TestReprobe_AnalyzerMandatoryMidRollingUpgrade is the rolling-upgrade case:
// the process resolved on an older build, then one node moves to a
// mandatory-analyzer build while another stays behind. The ids resolve against
// the older node and do not move, yet the analyzer decision must flip, so
// queries landing on the upgraded node stop carrying enable_analyzer=0.
func TestReprobe_AnalyzerMandatoryMidRollingUpgrade(t *testing.T) {
	t.Parallel()

	boot := resolutionAt(t, analyzerOptionalBuild)
	if boot.Set.AnalyzerMandatory() {
		t.Fatal("premise: a set resolved on the older build must not report the analyzer mandatory")
	}
	mixed := chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerMandatoryBuild}, Complete: true}
	got := reprobeUntilAnalyzerMandatory(t, config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}, boot, mixed)
	if got.ResolvedVersion != analyzerOptionalBuild || !got.Set.Equal(boot.Set.WithAnalyzerMandatory(true)) {
		t.Fatalf("the re-probe moved more than the analyzer decision: got %+v, boot %+v", got, boot)
	}
}

// TestReprobe_AnalyzerMandatoryWhenResolveFails pins that the analyzer
// decision follows the fleet even on a pass whose resolution is refused: an
// explicit join_spill under enforcing, resolved on a capable build, then a
// node below join_spill's floor and a node on a mandatory-analyzer build both
// answer. Resolve refuses the selection against the older node and the
// re-probe keeps the set in force, but not its analyzer decision.
func TestReprobe_AnalyzerMandatoryWhenResolveFails(t *testing.T) {
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
	boot := resolutionAt(t, analyzerOptionalBuild, chopt.FeatureJoinSpill)
	fleet := chclient.FleetVersions{Versions: []chopt.Version{belowJoinSpill, analyzerMandatoryBuild}, Complete: true}
	got := reprobeUntilAnalyzerMandatory(t, cfg, boot, fleet)
	if got.ResolvedVersion != analyzerOptionalBuild || !got.Set.Has(chopt.FeatureJoinSpill) {
		t.Fatalf("premise: the refused re-resolve should have kept the set in force; got %+v", got)
	}
}

// reprobeUntilAnalyzerMandatory runs the re-probe from boot against a fixed
// fleet until the live resolution reports the analyzer mandatory, and returns
// that resolution.
func reprobeUntilAnalyzerMandatory(t *testing.T, cfg config.Config, boot chOptResolution, fleet chclient.FleetVersions) chOptResolution {
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
	for !live.get().Set.AnalyzerMandatory() {
		if time.Now().After(deadline) {
			t.Fatal("the analyzer was never reported mandatory after a node moved to a mandatory-analyzer build")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return live.get()
}
