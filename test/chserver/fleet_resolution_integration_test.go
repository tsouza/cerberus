//go:build integration

package chserver

import (
	"context"
	"io"
	"maps"
	"net"
	"strings"
	"sync"
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

// Credentials and settings profile of the canary user below: on one server
// its profile pins the experimental time-series setting, so the ts_grid
// canary's stamp is a constraint violation there.
const (
	canaryUser     = "fleet_canary"
	canaryPassword = "fleet_canary"
	canaryProfile  = "fleet_canary_pinned"
)

// canaryServerUUID reads s's own serverUUID(), as the fleet probe renders it.
func canaryServerUUID(ctx context.Context, t *testing.T, s *server) string {
	t.Helper()
	rows, err := s.admin.QueryStrings(ctx, "SELECT toString(serverUUID())")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%s: read serverUUID() = %v, %v", s.image, rows, err)
	}
	return rows[0]
}

// fleetCanaryNames are the canaries the tests below run.
const (
	tsGridCanary      = "ts_grid"
	resultCacheCanary = "result_cache"
)

// fleetCanaries runs the real ts_grid and result_cache canaries.
var fleetCanaries = []chclient.FleetCanary{
	{Name: tsGridCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeTSGridCapability(ctx)
	}},
	{Name: resultCacheCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
		return c.ProbeResultCacheCapability(ctx)
	}},
}

// startCanaryPair boots two servers on one build, each with the canary user:
// on pinned the user's profile pins the experimental time-series setting, so
// the ts_grid canary's stamp is refused there; on open nothing is pinned.
func startCanaryPair(ctx context.Context, t *testing.T) (pinned, open *server) {
	t.Helper()
	pinned = startServer(ctx, t, fleetNewImage)
	open = startServer(ctx, t, fleetNewImage)
	pinned.exec(ctx, t, "CREATE SETTINGS PROFILE "+canaryProfile+" SETTINGS "+chclient.SettingExperimentalTSGridAggregate+" = 0 CONST")
	pinned.exec(ctx, t, "CREATE USER "+canaryUser+" IDENTIFIED BY '"+canaryPassword+"' SETTINGS PROFILE "+canaryProfile)
	open.exec(ctx, t, "CREATE USER "+canaryUser+" IDENTIFIED BY '"+canaryPassword+"'")
	return pinned, open
}

// canaryConfig is the canary user's bootstrap config over addrs.
func canaryConfig(addrs ...string) chclient.Config {
	return chclient.Config{
		Addr:            addrs[0],
		Addrs:           addrs,
		Database:        serverDB,
		Username:        canaryUser,
		Password:        canaryPassword,
		BreakerDisabled: true,
	}
}

// passVerdicts collects, per server identity, the verdict of every canary a
// pass ran on it, failing on a canary run twice on one server.
func passVerdicts(t *testing.T, pass chclient.FleetPass) map[string]map[string]chopt.Capability {
	t.Helper()
	got := map[string]map[string]chopt.Capability{}
	for _, n := range pass.Nodes {
		if got[n.ID] == nil {
			got[n.ID] = map[string]chopt.Capability{}
		}
		for canary, v := range n.Verdicts {
			if prev, dup := got[n.ID][canary]; dup {
				t.Fatalf("server %s gave %s twice in one pass (%s, %s); want each canary run once per address", n.ID, canary, prev, v)
			}
			got[n.ID][canary] = v
		}
	}
	return got
}

// TestFleetProbe_CanaryVerdictsBelongToTheServerThatGaveThem probes two
// servers behind one multi-address config with the real ts_grid and
// result_cache canaries. The pinned server refuses the ts_grid stamp — an
// error, which closes the connection — so its result_cache verdict is read
// over a new connection, and must still be attributed to it. The fleet the
// memory describes then refuses ts_grid and permits result_cache.
func TestFleetProbe_CanaryVerdictsBelongToTheServerThatGaveThem(t *testing.T) {
	ctx := context.Background()
	pinned, open := startCanaryPair(ctx, t)
	cfg := canaryConfig(pinned.addr, open.addr)
	pass := chclient.ProbeFleet(ctx, cfg, "", fleetCanaries...)
	if !pass.Complete {
		t.Fatalf("fleet probe over %v = %+v; want a complete pass", cfg.Addrs, pass)
	}

	want := map[string]map[string]chopt.Capability{
		canaryServerUUID(ctx, t, pinned): {tsGridCanary: chopt.CapabilityForbidden, resultCacheCanary: chopt.CapabilityAvailable},
		canaryServerUUID(ctx, t, open):   {tsGridCanary: chopt.CapabilityAvailable, resultCacheCanary: chopt.CapabilityAvailable},
	}
	if got := passVerdicts(t, pass); !maps.EqualFunc(got, want, maps.Equal) {
		t.Fatalf("pass verdicts by server = %v; want %v", got, want)
	}
	fv := chclient.NewFleetMemory().Observe(pass)
	if fv.Capability(tsGridCanary) != chopt.CapabilityForbidden || fv.Capability(resultCacheCanary) != chopt.CapabilityAvailable {
		t.Fatalf("fleet verdicts = %v; want ts_grid forbidden by the pinned server, result_cache available", fv.Verdicts)
	}
}

// switchingBalancer is a TCP load balancer in front of ClickHouse servers.
// It sends each connection it accepts to the server address route names for
// that connection's index since route was set.
type switchingBalancer struct {
	addr  string
	mu    sync.Mutex
	route func(conn int) string
	conns int
}

// startSwitchingBalancer listens on a loopback port and forwards every
// connection it accepts until the test ends.
func startSwitchingBalancer(t *testing.T) *switchingBalancer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("balancer listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	b := &switchingBalancer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			backend := b.route(b.conns)
			b.conns++
			b.mu.Unlock()
			go forward(c, backend)
		}
	}()
	return b
}

// routeEach makes route decide every later connection, counted from zero.
func (b *switchingBalancer) routeEach(route func(conn int) string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.route, b.conns = route, 0
}

// forward pipes c to backend both ways until either side closes.
func forward(c net.Conn, backend string) {
	defer func() { _ = c.Close() }()
	d, err := net.Dial("tcp", backend)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	go func() {
		_, _ = io.Copy(d, c)
		_ = d.Close()
	}()
	_, _ = io.Copy(c, d)
}

// canaryBalancedPasses is how many passes the load-balanced test alternates
// between the two servers.
const canaryBalancedPasses = 6

// TestFleetProbe_LoadBalancedCanaryHoldsTheRefusingReplica puts the two
// servers behind one load-balanced address. First, one pass whose first
// connection reaches the pinned server and every later one the open server:
// the ts_grid refusal closes the first connection, the result_cache canary
// runs on the open server, and each verdict must be attributed to the server
// that gave it. Then passes alternate between the servers, every connection
// of a pass reaching one of them; from the first pass on the pinned server,
// the fleet's ts_grid verdict must stay forbidden on every pass, including
// the ones that land on the open server.
func TestFleetProbe_LoadBalancedCanaryHoldsTheRefusingReplica(t *testing.T) {
	ctx := context.Background()
	pinned, open := startCanaryPair(ctx, t)
	pinnedID, openID := canaryServerUUID(ctx, t, pinned), canaryServerUUID(ctx, t, open)
	balancer := startSwitchingBalancer(t)
	cfg := canaryConfig(balancer.addr)

	balancer.routeEach(func(conn int) string {
		if conn == 0 {
			return pinned.addr
		}
		return open.addr
	})
	pass := chclient.ProbeFleet(ctx, cfg, "", fleetCanaries...)
	want := map[string]map[string]chopt.Capability{
		pinnedID: {tsGridCanary: chopt.CapabilityForbidden},
		openID:   {resultCacheCanary: chopt.CapabilityAvailable},
	}
	if got := passVerdicts(t, pass); !pass.Complete || !maps.EqualFunc(got, want, maps.Equal) {
		t.Fatalf("pass switching servers after the refusal: complete=%v, verdicts by server = %v; want complete, %v",
			pass.Complete, got, want)
	}

	memory := chclient.NewFleetMemory()
	for n := range canaryBalancedPasses {
		onPinned := n%2 == 1
		backend := open.addr
		if onPinned {
			backend = pinned.addr
		}
		balancer.routeEach(func(int) string { return backend })
		fv := memory.Observe(chclient.ProbeFleet(ctx, cfg, "", fleetCanaries...))
		want := chopt.CapabilityAvailable
		if n > 0 {
			want = chopt.CapabilityForbidden
		}
		if got := fv.Capability(tsGridCanary); got != want || fv.Capability(resultCacheCanary) != chopt.CapabilityAvailable {
			t.Errorf("pass %d (landed on the pinned server: %v): ts_grid %s, result_cache %s; want %s, %s",
				n+1, onPinned, got, fv.Capability(resultCacheCanary), want, chopt.CapabilityAvailable)
		}
	}
}
