package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// Builds on either side of condition_cache's known-unsafe ranges.
var (
	ccUnsafeBuild = chopt.Version{Major: 26, Minor: 2, Patch: 19, Build: 43}
	ccFixedBuild  = chopt.Version{Major: 26, Minor: 6, Patch: 1, Build: 1193}
)

// TestConditionCacheOverride_ConservativeAcrossTheFleet pins the decision
// table: any reached node on an unsafe build turns the override on; only a
// complete pass with every node on a fixed build turns it off; an incomplete
// pass with no unsafe node leaves the decision in force.
func TestConditionCacheOverride_ConservativeAcrossTheFleet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fv      chclient.FleetVersions
		current bool
		want    bool
	}{
		{"single unsafe node", chclient.FleetVersions{Versions: []chopt.Version{ccUnsafeBuild}, Complete: true}, false, true},
		{"mixed fleet, one unsafe replica", chclient.FleetVersions{Versions: []chopt.Version{ccFixedBuild, ccUnsafeBuild}, Complete: true}, false, true},
		{"unsafe node seen, others unreachable", chclient.FleetVersions{Versions: []chopt.Version{ccUnsafeBuild}}, false, true},
		{"every node fixed", chclient.FleetVersions{Versions: []chopt.Version{ccFixedBuild, ccFixedBuild}, Complete: true}, true, false},
		{"fixed nodes seen, one unreachable, override on", chclient.FleetVersions{Versions: []chopt.Version{ccFixedBuild}}, true, true},
		{"fixed nodes seen, one unreachable, override off", chclient.FleetVersions{Versions: []chopt.Version{ccFixedBuild}}, false, false},
		{"nothing reachable", chclient.FleetVersions{}, true, true},
	}
	for _, tc := range cases {
		if got := conditionCacheOverride(tc.fv, tc.current); got != tc.want {
			t.Errorf("%s: override = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestReprobe_ConditionCacheOverrideEngagesWhenResolveFails is the rollback
// case: an explicit condition_cache selection under enforcing, resolved on a
// fixed build, then the server moves into a known-unsafe build. Resolve now
// refuses the selection and the re-probe keeps the set in force — so the
// engine still stamps use_query_condition_cache=1 — yet the client override
// must engage, and win, on the very next pass.
func TestReprobe_ConditionCacheOverrideEngagesWhenResolveFails(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		CHOptimizations:     chopt.FeatureConditionCache,
		CHOptimizationsMode: chopt.Enforcing,
		Schema:              schema.DefaultOTelMetrics(),
	}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond

	if _, _, err := chopt.Resolve(chopt.Config{Optimizations: cfg.CHOptimizations, Mode: cfg.CHOptimizationsMode}, ccUnsafeBuild); err == nil {
		t.Fatal("premise: an explicit condition_cache under enforcing must be refused on the unsafe build")
	}

	client, err := chclient.New(chclient.Config{Addr: cfg.ClickHouse.Addr, Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	boot := resolutionAt(t, ccFixedBuild, chopt.FeatureConditionCache)
	live := newCHOptLive(boot)
	consumers := chOptConsumers{client: client}
	rolledBack := func(context.Context) chclient.FleetVersions {
		return chclient.FleetVersions{Versions: []chopt.Version{ccUnsafeBuild}, Complete: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, consumers, time.Millisecond, "", rolledBack)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for !client.QueryConditionCacheDisabled() {
		if time.Now().After(deadline) {
			t.Fatal("the override never engaged after the server moved into a known-unsafe build")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := live.get(); !got.Set.Has(chopt.FeatureConditionCache) || got.ResolvedVersion != ccFixedBuild {
		t.Fatalf("premise: the refused re-resolve should have kept the set in force; got %+v", got)
	}
}

// TestRefreshConditionCacheOverride_SharedByHeadViews pins that the refresh
// flips the one switch every ForHead view reads, in both directions.
func TestRefreshConditionCacheOverride_SharedByHeadViews(t *testing.T) {
	t.Parallel()

	client, err := chclient.New(chclient.Config{Addr: "127.0.0.1:1", Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	view := client.ForHead(chclient.HeadProm)
	probe := func(v chopt.Version) chclient.FleetVersions {
		return chclient.FleetVersions{Versions: []chopt.Version{v}, Complete: true}
	}

	refreshConditionCacheOverride(quietLogger(), client, probe(ccUnsafeBuild))
	if !view.QueryConditionCacheDisabled() {
		t.Fatal("override off on a head view after an unsafe fleet probe")
	}
	refreshConditionCacheOverride(quietLogger(), client, probe(ccFixedBuild))
	if view.QueryConditionCacheDisabled() {
		t.Fatal("override still on on a head view after every node answered from a fixed build")
	}
}

// TestLogCancellationGaps pins the operator-facing warning of the bounded-
// cancellation policy: one WARN per gap the build carries, none on a build
// with every fix.
func TestLogCancellationGaps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		server chopt.Version
		want   int
	}{
		{chopt.Version{Major: 26, Minor: 6, Patch: 1, Build: 1193}, 2},
		{chopt.Version{Major: 26, Minor: 7, Patch: 13, Build: 12}, 1},
		{chopt.Version{Major: 26, Minor: 8, Patch: 10, Build: 6}, 0},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		logCancellationGaps(slog.New(slog.NewTextHandler(&buf, nil)), tc.server)
		if got := strings.Count(buf.String(), "level=WARN"); got != tc.want {
			t.Errorf("%s: %d warnings, want %d:\n%s", tc.server, got, tc.want, buf.String())
		}
	}
}

// TestLiveFleetProber_UnreachableNodesMakeAnIncompletePass pins that a node
// the probe cannot reach — here every configured address — leaves the pass
// incomplete rather than silently reporting a smaller fleet, so the override
// decision stays as it was.
func TestLiveFleetProber_UnreachableNodesMakeAnIncompletePass(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.ClickHouse.Addrs = []string{unreachableAddr(t), unreachableAddr(t)}
	cfg.ClickHouse.Addr = cfg.ClickHouse.Addrs[0]
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fv := liveFleetProber(cfg)(ctx)
	if fv.Complete || len(fv.Versions) != 0 {
		t.Fatalf("fleet probe over unreachable addresses = %+v; want an incomplete pass with no versions", fv)
	}
	if !conditionCacheOverride(fv, true) {
		t.Fatal("an incomplete pass lifted the override")
	}
}

// Builds on either side of join_spill's 26.4 floor.
var (
	fleetOldBuild = chopt.Version{Major: 25, Minor: 3, Patch: 14, Build: 14}
	fleetNewBuild = chopt.Version{Major: 26, Minor: 6, Patch: 1, Build: 1193}
)

// TestFleetResolutionVersion pins the version feature resolution runs
// against: the oldest reached build; an incomplete pass never raises it above
// the version in force, except off a floor fallback; nothing reached is an
// error.
func TestFleetResolutionVersion(t *testing.T) {
	t.Parallel()

	held := &chOptResolution{ResolvedVersion: fleetOldBuild}
	fallback := &chOptResolution{ResolvedVersion: supportedFloorVersion, VersionFallback: true}
	cases := []struct {
		name    string
		fv      chclient.FleetVersions
		inForce *chOptResolution
		want    chopt.Version
		wantErr bool
	}{
		{"boot, mixed fleet", chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild, fleetOldBuild}, Complete: true}, nil, fleetOldBuild, false},
		{"boot, partial fleet", chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild}}, nil, fleetNewBuild, false},
		{"complete pass raises", chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild}, Complete: true}, held, fleetNewBuild, false},
		{"incomplete pass never raises", chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild}}, held, fleetOldBuild, false},
		{"incomplete pass lowers", chclient.FleetVersions{Versions: []chopt.Version{fleetOldBuild}}, &chOptResolution{ResolvedVersion: fleetNewBuild}, fleetOldBuild, false},
		{"incomplete pass lifts a floor fallback", chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild}}, fallback, fleetNewBuild, false},
		{"nothing reached", chclient.FleetVersions{}, held, chopt.Version{}, true},
	}
	for _, tc := range cases {
		got, err := fleetResolutionVersion(tc.fv, tc.inForce)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: fleetResolutionVersion = %v, %v; want %v, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestReprobe_ResolvesAgainstTheOldestNode — a fleet whose probed nodes sit
// on either side of join_spill's floor must resolve without it, however new
// the node the resolution in force was read from.
func TestReprobe_ResolvesAgainstTheOldestNode(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		CHOptimizations:     "auto",
		CHOptimizationsMode: chopt.Permissive,
		Schema:              schema.DefaultOTelMetrics(),
	}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond

	boot := resolutionAt(t, fleetNewBuild, chopt.FeatureJoinSpill)
	live := newCHOptLive(boot)
	mixed := func(context.Context) chclient.FleetVersions {
		return chclient.FleetVersions{Versions: []chopt.Version{fleetNewBuild, fleetOldBuild}, Complete: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{}, time.Millisecond, "", mixed)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for live.get().ResolvedVersion != fleetOldBuild {
		if time.Now().After(deadline) {
			t.Fatalf("the re-probe never resolved against the oldest node: %+v", live.get())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := live.get(); got.Set.Has(chopt.FeatureJoinSpill) || got.VersionFallback {
		t.Fatalf("resolved against %s but enabled=%v fallback=%v; want join_spill off", got.ResolvedVersion, got.Set.IDs(), got.VersionFallback)
	}
}
