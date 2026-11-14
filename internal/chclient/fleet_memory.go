package chclient

import (
	"slices"
	"sync"

	"github.com/tsouza/cerberus/internal/chopt"
)

// fleetNodeRetentionPassesPerServer sets how long FleetMemory keeps a server
// no complete pass reaches: it is forgotten once that many consecutive complete
// passes per remembered server have missed it (retentionPasses).
//
// Behind a load balancer each pass reaches one replica, so a serving replica
// is missed by most passes, and forgetting it early lets the version rise back
// above its build. Under uniform random routing across N replicas, the chance
// that 40·N passes in a row all miss one of them is (1-1/N)^(40·N), below
// e^-40 (about 4e-18) for every N, so scaling the count with the fleet keeps
// that chance flat however many replicas the balancer fronts. The cost runs
// the other way only for a server that left the fleet: it keeps counting for
// 40 complete passes per remembered server, which at the re-probe's 10-second
// cadence while the memory holds a build the last pass did not reach
// (FleetVersions.Held) is under seven minutes per server.
const fleetNodeRetentionPassesPerServer = 40

// retentionPasses is how many consecutive complete passes may miss a server
// before a memory holding that many servers forgets it.
func retentionPasses(servers int) int {
	return fleetNodeRetentionPassesPerServer * servers
}

// FleetMemory holds every ClickHouse server fleet probe passes have reached,
// by identity, so that a server a pass did not reach still counts as part of
// the fleet.
//
// A pass made through one load-balanced address reaches the one replica its
// connection landed on, and reports itself complete because it reached every
// address. Without memory the fleet would be whichever replica the last pass
// landed on, and during a rolling rollback the version would follow it up and
// down. With it, the fleet is every server a pass has reached and not yet
// forgotten:
//
//   - A reached server's entry takes the build it reported, so a replica that
//     rolls forward raises its own entry on the first pass that reaches it.
//   - A complete pass that does not reach a remembered server counts a miss
//     against it; retentionPasses consecutive misses forget it.
//   - A complete pass that read the named cluster's replica list
//     (FleetPass.Enumerated) enumerates the fleet: the memory becomes exactly
//     the servers it reached.
//   - An incomplete pass counts no miss, because the server it missed may sit
//     behind the address that failed.
//   - A pass that reached no server leaves the memory as it is and describes
//     no fleet, so the caller falls back to the supported floor exactly as
//     before any pass ran.
//   - A pass that reads one identity on two builds — servers cloned from one
//     data directory share a serverUUID() — keeps the older build for it, so
//     the clone on the older build still holds the fleet down.
//
// The zero value is not usable; build one with NewFleetMemory. It is safe for
// concurrent use.
type FleetMemory struct {
	mu    sync.Mutex
	nodes map[string]*rememberedNode
}

// rememberedNode is one server FleetMemory holds: the build it reported the
// last time a pass reached it, and how many complete passes in a row have
// missed it since.
type rememberedNode struct {
	version chopt.Version
	missed  int
}

// NewFleetMemory returns a memory that holds no server yet.
func NewFleetMemory() *FleetMemory {
	return &FleetMemory{nodes: make(map[string]*rememberedNode)}
}

// Observe folds pass into the memory and returns the fleet the memory now
// describes: the build of every server it holds, sorted oldest first, with
// pass's completeness, and Held set when one of those builds is a build no
// server pass reached runs.
func (m *FleetMemory) Observe(pass FleetPass) FleetVersions {
	if len(pass.Nodes) == 0 {
		return FleetVersions{Complete: pass.Complete}
	}
	reached := make(map[string]chopt.Version, len(pass.Nodes))
	reachedBuilds := make(map[chopt.Version]bool, len(pass.Nodes))
	for _, n := range pass.Nodes {
		reachedBuilds[n.Version] = true
		if v, dup := reached[n.ID]; dup && v.Less(n.Version) {
			continue
		}
		reached[n.ID] = n.Version
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if pass.Complete && pass.Enumerated {
		clear(m.nodes)
	}
	for id, v := range reached {
		m.nodes[id] = &rememberedNode{version: v}
	}
	if pass.Complete && !pass.Enumerated {
		limit := retentionPasses(len(m.nodes))
		for id, n := range m.nodes {
			if _, ok := reached[id]; ok {
				continue
			}
			n.missed++
			if n.missed >= limit {
				delete(m.nodes, id)
			}
		}
	}

	fv := FleetVersions{Versions: make([]chopt.Version, 0, len(m.nodes)), Complete: pass.Complete}
	for _, n := range m.nodes {
		fv.Versions = append(fv.Versions, n.version)
		fv.Held = fv.Held || !reachedBuilds[n.version]
	}
	slices.SortFunc(fv.Versions, chopt.Version.Compare)
	return fv
}
