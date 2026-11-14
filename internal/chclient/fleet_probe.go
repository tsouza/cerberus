package chclient

import (
	"context"

	"github.com/tsouza/cerberus/internal/chopt"
)

// FleetNode is one ClickHouse server a fleet probe pass reached: its
// serverUUID(), which names one server across restarts and builds, and its
// build.
type FleetNode struct {
	ID      string
	Version chopt.Version
}

// FleetPass is what one pass of the fleet probe read: every server it
// reached, whether it reached every node it knows of — each configured
// address and, when a cluster is named, every replica of that cluster — and
// whether it read that cluster's replica list (Enumerated).
type FleetPass struct {
	Nodes      []FleetNode
	Complete   bool
	Enumerated bool
}

// FleetVersions is the fleet a FleetMemory describes after a pass: the build
// of every ClickHouse server it holds, and whether the pass reached every node
// it knows of.
type FleetVersions struct {
	Versions []chopt.Version
	Complete bool
	// Held reports that Versions holds a build no server the pass reached
	// runs: a server an earlier pass reached, which the memory has not
	// forgotten, runs it. Every decision read off Versions may then rest on
	// that server.
	Held bool
}

// Lowest returns the oldest build of the fleet, or ok=false when it holds
// none.
func (fv FleetVersions) Lowest() (chopt.Version, bool) {
	return chopt.LowestVersion(fv.Versions)
}

// ProbeFleet reads the identity and build of every node cfg can reach: each
// address in cfg.Addrs (or cfg.Addr alone), over one short-lived connection per
// address, and — when cluster is non-empty — every replica of that cluster
// through clusterAllReplicas on the first address that answers. A node behind
// a load balancer that neither list names cannot be reached individually; a
// pass reads it only when its connection lands on it, and FleetMemory holds
// it between such passes.
//
// A failed dial, a failed read, or a failed cluster read leaves the pass
// incomplete rather than silently reporting a smaller fleet.
func ProbeFleet(ctx context.Context, cfg Config, cluster string) FleetPass {
	hosts := cfg.Addrs
	if len(hosts) == 0 {
		hosts = []string{cfg.Addr}
	}
	pass := FleetPass{Complete: true}
	var reached *Client
	for _, host := range hosts {
		hostCfg := cfg
		hostCfg.Addrs = nil
		hostCfg.Addr = host
		c, err := New(hostCfg)
		if err != nil {
			pass.Complete = false
			continue
		}
		node, err := c.ProbeNode(ctx)
		if err != nil {
			pass.Complete = false
			_ = c.Close()
			continue
		}
		pass.Nodes = append(pass.Nodes, node)
		if reached == nil {
			reached = c
			continue
		}
		_ = c.Close()
	}
	if reached == nil {
		return pass
	}
	defer func() { _ = reached.Close() }()
	if cluster != "" {
		nodes, err := reached.ProbeClusterNodes(ctx, cluster)
		if err != nil {
			pass.Complete = false
		} else {
			pass.Nodes = append(pass.Nodes, nodes...)
			pass.Enumerated = true
		}
	}
	return pass
}
