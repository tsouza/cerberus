package chclient

import (
	"context"

	"github.com/tsouza/cerberus/internal/chopt"
)

// FleetVersions is what one pass of the fleet version probe saw: the build of
// every ClickHouse node it reached, and whether it reached every node it knows
// of — each configured address and, when a cluster is named, every replica of
// that cluster.
type FleetVersions struct {
	Versions []chopt.Version
	Complete bool
}

// Lowest returns the oldest build the pass reached, or ok=false when it
// reached none.
func (fv FleetVersions) Lowest() (chopt.Version, bool) {
	return chopt.LowestVersion(fv.Versions)
}

// ProbeFleetVersions reads the build of every node cfg can reach: each address
// in cfg.Addrs (or cfg.Addr alone), over one short-lived connection per
// address, and — when cluster is non-empty — every replica of that cluster
// through clusterAllReplicas on the first address that answers. A node behind
// a load balancer that neither list names cannot be reached individually; its
// build is seen only when a connection happens to land on it.
//
// A failed dial, a failed version read, or a failed cluster read leaves the
// pass incomplete rather than silently reporting a smaller fleet.
func ProbeFleetVersions(ctx context.Context, cfg Config, cluster string) FleetVersions {
	hosts := cfg.Addrs
	if len(hosts) == 0 {
		hosts = []string{cfg.Addr}
	}
	fv := FleetVersions{Complete: true}
	var reached *Client
	for _, host := range hosts {
		hostCfg := cfg
		hostCfg.Addrs = nil
		hostCfg.Addr = host
		c, err := New(hostCfg)
		if err != nil {
			fv.Complete = false
			continue
		}
		v, err := c.ProbeVersion(ctx)
		if err != nil {
			fv.Complete = false
			_ = c.Close()
			continue
		}
		fv.Versions = append(fv.Versions, v)
		if reached == nil {
			reached = c
			continue
		}
		_ = c.Close()
	}
	if reached == nil {
		return fv
	}
	defer func() { _ = reached.Close() }()
	if cluster != "" {
		versions, err := reached.ProbeClusterVersions(ctx, cluster)
		if err != nil {
			fv.Complete = false
		} else {
			fv.Versions = append(fv.Versions, versions...)
		}
	}
	return fv
}
