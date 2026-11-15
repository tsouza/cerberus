package main

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// Names of the capability canaries the fleet probe runs, keying their
// verdicts in chclient.FleetVersions.
const (
	tsGridCanary        = "ts_grid"
	resultCacheCanary   = "result_cache"
	queryLogUnionCanary = "query_log_union"
	queryWorkloadCanary = "query_workload"
)

// fleetCanaries is the capability canaries the fleet probe runs on every
// server it reads through a configured address, given the configured
// selection and the operator-configured CERBERUS_CH_QUERY_WORKLOAD:
//
//   - the experimental ts_grid setting (chclient.ProbeTSGridCapability) and
//     the query result cache settings (chclient.ProbeResultCacheCapability),
//     always;
//   - the query-log union table (chclient.ProbeQueryLogUnionCapability) only
//     when selection names query_log_union explicitly: the feature is never
//     auto-selected, so an unrequested verdict decides nothing, and the canary
//     reaches every member of a cluster-wide union table;
//   - the `workload` setting with workload itself
//     (chclient.ProbeQueryWorkloadCapability) only when workload is set.
//
// Running them inside the fleet probe ties each verdict to the server that
// gave it, so the fleet memory keeps every remembered server's verdict and
// the fleet's verdict is the most conservative across them
// (chclient.FleetVersions.Capability).
func fleetCanaries(selection, workload string) []chclient.FleetCanary {
	canaries := []chclient.FleetCanary{
		{Name: tsGridCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
			return c.ProbeTSGridCapability(ctx)
		}},
		{Name: resultCacheCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
			return c.ProbeResultCacheCapability(ctx)
		}},
	}
	if chopt.ExplicitlyRequested(selection, chopt.FeatureQueryLogUnion) {
		canaries = append(canaries, chclient.FleetCanary{Name: queryLogUnionCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
			return c.ProbeQueryLogUnionCapability(ctx)
		}})
	}
	if workload != "" {
		canaries = append(canaries, chclient.FleetCanary{Name: queryWorkloadCanary, Probe: func(ctx context.Context, c *chclient.Client) chopt.Capability {
			return c.ProbeQueryWorkloadCapability(ctx, workload)
		}})
	}
	return canaries
}

// fleetResolveConfig is the resolver configuration for cfg's selection
// against fleet: the configured selection, mode and legacy flag, and the
// fleet's verdict on each capability canary. The query-log union verdict is
// CapabilityUnknown when the selection does not name the feature, because
// fleetCanaries does not run that canary then.
func fleetResolveConfig(cfg config.Config, fleet chclient.FleetVersions) chopt.Config {
	queryLogUnion := chopt.CapabilityUnknown
	if chopt.ExplicitlyRequested(cfg.CHOptimizations, chopt.FeatureQueryLogUnion) {
		queryLogUnion = fleet.Capability(queryLogUnionCanary)
	}
	return chopt.Config{
		Optimizations:           cfg.CHOptimizations,
		Mode:                    cfg.CHOptimizationsMode,
		LegacyTSGrid:            cfg.LegacyTSGridFlag,
		Capability:              fleet.Capability(tsGridCanary),
		ResultCacheCapability:   fleet.Capability(resultCacheCanary),
		QueryLogUnionCapability: queryLogUnion,
	}
}
