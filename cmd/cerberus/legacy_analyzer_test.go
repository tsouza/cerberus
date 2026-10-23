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

// TestLegacyAnalyzerAllowed_ConservativeAcrossTheFleet pins the decision
// table: any reached node on a mandatory-analyzer build withholds the stamp;
// only a complete pass with every node on an older build grants it; an
// incomplete pass with no mandatory-analyzer node leaves the decision in force.
func TestLegacyAnalyzerAllowed_ConservativeAcrossTheFleet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fv      chclient.FleetVersions
		current bool
		want    bool
	}{
		{"single mandatory node", chclient.FleetVersions{Versions: []chopt.Version{analyzerMandatoryBuild}, Complete: true}, true, false},
		{"rolling upgrade, one node upgraded", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerMandatoryBuild}, Complete: true}, true, false},
		{"mandatory node seen, others unreachable", chclient.FleetVersions{Versions: []chopt.Version{analyzerMandatoryBuild}}, true, false},
		{"every node older", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerOptionalBuild}, Complete: true}, false, true},
		{"older nodes seen, one unreachable, stamp withheld", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild}}, false, false},
		{"older nodes seen, one unreachable, stamp granted", chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild}}, true, true},
		{"nothing reachable", chclient.FleetVersions{}, true, true},
	}
	for _, tc := range cases {
		if got := legacyAnalyzerAllowed(tc.fv, tc.current); got != tc.want {
			t.Errorf("%s: legacyAnalyzerAllowed = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestReprobe_LegacyAnalyzerWithheldMidRollingUpgrade is the rolling-upgrade
// case: the process resolved on an older build, then one node moves to a
// mandatory-analyzer build while another stays behind. The ids resolve against
// the older node and do not move, yet the analyzer decision must flip, so
// queries landing on the upgraded node stop carrying enable_analyzer=0.
func TestReprobe_LegacyAnalyzerWithheldMidRollingUpgrade(t *testing.T) {
	t.Parallel()

	boot := resolutionAt(t, analyzerOptionalBuild)
	if !boot.Set.LegacyAnalyzer() {
		t.Fatal("premise: a set resolved on the older build must permit the legacy analyzer")
	}
	mixed := chclient.FleetVersions{Versions: []chopt.Version{analyzerOptionalBuild, analyzerMandatoryBuild}, Complete: true}
	got := reprobeUntilLegacyAnalyzerWithheld(t, config.Config{CHOptimizations: "off", Schema: schema.DefaultOTelMetrics()}, boot, mixed)
	if got.ResolvedVersion != analyzerOptionalBuild || !got.Set.Equal(boot.Set.WithLegacyAnalyzer(false)) {
		t.Fatalf("the re-probe moved more than the analyzer decision: got %+v, boot %+v", got, boot)
	}
}

// TestReprobe_LegacyAnalyzerWithheldWhenResolveFails pins that the analyzer
// decision follows the fleet even on a pass whose resolution is refused: an
// explicit join_spill under enforcing, resolved on a capable build, then a
// node below join_spill's floor and a node on a mandatory-analyzer build both
// answer. Resolve refuses the selection against the older node and the
// re-probe keeps the set in force, but not its analyzer decision.
func TestReprobe_LegacyAnalyzerWithheldWhenResolveFails(t *testing.T) {
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
	got := reprobeUntilLegacyAnalyzerWithheld(t, cfg, boot, fleet)
	if got.ResolvedVersion != analyzerOptionalBuild || !got.Set.Has(chopt.FeatureJoinSpill) {
		t.Fatalf("premise: the refused re-resolve should have kept the set in force; got %+v", got)
	}
}

// reprobeUntilLegacyAnalyzerWithheld runs the re-probe from boot against a
// fixed fleet until the live resolution withholds the legacy analyzer, and
// returns that resolution.
func reprobeUntilLegacyAnalyzerWithheld(t *testing.T, cfg config.Config, boot chOptResolution, fleet chclient.FleetVersions) chOptResolution {
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
	for live.get().Set.LegacyAnalyzer() {
		if time.Now().After(deadline) {
			t.Fatal("the legacy analyzer was never withheld after a node moved to a mandatory-analyzer build")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return live.get()
}
