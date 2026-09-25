//go:build integration

package chserver

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// Builds on either side of join_spill's 26.4 floor, both already pinned by
// the other real-server probes in this package.
const (
	fleetOldImage = "clickhouse/clickhouse-server:25.3.14.14-alpine"
	fleetNewImage = "clickhouse/clickhouse-server:26.6.1.1193-alpine"
)

// TestFleetResolution_FollowsTheOldestAddress boots two builds on either side
// of a feature floor behind one multi-address client config and requires the
// fleet probe to read both, and the optimization set resolved against the
// fleet to follow the older build: the newer node alone would enable
// join_spill, whose 26.4 floor the older node does not meet.
func TestFleetResolution_FollowsTheOldestAddress(t *testing.T) {
	ctx := context.Background()
	newer := startServer(ctx, t, fleetNewImage)
	older := startServer(ctx, t, fleetOldImage)

	cfg := chclient.Config{
		Addr:            newer.addr,
		Addrs:           []string{newer.addr, older.addr},
		Database:        serverDB,
		Username:        adminUser,
		Password:        adminPassword,
		BreakerDisabled: true,
	}
	fv := chclient.ProbeFleetVersions(ctx, cfg, "")
	if !fv.Complete || len(fv.Versions) != len(cfg.Addrs) {
		t.Fatalf("fleet probe over %v = %+v; want a complete pass with one build per address", cfg.Addrs, fv)
	}
	lowest, ok := fv.Lowest()
	if !ok || lowest != older.version {
		t.Fatalf("lowest fleet build = %v (ok=%v); want the older node's %v", lowest, ok, older.version)
	}

	resolve := func(v chopt.Version) chopt.EnabledSet {
		t.Helper()
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: chopt.SelectionAuto, Mode: chopt.Permissive}, v)
		if err != nil {
			t.Fatalf("resolve auto against %s: %v", v, err)
		}
		return set
	}
	if !resolve(newer.version).Has(chopt.FeatureJoinSpill) {
		t.Fatalf("premise: auto against the newer node %s must enable %s", newer.version, chopt.FeatureJoinSpill)
	}
	if set := resolve(lowest); set.Has(chopt.FeatureJoinSpill) {
		t.Fatalf("auto against the fleet (%s) enabled %s: %v", lowest, chopt.FeatureJoinSpill, set.IDs())
	}
}
