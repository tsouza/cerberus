package chclient

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// FleetNode is one ClickHouse server a fleet probe pass reached: its
// serverUUID(), which names one server across restarts and builds, its build,
// and the verdict of every capability canary the pass attributed to it, keyed
// by FleetCanary.Name. A canary the pass did not run on the server has no entry.
type FleetNode struct {
	ID       string
	Version  chopt.Version
	Verdicts map[string]chopt.Capability
}

// FleetPass is what one pass of the fleet probe read: every server it
// reached, the canary verdicts it could not tie to a server (Unattributed),
// whether it reached every node it knows of — each configured address and,
// when a cluster is named, every replica of that cluster — and whether it
// read that cluster's replica list (Enumerated).
type FleetPass struct {
	Nodes []FleetNode
	// Unattributed holds every canary verdict a server gave that no
	// statement of the pass tied to that server: over HTTP, a refusal and a
	// query-log union answer name no server, and each is its own request,
	// which a balancer may route to a server no identity read named. Over
	// either protocol it also holds every PassOnly verdict.
	Unattributed []CanaryReading
	Complete     bool
	Enumerated   bool
}

// CanaryReading is one verdict a server gave on the canary named Canary.
// PassOnly marks a verdict that counts for the pass that read it and is not
// held after it: a server error that is neither a known refusal nor a known
// condition of the moment.
type CanaryReading struct {
	Canary   string
	Verdict  chopt.Capability
	PassOnly bool
}

// FleetVersions is the fleet a FleetMemory describes after a pass: the build
// of every ClickHouse server it holds, the fleet's verdict on each capability
// canary, and whether the pass reached every node it knows of.
type FleetVersions struct {
	Versions []chopt.Version
	// Verdicts holds, per canary name, the most conservative verdict any
	// server the memory holds gave on that canary, or any unattributed
	// verdict the memory holds on it, or any verdict the pass read for itself
	// only (a PassOnly reading, or an unreachable one from a server with no
	// verdict remembered) (chopt.Capability.Conservative). A server no pass
	// ran the canary on contributes nothing.
	Verdicts map[string]chopt.Capability
	Complete bool
	// Held reports that Versions holds a build no server the pass reached runs,
	// or that Verdicts holds a verdict no server the pass ran a canary on holds
	// on that canary, read this pass or remembered for it, and the pass did not
	// read unattributed or for itself only: a server no canary of this pass ran
	// on, which the memory has not forgotten, gave it, or it is an unattributed
	// verdict the memory still holds and this pass did not read. Every decision
	// read off the fleet may then rest on a server this pass did not see.
	Held bool
}

// Capability returns the fleet's verdict on the canary named name, or
// CapabilityUnreachable when neither a server the fleet holds nor a held
// unattributed verdict answered it.
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
// Every canary runs on each address's client, in the order order gives for
// the server it is about to run on (the given order when order is nil), and
// its verdict is attributed to a server, or left unattributed, by
// probeCanaries. The cluster arm reads builds only, so a replica read only
// through it carries no verdict.
//
// A failed dial, a failed read, or a failed cluster read leaves the pass
// incomplete rather than silently reporting a smaller fleet.
func ProbeFleet(ctx context.Context, cfg Config, cluster string, order CanaryOrder, canaries ...FleetCanary) FleetPass {
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
		nodes, unattributed, read := probeCanaries(ctx, c, node, order, canaries)
		pass.Nodes = append(pass.Nodes, nodes...)
		pass.Unattributed = append(pass.Unattributed, unattributed...)
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
// each carrying the verdicts attributed to it, and the verdicts it could tie
// to no server. first is the server c's connection reached last. Before each
// canary, order (when set) orders the canaries still pending for the server
// the next one will run on.
//
// A canary whose own statement names the server that answered (an accepted
// settings canary) is attributed to that server. Any other verdict — a
// refusal, a failed read, or an answer that names no server — names no
// server itself:
//
//   - Over the native protocol, c's statements share one connection until an
//     error closes it, so such a verdict is attributed to the server the
//     connection's last identity read named. The driver closes a connection
//     on any error, so after a canary that did not answer available, the next
//     statement dials anew, and behind a load balancer it can reach a
//     different server: the identity is read again before the next canary.
//     read is false when that read failed, leaving the remaining canaries
//     unrun.
//   - Over HTTP, every statement is its own request, and a balancer that
//     routes requests one by one can send the canary to a server other than
//     the one the identity read reached, so the verdict is returned as
//     unattributed. No identity read can tie the next canary to a server
//     either, so none is made.
//
// A PassOnly verdict counts toward the pass that read it only, so it is
// returned unattributed over either protocol: attributed to a server, it
// would stay in force until a pass reached that server again. Over the native
// protocol the server it ran on records an unreachable verdict for it, which
// keeps that server's remembered verdict (FleetMemory) and moves the canary
// order on, and the identity is read again before the next canary.
func probeCanaries(ctx context.Context, c *Client, first FleetNode, order CanaryOrder, canaries []FleetCanary) (nodes []FleetNode, unattributed []CanaryReading, read bool) {
	nodes = []FleetNode{first}
	perRequest := c.protocol == clickhouse.HTTP
	connected := true
	for pending := canaries; len(pending) > 0; pending = pending[1:] {
		if !connected {
			node, err := c.ProbeNode(ctx)
			if err != nil {
				return nodes, unattributed, false
			}
			nodes = append(nodes, node)
		}
		if order != nil {
			pending = order(nodes[len(nodes)-1].ID, pending)
		}
		canary := pending[0]
		answer := canary.probe(ctx, c)
		switch {
		case answer.identified:
			nodes = append(nodes, answer.node)
		case perRequest:
			unattributed = append(unattributed, CanaryReading{Canary: canary.Name, Verdict: answer.verdict, PassOnly: answer.passOnly})
			continue
		case answer.passOnly:
			// The verdict counts for this pass only; the server it ran on
			// records the run with no verdict, so the canary order moves on.
			unattributed = append(unattributed, CanaryReading{Canary: canary.Name, Verdict: answer.verdict, PassOnly: true})
			answer.verdict = chopt.CapabilityUnreachable
		}
		on := &nodes[len(nodes)-1]
		if on.Verdicts == nil {
			on.Verdicts = make(map[string]chopt.Capability, 1)
		}
		on.Verdicts[canary.Name] = answer.verdict
		connected = answer.verdict == chopt.CapabilityAvailable
	}
	return nodes, unattributed, true
}
