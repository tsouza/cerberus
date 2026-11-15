package chclient

import (
	"context"
	"slices"

	"github.com/tsouza/cerberus/internal/chopt"
)

// Names of the capability canaries, keying their verdicts in
// FleetNode.Verdicts and FleetVersions.Verdicts.
const (
	CanaryTSGrid        = "ts_grid"
	CanaryResultCache   = "result_cache"
	CanaryQueryLogUnion = "query_log_union"
	CanaryQueryWorkload = "query_workload"
)

// FleetCanary is a capability canary the fleet probe runs on every server it
// reads through a configured address. Name keys its verdict; build one with
// TSGridCanary, ResultCacheCanary, QueryLogUnionCanary or
// QueryWorkloadCanary.
type FleetCanary struct {
	Name  string
	probe func(ctx context.Context, c *Client) canaryAnswer
}

// canaryAnswer is what one canary run read: its verdict and, when the
// canary's own statement returned a row naming the server that answered
// (identified), that server.
type canaryAnswer struct {
	verdict    chopt.Capability
	node       FleetNode
	identified bool
}

// settingsCanary is a canary that stamps settings on capabilityCanarySQL:
// an accepted one identifies the server that accepted it.
func settingsCanary(name string, stamp func(context.Context) context.Context) FleetCanary {
	return FleetCanary{Name: name, probe: func(ctx context.Context, c *Client) canaryAnswer {
		node, verdict, ok := c.probeCanaryNode(stamp(ctx))
		return canaryAnswer{verdict: verdict, node: node, identified: ok}
	}}
}

// TSGridCanary probes the experimental ts_grid setting
// (ProbeTSGridCapability).
func TSGridCanary() FleetCanary {
	return settingsCanary(CanaryTSGrid, WithTSGridSetting)
}

// ResultCacheCanary probes the query result cache settings
// (ProbeResultCacheCapability).
func ResultCacheCanary() FleetCanary {
	return settingsCanary(CanaryResultCache, func(ctx context.Context) context.Context {
		return WithResultCacheSetting(ctx, resultCacheProbeTTLSeconds)
	})
}

// QueryWorkloadCanary probes the `workload` setting with workload
// (ProbeQueryWorkloadCapability).
func QueryWorkloadCanary(workload string) FleetCanary {
	return settingsCanary(CanaryQueryWorkload, func(ctx context.Context) context.Context {
		return WithWorkloadSetting(ctx, workloadProbeName(workload))
	})
}

// QueryLogUnionCanary probes the query-log union table
// (ProbeQueryLogUnionCapability). Its statement is the reader's own record
// selection, which returns no row naming the server, so its verdict belongs to
// the server the connection's last identity read named.
func QueryLogUnionCanary() FleetCanary {
	return FleetCanary{Name: CanaryQueryLogUnion, probe: func(ctx context.Context, c *Client) canaryAnswer {
		return canaryAnswer{verdict: c.ProbeQueryLogUnionCapability(ctx)}
	}}
}

// RotateCanaries returns canaries starting at index pass modulo their count,
// for the fleet probe's pass number pass. A refusal closes the connection, and
// the canaries after it run on whichever server the next connection reaches;
// a balancer that routes connections in a fixed rotation would otherwise keep
// a refusing server from ever running the canaries after the first. Rotating
// the order runs every canary first within as many passes as there are
// canaries.
func RotateCanaries(canaries []FleetCanary, pass int) []FleetCanary {
	if len(canaries) == 0 {
		return canaries
	}
	start := pass % len(canaries)
	return slices.Concat(canaries[start:], canaries[:start])
}
