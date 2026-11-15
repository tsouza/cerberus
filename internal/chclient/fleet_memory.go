package chclient

import (
	"cmp"
	"maps"
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
// down; so would every capability canary verdict on which the replicas
// disagree. With it, the fleet is every server a pass has reached and not yet
// forgotten:
//
//   - A reached server's entry takes the build it reported, so a replica that
//     rolls forward raises its own entry on the first pass that reaches it.
//   - A reached server's entry takes the verdict of every canary the pass ran
//     on it, and keeps the remembered verdict of a canary the pass did not
//     run on it or got no verdict from (CapabilityUnreachable).
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
//     the clone on the older build still holds the fleet down. Two verdicts
//     on one canary for one identity keep the more conservative one.
//
// The zero value is not usable; build one with NewFleetMemory. It is safe for
// concurrent use.
type FleetMemory struct {
	mu     sync.Mutex
	nodes  map[string]*rememberedNode
	passes int
}

// rememberedNode is one server FleetMemory holds: the build it reported the
// last time a pass reached it, the last verdict it gave on each canary a pass
// ran on it, the pass that last ran each canary on it, and how many complete
// passes in a row have missed it since.
type rememberedNode struct {
	version  chopt.Version
	verdicts map[string]chopt.Capability
	ranAt    map[string]int
	missed   int
}

// NewFleetMemory returns a memory that holds no server yet.
func NewFleetMemory() *FleetMemory {
	return &FleetMemory{nodes: make(map[string]*rememberedNode)}
}

// CanaryOrder orders pending for the server named id: the canaries no pass has
// run on it first, then the one run on it longest ago, keeping pending's order
// among equals. A canary the server refuses closes the connection, so a pass
// that reaches a server runs it only up to its first refusal; ordering by the
// server's own history runs every canary on it within as many passes that
// reach it as there are canaries, however a load balancer hands out
// connections. It is a CanaryOrder for ProbeFleet.
func (m *FleetMemory) CanaryOrder(id string, pending []FleetCanary) []FleetCanary {
	m.mu.Lock()
	defer m.mu.Unlock()
	ranAt := func(c FleetCanary) int {
		if n, ok := m.nodes[id]; ok {
			if at, ran := n.ranAt[c.Name]; ran {
				return at
			}
		}
		return -1
	}
	ordered := slices.Clone(pending)
	slices.SortStableFunc(ordered, func(a, b FleetCanary) int { return cmp.Compare(ranAt(a), ranAt(b)) })
	return ordered
}

// Observe folds pass into the memory and returns the fleet the memory now
// describes: the build of every server it holds, sorted oldest first, the
// most conservative verdict they gave on each canary, pass's completeness,
// and Held set when one of those builds is one no server pass reached runs,
// or the verdict in force on a canary is one no server pass reached holds.
func (m *FleetMemory) Observe(pass FleetPass) FleetVersions {
	if len(pass.Nodes) == 0 {
		return FleetVersions{Complete: pass.Complete}
	}
	reached := make(map[string]*rememberedNode, len(pass.Nodes))
	reachedBuilds := make(map[chopt.Version]bool, len(pass.Nodes))
	for _, n := range pass.Nodes {
		reachedBuilds[n.Version] = true
		r, dup := reached[n.ID]
		if !dup {
			r = &rememberedNode{version: n.Version, verdicts: make(map[string]chopt.Capability, len(n.Verdicts))}
			reached[n.ID] = r
		} else if n.Version.Less(r.version) {
			r.version = n.Version
		}
		for canary, v := range n.Verdicts {
			r.verdicts[canary] = r.verdicts[canary].Conservative(v)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.passes++
	for id, r := range reached {
		r.ranAt = make(map[string]int, len(r.verdicts))
		prev, known := m.nodes[id]
		if known {
			maps.Copy(r.ranAt, prev.ranAt)
		}
		for canary, v := range r.verdicts {
			r.ranAt[canary] = m.passes
			// A canary that got no verdict says nothing about the server, so
			// it keeps the verdict remembered for it.
			if was, ok := prev.verdictOn(canary); ok && v == chopt.CapabilityUnreachable {
				r.verdicts[canary] = was
			}
		}
		if known {
			for canary, v := range prev.verdicts {
				if _, read := r.verdicts[canary]; !read {
					r.verdicts[canary] = v
				}
			}
		}
	}
	if pass.Complete && pass.Enumerated {
		clear(m.nodes)
	}
	maps.Copy(m.nodes, reached)
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
		for canary, v := range n.verdicts {
			if fv.Verdicts == nil {
				fv.Verdicts = make(map[string]chopt.Capability, len(n.verdicts))
			}
			fv.Verdicts[canary] = fv.Verdicts[canary].Conservative(v)
		}
	}
	for canary, v := range fv.Verdicts {
		fv.Held = fv.Held || !reachedHolds(reached, canary, v)
	}
	slices.SortFunc(fv.Versions, chopt.Version.Compare)
	return fv
}

// verdictOn returns the verdict n remembers on canary, ok=false when n is nil
// or remembers none.
func (n *rememberedNode) verdictOn(canary string) (chopt.Capability, bool) {
	if n == nil {
		return chopt.CapabilityUnknown, false
	}
	v, ok := n.verdicts[canary]
	return v, ok
}

// reachedHolds reports whether a server the pass reached holds verdict on
// canary, read this pass or remembered for it.
func reachedHolds(reached map[string]*rememberedNode, canary string, verdict chopt.Capability) bool {
	for _, r := range reached {
		if v, ok := r.verdicts[canary]; ok && v == verdict {
			return true
		}
	}
	return false
}
