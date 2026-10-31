package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// fleetProber runs one pass of the fleet version probe.
type fleetProber func(ctx context.Context) chclient.FleetVersions

// conditionCacheOverride decides whether the client must force
// use_query_condition_cache=0, given what the fleet probe saw and the decision
// in force. It is conservative across nodes: any reached node on a
// known-unsafe build keeps the override on, because a query can land on that
// node however healthy the others are. The override is lifted only when every
// known node answered from a build outside the unsafe ranges; a pass that could
// not reach every node and saw no unsafe build leaves the decision unchanged.
func conditionCacheOverride(fv chclient.FleetVersions, current bool) bool {
	for _, v := range fv.Versions {
		if chopt.KnownUnsafe(chopt.FeatureConditionCache, v) {
			return true
		}
	}
	if !fv.Complete {
		return current
	}
	return false
}

// refreshConditionCacheOverride installs the override decision fv implies on
// client, logging when the decision changes. It is deliberately independent
// of the optimization resolution: the server's own default engages the cache
// whether or not cerberus asks for it, so neither the selection nor a failed
// or unchanged resolution may stop the override from following the fleet.
func refreshConditionCacheOverride(logger *slog.Logger, client *chclient.Client, fv chclient.FleetVersions) {
	current := client.QueryConditionCacheDisabled()
	next := conditionCacheOverride(fv, current)
	if next == current {
		return
	}
	client.SetQueryConditionCacheDisabled(next)
	logger.Warn(
		"query condition cache override changed",
		"query_condition_cache_forced_off", next,
		"fleet_versions", fleetVersionStrings(fv),
		"fleet_probe_complete", fv.Complete,
	)
}

// fleetVersionStrings renders every build a fleet probe pass reached, for logs.
func fleetVersionStrings(fv chclient.FleetVersions) []string {
	versions := make([]string, 0, len(fv.Versions))
	for _, v := range fv.Versions {
		versions = append(versions, v.String())
	}
	return versions
}

// liveFleetProber probes the deployment's real fleet over short-lived
// bootstrap connections: every configured ClickHouse address and — when
// CERBERUS_SCHEMA_CLUSTER names a cluster — every replica of that cluster
// (chclient.ProbeFleetVersions). A node behind a load balancer that neither
// list names cannot be reached individually; its build is seen only when a
// probe happens to land on it (docs/clickhouse-optimizations.md).
//
// The connections bind to ClickHouse's always-present `default` database, not
// the configured one: the probe runs at boot before setupSchema creates that
// database, and ClickHouse rejects every statement — version() included — on
// a session whose default database is absent (code 81, UNKNOWN_DATABASE),
// which would mask a reachable server as a failed probe pinned to the floor.
func liveFleetProber(cfg config.Config) fleetProber {
	return func(ctx context.Context) chclient.FleetVersions {
		return chclient.ProbeFleetVersions(ctx, bootstrapClickHouseConfig(cfg.ClickHouse, versionProbePool), cfg.SchemaProvisioning.Cluster)
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

// errNoFleetVersion is fleetResolutionVersion's answer when a fleet probe
// pass reached no node at all.
var errNoFleetVersion = errors.New("no clickhouse node answered the version probe")

// fleetResolutionVersion is the build feature resolution runs against, given
// one fleet probe pass and the resolution in force (nil at boot). It is the
// lowest build the pass reached, because a query can land on any reachable
// node and a feature the oldest one lacks fails there.
//
// A pass that could not reach every node keeps the posture in force: it may
// lower the version (an older build it did reach is real evidence), but never
// raise it above the version in force, because the node it missed may be the
// one holding the version down. A floor fallback is not a posture to keep —
// it records that no node answered — so an incomplete pass lifts it. It
// returns errNoFleetVersion when the pass reached nothing.
func fleetResolutionVersion(fv chclient.FleetVersions, inForce *chOptResolution) (chopt.Version, error) {
	lowest, ok := fv.Lowest()
	if !ok {
		return chopt.Version{}, errNoFleetVersion
	}
	if !fv.Complete && inForce != nil && !inForce.VersionFallback && inForce.ResolvedVersion.Less(lowest) {
		return inForce.ResolvedVersion, nil
	}
	return lowest, nil
}
