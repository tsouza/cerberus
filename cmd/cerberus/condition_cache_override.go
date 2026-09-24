package main

import (
	"context"
	"log/slog"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// fleetVersions is what one pass of the fleet version probe saw: the build of
// every ClickHouse node it reached, and whether it reached every node it knows
// of — each configured address and, when a cluster is configured, every
// replica of that cluster.
type fleetVersions struct {
	versions []chopt.Version
	complete bool
}

// fleetProber runs one pass of the fleet version probe.
type fleetProber func(ctx context.Context) fleetVersions

// conditionCacheOverride decides whether the client must force
// use_query_condition_cache=0, given what the fleet probe saw and the decision
// in force. It is conservative across nodes: any reached node on a
// known-unsafe build keeps the override on, because a query can land on that
// node however healthy the others are. The override is lifted only when every
// known node answered from a build outside the unsafe ranges; a pass that could
// not reach every node and saw no unsafe build leaves the decision unchanged.
func conditionCacheOverride(fv fleetVersions, current bool) bool {
	for _, v := range fv.versions {
		if chopt.KnownUnsafe(chopt.FeatureConditionCache, v) {
			return true
		}
	}
	if !fv.complete {
		return current
	}
	return false
}

// refreshConditionCacheOverride runs probe once and installs the resulting
// override decision on client, logging when the decision changes. It is
// deliberately independent of the optimization resolution: the server's own
// default engages the cache whether or not cerberus asks for it, so neither
// the selection nor a failed or unchanged resolution may stop the override
// from following the fleet.
func refreshConditionCacheOverride(ctx context.Context, logger *slog.Logger, client *chclient.Client, probe fleetProber) {
	fv := probe(ctx)
	current := client.QueryConditionCacheDisabled()
	next := conditionCacheOverride(fv, current)
	if next == current {
		return
	}
	client.SetQueryConditionCacheDisabled(next)
	versions := make([]string, 0, len(fv.versions))
	for _, v := range fv.versions {
		versions = append(versions, v.String())
	}
	logger.Warn(
		"query condition cache override changed",
		"query_condition_cache_forced_off", next,
		"fleet_versions", versions,
		"fleet_probe_complete", fv.complete,
	)
}

// liveFleetProber probes the deployment's real fleet: every configured
// ClickHouse address, one short-lived bootstrap connection each, and — when
// CERBERUS_SCHEMA_CLUSTER names a cluster — every replica of that cluster
// through the first address that answers. A node behind a load balancer that
// neither list names cannot be reached individually; its build is seen only
// when a probe happens to land on it (docs/clickhouse-optimizations.md).
func liveFleetProber(cfg config.Config) fleetProber {
	return func(ctx context.Context) fleetVersions {
		hosts := cfg.ClickHouse.Addrs
		if len(hosts) == 0 {
			hosts = []string{cfg.ClickHouse.Addr}
		}
		fv := fleetVersions{complete: true}
		var reached *chclient.Client
		for _, host := range hosts {
			hostCfg := bootstrapClickHouseConfig(cfg.ClickHouse, versionProbePool)
			hostCfg.Addrs = nil
			hostCfg.Addr = host
			c, err := chclient.New(hostCfg)
			if err != nil {
				fv.complete = false
				continue
			}
			v, err := c.ProbeVersion(ctx)
			if err != nil {
				fv.complete = false
				_ = c.Close()
				continue
			}
			fv.versions = append(fv.versions, v)
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
		if cfg.SchemaProvisioning.Cluster != "" {
			versions, err := reached.ProbeClusterVersions(ctx, cfg.SchemaProvisioning.Cluster)
			if err != nil {
				fv.complete = false
			} else {
				fv.versions = append(fv.versions, versions...)
			}
		}
		return fv
	}
}

// logVendorBuild warns when the server reports a non-upstream version string:
// its patch and build numbers cannot be compared against upstream fix builds,
// so the known-defective-build gates and the cancellation-gap policy judge it
// by its major.minor line alone, conservatively (chopt.Version.Vendor).
func logVendorBuild(logger *slog.Logger, server chopt.Version) {
	if !server.Vendor {
		return
	}
	logger.Warn(
		"clickhouse reports a non-upstream build version; patch-level defect gates treat it by its release line, "+
			"so fixes a vendor backported are not credited",
		"server_version", server.String(),
	)
}
