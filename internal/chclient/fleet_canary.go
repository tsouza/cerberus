package chclient

import (
	"context"

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

// canaryAnswer is what one canary run read: its verdict, whether that verdict
// holds for this pass only (passOnly, canaryErrAnswer) and, when the canary's
// own statement returned a row naming the server that answered (identified),
// that server.
type canaryAnswer struct {
	verdict    chopt.Capability
	passOnly   bool
	node       FleetNode
	identified bool
}

// settingsCanary is a canary that stamps settings on capabilityCanarySQL:
// an accepted one identifies the server that accepted it.
func settingsCanary(name string, stamp func(context.Context) context.Context) FleetCanary {
	return FleetCanary{Name: name, probe: func(ctx context.Context, c *Client) canaryAnswer {
		return c.probeCanaryNode(stamp(ctx))
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
// selection, which returns no row naming the server, so over the native
// protocol its verdict belongs to the server the connection's last identity
// read named, and over HTTP it belongs to no named server (probeCanaries).
func QueryLogUnionCanary() FleetCanary {
	return FleetCanary{Name: CanaryQueryLogUnion, probe: func(ctx context.Context, c *Client) canaryAnswer {
		return c.queryLogUnionAnswer(ctx)
	}}
}

// CanaryOrder orders the canaries still pending in a pass for the server
// named server, the one the next canary will run on. FleetMemory.CanaryOrder
// is the one cerberus uses.
type CanaryOrder func(server string, pending []FleetCanary) []FleetCanary
