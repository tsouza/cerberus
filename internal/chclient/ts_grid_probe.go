package chclient

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// capabilityCanarySQL is the body of every settings canary (ts_grid,
// result_cache, workload): the identity read itself (nodeSQL), with the
// canary's settings stamped on it. It is deliberately a trivial constant
// SELECT and NOT a call to a native timeSeries*ToGrid aggregate: the canary
// only needs to learn whether the server will ACCEPT the setting cerberus
// stamps, and that verdict is decided when ClickHouse applies the per-query
// settings map (constraint / readonly enforcement), BEFORE the query body is
// analysed. Stamping the setting over a trivial SELECT therefore isolates
// exactly the FORBIDDEN signal with zero dependence on getting an aggregate's
// argument types right -- a type slip in a real-aggregate canary would
// misclassify a permitting server as not-capable and silently lose the
// optimization. The setting itself is added via the SAME production carrier
// the engine uses (WithTSGridSetting and its siblings), so the probe rides the
// identical settings-map path a real query does.
//
// Reading the identity in the canary statement means an accepted canary names
// the server that accepted it in its own row (probeNodes), however a load
// balancer routes the statement. Both projected values are String columns,
// which the strict scanner decodes; a numeric projection scanned into a Go
// string fails CLIENT-SIDE (`converting UInt8 to *string is unsupported`),
// which is not a *clickhouse.Exception and would read a capable server as
// CapabilityUnreachable.
const capabilityCanarySQL = nodeSQL

// probeCanaryNode runs capabilityCanarySQL under ctx's canary settings and
// returns the server that accepted them, or the verdict its error implies
// (classifyCapabilityFromProbeErr over isSettingsRefusal) with ok=false.
func (c *Client) probeCanaryNode(ctx context.Context) (node FleetNode, verdict chopt.Capability, ok bool) {
	nodes, err := c.probeNodes(withCapabilityCanary(ctx), capabilityCanarySQL)
	if err != nil {
		return FleetNode{}, classifyCapabilityFromProbeErr(err, isSettingsRefusal), false
	}
	return nodes[0], chopt.CapabilityAvailable, true
}

// ProbeTSGridCapability runs the boot-time capability canary once and classifies
// whether the connected ClickHouse server will run the experimental
// timeSeries*ToGrid aggregate family: the ts_grid canary (TSGridCanary) run
// on its own. cmd/cerberus runs TSGridCanary through ProbeFleet on every
// server the fleet probe reads over its bootstrap/default-DB connection, then
// hands the fleet's verdict to chopt.Resolve as Config.Capability.
//
// The probe stamps WithTSGridSetting's settings —
// allow_experimental_time_series_aggregate_functions=1 and the
// parallel-replicas pin — on a trivial query, so a profile that refuses
// either one keeps the native family off. The server's response is mapped to a
// tri-state:
//
//   - success                         -> CapabilityAvailable
//   - the server refusing the setting  -> CapabilityForbidden (a constrained
//     or readonly profile, or a build that does not know the setting:
//     isSettingsRefusal)
//   - any other failure                -> CapabilityUnreachable (a transport
//     failure, or a server exception that is not a refusal, such as a
//     timeout or a memory limit; conservative: do NOT enable native, matching
//     the version probe's connectivity fallback)
//
// It never returns an error: a probe failure is a verdict (Forbidden /
// Unreachable), not a fatal. The resolver decides what an inconclusive verdict
// means per selection mode (auto silently falls back, an explicit list errors
// under enforcing).
func (c *Client) ProbeTSGridCapability(ctx context.Context) chopt.Capability {
	return TSGridCanary().probe(ctx, c).verdict
}

// Server error codes that, with chCodeUnknownSetting and
// chCodeSettingConstraintViolation, mean the server refuses a setting a
// statement carries.
const (
	// chCodeReadonly is READONLY: the user's profile sets readonly=1, which
	// refuses every setting a statement carries.
	chCodeReadonly = 164
	// chCodeResourceAccessDenied is RESOURCE_ACCESS_DENIED: under the server
	// setting throw_on_unknown_workload, a `workload` that names no defined
	// WORKLOAD.
	chCodeResourceAccessDenied = 673
)

// isSettingsRefusal reports whether err is the server refusing a setting the
// statement carries: a setting the build does not know, a readonly profile, a
// settings constraint, or a workload the server cannot resolve.
func isSettingsRefusal(err error) bool {
	var ex *clickhouse.Exception
	if !errors.As(err, &ex) {
		return false
	}
	switch ex.Code {
	case chCodeUnknownSetting, chCodeReadonly, chCodeSettingConstraintViolation, chCodeResourceAccessDenied:
		return true
	default:
		return false
	}
}

// classifyCapabilityFromProbeErr maps a capability canary's error into the
// tri-state chopt.Capability verdict: a nil error is Available, an error
// refused reports as the server refusing the capability is Forbidden, and
// anything else is Unreachable — a transport failure, or a server exception
// that says nothing about the capability, such as a timeout or a memory or
// concurrency limit. Unreachable keeps the verdict the fleet memory
// remembers for the server and, when it remembers none, leaves the gated
// feature off until a later re-probe gets a verdict.
func classifyCapabilityFromProbeErr(err error, refused func(error) bool) chopt.Capability {
	switch {
	case err == nil:
		return chopt.CapabilityAvailable
	case refused(err):
		return chopt.CapabilityForbidden
	default:
		return chopt.CapabilityUnreachable
	}
}
