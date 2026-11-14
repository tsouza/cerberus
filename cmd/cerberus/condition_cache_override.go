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
// in force. fv is the remembered fleet (rememberingFleetProber). It is
// conservative across nodes: any node of the fleet on a known-unsafe build
// keeps the override on, because a query can land on that
// node however healthy the others are. The override is lifted only when every
// known node answered from a build outside the unsafe ranges; a pass that could
// not reach every node and saw no unsafe build leaves the decision in force
// (current, as gatesInForce derives it) unchanged.
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

// refreshConditionCacheOverride installs on client the override decision fv
// implies given inForce, the decision the pass is judged against
// (gatesInForce), logging when the installed decision changes. It is
// deliberately independent of the optimization resolution: the server's own
// default engages the cache whether or not cerberus asks for it, so neither
// the selection nor a failed or unchanged resolution may stop the override
// from following the fleet.
func refreshConditionCacheOverride(logger *slog.Logger, client *chclient.Client, fv chclient.FleetVersions, inForce bool) {
	next := conditionCacheOverride(fv, inForce)
	if next == client.QueryConditionCacheDisabled() {
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

// fleetVersionStrings renders every build of the remembered fleet, for logs.
func fleetVersionStrings(fv chclient.FleetVersions) []string {
	versions := make([]string, 0, len(fv.Versions))
	for _, v := range fv.Versions {
		versions = append(versions, v.String())
	}
	return versions
}

// fleetPassProber runs one pass of the fleet probe and reports what it read.
type fleetPassProber func(ctx context.Context) chclient.FleetPass

// rememberingFleetProber is the fleetProber over probe: every pass is folded
// into one chclient.FleetMemory, and the fleet the memory then describes is
// what feature resolution, the defect gates and the analyzer decision read. A
// replica a pass through a load-balanced address did not land on therefore
// still counts in each of them until it reports another build or is
// forgotten. Boot and the re-probe loop share the returned prober, so they
// share the memory.
func rememberingFleetProber(probe fleetPassProber) fleetProber {
	memory := chclient.NewFleetMemory()
	return func(ctx context.Context) chclient.FleetVersions {
		return memory.Observe(probe(ctx))
	}
}

// liveFleetProber probes the deployment's real fleet over short-lived
// bootstrap connections: every configured ClickHouse address and — when
// CERBERUS_SCHEMA_CLUSTER names a cluster — every replica of that cluster
// (chclient.ProbeFleet). A node behind a load balancer that neither list
// names is read only when a probe lands on it, and the memory holds it
// between those passes (rememberingFleetProber).
//
// The connections bind to ClickHouse's always-present `default` database, not
// the configured one: the probe runs at boot before setupSchema creates that
// database, and ClickHouse rejects every statement — version() included — on
// a session whose default database is absent (code 81, UNKNOWN_DATABASE),
// which would mask a reachable server as a failed probe pinned to the floor.
func liveFleetProber(cfg config.Config) fleetProber {
	return rememberingFleetProber(func(ctx context.Context) chclient.FleetPass {
		return chclient.ProbeFleet(ctx, bootstrapClickHouseConfig(cfg.ClickHouse, versionProbePool), cfg.SchemaProvisioning.Cluster)
	})
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
// lowest build of the fleet the pass describes — the servers it reached and
// those earlier passes reached (rememberingFleetProber) — because a query can
// land on any of them and a feature the oldest one lacks fails there.
//
// A pass that could not reach every node keeps the posture in force: it may
// lower the version (an older build in the fleet is real evidence), but never
// raise it above the version in force, because the node it missed may be the
// one holding the version down. A floor fallback is not a posture to keep —
// it records that no node answered — so an incomplete pass lifts it. It
// returns errNoFleetVersion when the pass reached nothing.
func fleetResolutionVersion(fv chclient.FleetVersions, inForce *chOptResolution) (chopt.Version, error) {
	lowest, ok := fv.Lowest()
	if !ok {
		return chopt.Version{}, errNoFleetVersion
	}
	if !fv.Complete && inForce.describesFleet() && inForce.ResolvedVersion.Less(lowest) {
		return inForce.ResolvedVersion, nil
	}
	return lowest, nil
}

// describesFleet reports whether r is a posture a pass that could not reach
// every node keeps: a resolution in force that some node answered for. It is
// false at boot (r nil), where nothing is in force yet, and for a floor
// fallback, which records that no node answered.
func (r *chOptResolution) describesFleet() bool {
	return r != nil && !r.VersionFallback
}
