package chclient

import (
	"context"

	"github.com/tsouza/cerberus/internal/chopt"
)

// workloadCapabilityProbeName is the WORKLOAD name the boot canary stamps
// when the operator has NOT configured CERBERUS_CH_QUERY_WORKLOAD (cmd/
// cerberus only calls this probe when a name IS configured, so in practice
// the real configured name always rides the canary — see
// ProbeQueryWorkloadCapability's own doc for why probing the REAL name,
// not a sentinel, is deliberate). This constant exists only as the probe's
// own safety net so the function never sends an empty workload name.
const workloadCapabilityProbeName = "cerberus_probe"

// ProbeQueryWorkloadCapability runs the boot-time capability canary once and
// classifies whether the connected ClickHouse server accepts the `workload`
// per-query setting cerberus stamps on every outgoing query when
// CERBERUS_CH_QUERY_WORKLOAD is configured. It probes the OPERATOR'S OWN
// configured workload name (not a fixed sentinel): unlike the ts-grid /
// result-cache canaries, which only need to learn whether a SETTING NAME is
// accepted, a hardened deployment can additionally set
// `throw_on_unknown_workload=true` server-side to require every workload
// name a query carries to resolve to a real, provisioned WORKLOAD — probing
// the real name catches that misconfiguration too, at boot rather than on
// the first real dashboard query.
//
// Mirrors ProbeTSGridCapability / ProbeResultCacheCapability exactly: the
// same tri-state classification (canaryErrAnswer) over the
// same trivial canary body (capabilityCanarySQL), and the same "never returns
// an error" contract.
// It is the query_workload canary (QueryWorkloadCanary) run on its own;
// cmd/cerberus runs QueryWorkloadCanary through ProbeFleet, ONLY when
// CERBERUS_CH_QUERY_WORKLOAD is non-empty (an unconfigured deployment skips
// the probe entirely — the knob's mere absence is already the full gate),
// and combines the verdict with CERBERUS_CH_OPTIMIZATIONS_MODE to decide
// FATAL (enforcing) vs WARN-and-skip (permissive) on a Forbidden/Unreachable
// verdict.
func (c *Client) ProbeQueryWorkloadCapability(ctx context.Context, workloadName string) chopt.Capability {
	return QueryWorkloadCanary(workloadName).probe(ctx, c).verdict
}

// workloadProbeName returns configured unchanged when non-empty, else the
// probe sentinel. Split out from ProbeQueryWorkloadCapability so the
// fallback is unit-testable without a live ClickHouse connection.
func workloadProbeName(configured string) string {
	if configured == "" {
		return workloadCapabilityProbeName
	}
	return configured
}
