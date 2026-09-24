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
		fv      fleetVersions
		current bool
		want    bool
	}{
		{"single unsafe node", fleetVersions{versions: []chopt.Version{ccUnsafeBuild}, complete: true}, false, true},
		{"mixed fleet, one unsafe replica", fleetVersions{versions: []chopt.Version{ccFixedBuild, ccUnsafeBuild}, complete: true}, false, true},
		{"unsafe node seen, others unreachable", fleetVersions{versions: []chopt.Version{ccUnsafeBuild}}, false, true},
		{"every node fixed", fleetVersions{versions: []chopt.Version{ccFixedBuild, ccFixedBuild}, complete: true}, true, false},
		{"fixed nodes seen, one unreachable, override on", fleetVersions{versions: []chopt.Version{ccFixedBuild}}, true, true},
		{"fixed nodes seen, one unreachable, override off", fleetVersions{versions: []chopt.Version{ccFixedBuild}}, false, false},
		{"nothing reachable", fleetVersions{}, true, true},
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
	consumers := chOptConsumers{
		client: client,
		fleet: func(context.Context) fleetVersions {
			return fleetVersions{versions: []chopt.Version{ccUnsafeBuild}, complete: true}
		},
	}
	rolledBack := func(context.Context, chclient.Config) (chopt.Version, error) { return ccUnsafeBuild, nil }

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
	probe := func(v chopt.Version) fleetProber {
		return func(context.Context) fleetVersions {
			return fleetVersions{versions: []chopt.Version{v}, complete: true}
		}
	}

	refreshConditionCacheOverride(context.Background(), quietLogger(), client, probe(ccUnsafeBuild))
	if !view.QueryConditionCacheDisabled() {
		t.Fatal("override off on a head view after an unsafe fleet probe")
	}
	refreshConditionCacheOverride(context.Background(), quietLogger(), client, probe(ccFixedBuild))
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
	if fv.complete || len(fv.versions) != 0 {
		t.Fatalf("fleet probe over unreachable addresses = %+v; want an incomplete pass with no versions", fv)
	}
	if !conditionCacheOverride(fv, true) {
		t.Fatal("an incomplete pass lifted the override")
	}
}
