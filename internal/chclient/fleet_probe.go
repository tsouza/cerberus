package chclient

import (
	"context"

	"github.com/tsouza/cerberus/internal/chopt"
)

// FleetNode is one ClickHouse server a fleet probe pass reached: its
// serverUUID(), which names one server across restarts and builds, its build,
// and the verdict of every capability canary the pass ran on it, keyed by
// FleetCanary.Name. A canary the pass did not run on the server has no entry.
type FleetNode struct {
	ID       string
	Version  chopt.Version
	Verdicts map[string]chopt.Capability
}

// FleetCanary is a capability canary the fleet probe runs on every server it
// reads through a configured address. Name keys its verdict in
// FleetNode.Verdicts and FleetVersions.Verdicts, and Probe runs it once over a
// client and returns the server's verdict.
type FleetCanary struct {
	Name  string
	Probe func(ctx context.Context, c *Client) chopt.Capability
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
// of every ClickHouse server it holds, the fleet's verdict on each capability
// canary, and whether the pass reached every node it knows of.
type FleetVersions struct {
	Versions []chopt.Version
	// Verdicts holds, per canary name, the most conservative verdict any
	// server the memory holds gave on that canary
	// (chopt.Capability.Conservative). A server no pass ran the canary on
	// contributes nothing.
	Verdicts map[string]chopt.Capability
	Complete bool
	// Held reports that Versions holds a build, or Verdicts rests on a
	// verdict, that no server the pass reached gave: a server an earlier pass
	// reached, which the memory has not forgotten, gave it. Every decision
	// read off the fleet may then rest on that server.
	Held bool
}

// Capability returns the fleet's verdict on the canary named name, or
// CapabilityUnreachable when no server the fleet holds answered it.
func (fv FleetVersions) Capability(name string) chopt.Capability {
	if v, ok := fv.Verdicts[name]; ok {
		return v
	}
	return chopt.CapabilityUnreachable
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
// Every canary runs on each address's client, and its verdict is attributed
// to a server by probeCanaries. The cluster arm reads builds only, so a
// replica read only through it carries no verdict.
//
// A failed dial, a failed read, or a failed cluster read leaves the pass
// incomplete rather than silently reporting a smaller fleet.
func ProbeFleet(ctx context.Context, cfg Config, cluster string, canaries ...FleetCanary) FleetPass {
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
		nodes, read := probeCanaries(ctx, c, node, canaries)
		pass.Nodes = append(pass.Nodes, nodes...)
		if !read {
			pass.Complete = false
		}
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

// probeCanaries runs every canary over c and returns the servers they ran on,
// each carrying the verdicts of the canaries that ran on it. first is the
// server c's connection reached last. Every canary after the first runs right
// after a fresh identity read on c, and its verdict belongs to the server that
// read named: the driver closes a connection on any error, so the statement
// after a canary the server refused dials anew, and behind a load balancer the
// new connection can reach a different server. read is false when an identity
// read failed, leaving the remaining canaries unrun.
func probeCanaries(ctx context.Context, c *Client, first FleetNode, canaries []FleetCanary) (nodes []FleetNode, read bool) {
	nodes = []FleetNode{first}
	for i, canary := range canaries {
		if i > 0 {
			node, err := c.ProbeNode(ctx)
			if err != nil {
				return nodes, false
			}
			nodes = append(nodes, node)
		}
		on := &nodes[len(nodes)-1]
		if on.Verdicts == nil {
			on.Verdicts = make(map[string]chopt.Capability, 1)
		}
		on.Verdicts[canary.Name] = canary.Probe(ctx, c)
	}
	return nodes, true
}
