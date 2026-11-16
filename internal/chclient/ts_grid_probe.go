package chclient

import (
	"context"
	"slices"

	chproto "github.com/ClickHouse/ch-go/proto"

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
// answers with the server that accepted them, or with what its error implies
// (canaryErrAnswer over isSettingsRefusal).
func (c *Client) probeCanaryNode(ctx context.Context) canaryAnswer {
	nodes, err := c.probeNodes(withCapabilityCanary(ctx), capabilityCanarySQL)
	if err != nil {
		return canaryErrAnswer(err, isSettingsRefusal)
	}
	return canaryAnswer{verdict: chopt.CapabilityAvailable, node: nodes[0], identified: true}
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
	// WORKLOAD. Read off a live 26.6 server reloaded with
	// throw_on_unknown_workload=true, `SELECT 1 SETTINGS
	// workload='nonexistent'` answers `Code: 673 (RESOURCE_ACCESS_DENIED):
	// Could not access resource 'cpu'. Please check throw_on_unknown_workload
	// setting.`
	chCodeResourceAccessDenied = 673
	// chCodeQueryCacheUsedWithNonThrowOverflowMode is
	// QUERY_CACHE_USED_WITH_NON_THROW_OVERFLOW_MODE: the query cache refuses
	// a statement whose profile sets an overflow mode other than throw
	// (result_overflow_mode='break', for one).
	chCodeQueryCacheUsedWithNonThrowOverflowMode = 731
)

// isSettingsRefusal reports whether err is the server refusing a setting the
// statement carries: a setting the build does not know, a readonly profile, a
// settings constraint, a workload the server cannot resolve, or the query
// cache under a non-throw overflow mode.
func isSettingsRefusal(err error) bool {
	return exceptionCodeIn(err, chCodeUnknownSetting, chCodeReadonly, chCodeSettingConstraintViolation,
		chCodeResourceAccessDenied, chCodeQueryCacheUsedWithNonThrowOverflowMode)
}

// exceptionCodeIn reports whether err is a server exception whose code is one
// of codes.
func exceptionCodeIn(err error, codes ...chproto.Error) bool {
	code, answered := serverExceptionCode(err)
	return answered && slices.Contains(codes, code)
}

// canaryConditionCodes are the server exceptions a canary's statement can
// fail with that report a resource or load condition of the moment rather
// than whether the server accepts the capability: a per-query limit, a
// cancellation, a quota. With the server conditions the breaker counts
// (breakerServerHealthCodes), they are what canaryErrAnswer reads as no
// verdict.
var canaryConditionCodes = []chproto.Error{
	chproto.ErrTimeoutExceeded,
	chproto.ErrMemoryLimitExceeded,
	chproto.ErrQueryWasCancelled,
	chproto.ErrQuotaExceeded,
}

// isCanaryCondition reports whether err says nothing about whether the
// server accepts a capability: no server answer at all, or a server exception
// that reports a condition of the moment (canaryConditionCodes,
// breakerServerHealthCodes).
func isCanaryCondition(err error) bool {
	code, answered := serverExceptionCode(err)
	if !answered {
		return true
	}
	_, serverCondition := breakerServerHealthCodes[code]
	return serverCondition || slices.Contains(canaryConditionCodes, code)
}

// canaryErrAnswer is the answer of a canary whose statement failed with err;
// a nil err is Available. refused names the codes the server answers with
// for refusing the capability:
//
//   - A refusal is Forbidden.
//   - A transport failure, or a server exception reporting a condition of the
//     moment (isCanaryCondition), is Unreachable: it keeps the verdict the
//     fleet memory remembers for the server and, when it remembers none,
//     leaves the gated feature off until a later re-probe gets a verdict.
//   - Any other server exception is Forbidden for the pass that read it only
//     (passOnly): the server answered the canary with an error, so the
//     capability stays off, but an error that is neither a known refusal nor
//     a known condition is not held as one.
func canaryErrAnswer(err error, refused func(error) bool) canaryAnswer {
	switch {
	case err == nil:
		return canaryAnswer{verdict: chopt.CapabilityAvailable}
	case refused(err):
		return canaryAnswer{verdict: chopt.CapabilityForbidden}
	case isCanaryCondition(err):
		return canaryAnswer{verdict: chopt.CapabilityUnreachable}
	default:
		return canaryAnswer{verdict: chopt.CapabilityForbidden, passOnly: true}
	}
}
