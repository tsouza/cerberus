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
// returns the server that accepted them, or the verdict the refusal implies
// (classifyCapabilityFromProbeErr) with ok=false.
func (c *Client) probeCanaryNode(ctx context.Context) (node FleetNode, verdict chopt.Capability, ok bool) {
	nodes, err := c.probeNodes(withCapabilityCanary(ctx), capabilityCanarySQL)
	if err != nil {
		return FleetNode{}, classifyCapabilityFromProbeErr(err), false
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
//   - typed server rejection          -> CapabilityForbidden (constrained /
//     readonly profile refused the setting; SETTING_CONSTRAINT_VIOLATION 452 or
//     READONLY 164 are the canonical codes, but any typed answer to `SELECT 1`
//     means the server refused the stamped setting)
//   - transport / connectivity failure -> CapabilityUnreachable (conservative:
//     do NOT enable native, matching the version probe's connectivity fallback)
//
// It never returns an error: a probe failure is a verdict (Forbidden /
// Unreachable), not a fatal. The resolver decides what an inconclusive verdict
// means per selection mode (auto silently falls back, an explicit list errors
// under enforcing).
func (c *Client) ProbeTSGridCapability(ctx context.Context) chopt.Capability {
	return TSGridCanary().probe(ctx, c).verdict
}

// classifyCapabilityFromProbeErr maps ANY boot capability canary's error into
// the tri-state chopt.Capability verdict shared by every probe in this
// package (probeCanaryNode, ProbeQueryLogUnionCapability): a nil error is
// Available. A typed *clickhouse.Exception means the server ANSWERED with a
// rejection -- since every settings canary body is a trivial constant SELECT
// (capabilityCanarySQL) that cannot
// fail on its own, a server-side exception means it refused the stamped
// setting, so it is Forbidden (the ts-grid canary's constraint 452 / readonly
// 164 codes are the expected shapes, and any other typed answer is treated
// the same: the server will not run the setting). Anything else is a
// transport failure with no server verdict -- Unreachable, the conservative
// state that leaves the gated feature off until a later re-probe gets a
// verdict.
func classifyCapabilityFromProbeErr(err error) chopt.Capability {
	if err == nil {
		return chopt.CapabilityAvailable
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return chopt.CapabilityForbidden
	}
	return chopt.CapabilityUnreachable
}
