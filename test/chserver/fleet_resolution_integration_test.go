//go:build integration

package chserver

import (
	"context"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"

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
	pass := chclient.ProbeFleet(ctx, cfg, "")
	if !pass.Complete || len(pass.Nodes) != len(cfg.Addrs) {
		t.Fatalf("fleet probe over %v = %+v; want a complete pass with one node per address", cfg.Addrs, pass)
	}
	if pass.Nodes[0].ID == "" || pass.Nodes[0].ID == pass.Nodes[1].ID {
		t.Fatalf("fleet probe node identities = %q, %q; want two distinct server UUIDs", pass.Nodes[0].ID, pass.Nodes[1].ID)
	}
	fv := chclient.NewFleetMemory().Observe(pass)
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

// Network aliases and cluster name of the two-server cluster below.
const (
	fleetNewAlias = "fleet-newer"
	fleetOldAlias = "fleet-older"
	fleetCluster  = "fleet"
)

// fleetClusterConfig defines fleetCluster on a server: one shard whose two
// replicas are the servers at the given network aliases, reached with the
// tests' administrative credentials.
func fleetClusterConfig(aliases ...string) testcontainers.ContainerCustomizer {
	var replicas strings.Builder
	for _, alias := range aliases {
		replicas.WriteString("<replica><host>" + alias + "</host><port>9000</port>" +
			"<user>" + adminUser + "</user><password>" + adminPassword + "</password></replica>")
	}
	config := "<clickhouse><remote_servers><" + fleetCluster + "><shard>" + replicas.String() +
		"</shard></" + fleetCluster + "></remote_servers></clickhouse>"
	return testcontainers.WithFiles(testcontainers.ContainerFile{
		Reader:            strings.NewReader(config),
		ContainerFilePath: "/etc/clickhouse-server/config.d/fleet-cluster.xml",
		FileMode:          configFileMode,
	})
}

// TestFleetProbe_ClusterArmReadsEveryReplicasIdentity boots two builds as the
// two replicas of one cluster and probes the fleet through the newer one's
// address alone. The cluster arm must read each replica's own serverUUID()
// and version() — evaluated on the replica, not folded on the initiator — so
// the pass holds both servers, the address's node is one of them, and the
// fleet the memory describes follows the older build.
func TestFleetProbe_ClusterArmReadsEveryReplicasIdentity(t *testing.T) {
	ctx := context.Background()
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })
	older := startServer(ctx, t, fleetOldImage, network.WithNetwork([]string{fleetOldAlias}, nw))
	newer := startServer(ctx, t, fleetNewImage, network.WithNetwork([]string{fleetNewAlias}, nw),
		fleetClusterConfig(fleetNewAlias, fleetOldAlias))

	cfg := chclient.Config{
		Addr:            newer.addr,
		Database:        serverDB,
		Username:        adminUser,
		Password:        adminPassword,
		BreakerDisabled: true,
	}
	pass := chclient.ProbeFleet(ctx, cfg, fleetCluster)
	if !pass.Complete || !pass.Enumerated {
		t.Fatalf("fleet probe through %s with cluster %q = %+v; want a complete pass that read the replica list", newer.addr, fleetCluster, pass)
	}
	byID := map[string]chopt.Version{}
	for _, n := range pass.Nodes {
		byID[n.ID] = n.Version
	}
	if len(byID) != 2 {
		t.Fatalf("fleet probe nodes = %+v; want the two replicas, each under its own server UUID", pass.Nodes)
	}
	if v, ok := byID[pass.Nodes[0].ID]; !ok || v != newer.version {
		t.Fatalf("address node %+v; want the newer replica on %s", pass.Nodes[0], newer.version)
	}
	builds := map[chopt.Version]bool{}
	for _, v := range byID {
		builds[v] = true
	}
	if !builds[newer.version] || !builds[older.version] {
		t.Fatalf("replica builds = %v; want %s and %s", byID, newer.version, older.version)
	}
	lowest, ok := chclient.NewFleetMemory().Observe(pass).Lowest()
	if !ok || lowest != older.version {
		t.Fatalf("lowest fleet build = %v (ok=%v); want the older replica's %v", lowest, ok, older.version)
	}
}
