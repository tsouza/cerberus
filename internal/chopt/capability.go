package chopt

import "slices"

// Capability is the boot-time, tri-state verdict on whether the connected
// ClickHouse server will actually accept a particular setting (or setting
// family) cerberus wants to use. It is a REUSABLE shape: three independent
// registry axes each thread their OWN Capability value through Resolve on
// their own Config field —
//
//   - the native timeSeries*ToGrid aggregate family's experimental gate
//     (`allow_experimental_time_series_aggregate_functions`), probed by
//     chclient.ProbeTSGridCapability and carried on Config.Capability, gating
//     every Feature.RequiresExperimentalTSGrid entry;
//   - the query result cache (`use_query_cache` / `query_cache_ttl`), probed
//     by chclient.ProbeResultCacheCapability and carried on
//     Config.ResultCacheCapability, gating Feature.RequiresResultCacheCapability
//     (result_cache);
//   - the query-log union table (`system.all_query_log`), probed by
//     chclient.ProbeQueryLogUnionCapability and carried on
//     Config.QueryLogUnionCapability, gating
//     Feature.RequiresQueryLogUnionCapability (query_log_union).
//
// All three exist because a server can be new enough — even, for
// result_cache, simply RUNNING at all, since that setting family predates
// cerberus's own version floor — yet still REFUSE the specific setting or
// table (a hardened profile that pins/constrains it, a readonly user, the
// underlying cache disabled server-side, or a union table the operator never
// configured or granted), in which case using the feature would only earn a
// typed rejection at query time.
//
// The resolver treats "capability not Available" EXACTLY like "version too
// old" for a feature that requires it: under auto it is a silent skip + a
// boot WARN, under an explicit list it is FATAL (enforcing) or WARN+skip
// (permissive) — except on the query-log union axis, whose block always
// degrades with a WARN (Feature.RequiresQueryLogUnionCapability). Only
// CapabilityAvailable permits a gated feature on any axis; every other state
// (Unknown / Forbidden / Unreachable) is conservative.
type Capability int

const (
	// CapabilityUnknown is the zero value: the boot canary has not run, or its
	// result was not threaded in. It is treated CONSERVATIVELY — identical to
	// Forbidden/Unreachable, i.e. the native ts_grid features are NOT enabled —
	// so a caller that forgets to probe can never silently re-enable the
	// experimental path against a server that may forbid the setting. Only an
	// explicit CapabilityAvailable unlocks the native family.
	CapabilityUnknown Capability = iota
	// CapabilityAvailable means the canary stamped the experimental setting on a
	// trivial probe query and the server accepted it: the native aggregates may
	// be auto-selected (subject to their version floor).
	CapabilityAvailable
	// CapabilityForbidden means the server ANSWERED the canary with an error:
	// a refusal of what the canary carries — a constrained profile
	// (SETTING_CONSTRAINT_VIOLATION), a readonly user (READONLY), a build
	// without the setting (UNKNOWN_SETTING) — or another server error that is
	// not a condition of the moment. The server is reachable and may be new
	// enough, but it will not run the gated feature, so cerberus falls back.
	CapabilityForbidden
	// CapabilityUnreachable means the canary got no verdict from the server:
	// a dial / breaker-open transport failure, or a server error reporting a
	// condition of the moment (a timeout, a memory or concurrency limit).
	// Treated conservatively like Forbidden — the native family
	// stays off until a re-probe reaches a server that answers, matching the
	// version probe's connectivity fallback — except where the fleet memory
	// (chclient.FleetMemory) keeps the verdict it remembers for the server.
	CapabilityUnreachable
)

// Inconclusive reports whether the canary failed to reach a DEFINITIVE verdict:
// Unreachable (a transport failure, or a server error reporting a condition
// of the moment) or Unknown (the probe never ran). It is the opposite axis from a definitive answer --
// Available definitively permits, Forbidden definitively refuses.
//
// The resolver uses it to mirror the version probe's connectivity fallback: an
// inconclusive verdict degrades the native family to fan-out with a WARN and is
// NEVER fatal, even for an explicitly-requested feature under enforcing mode. A
// probe that could not get an answer must not take a deployment down -- only a
// definitive Forbidden (the server reachably refused the setting) keeps the
// enforcing "I require this" contract fatal.
func (c Capability) Inconclusive() bool {
	return c == CapabilityUnreachable || c == CapabilityUnknown
}

// String renders the capability for boot logging.
func (c Capability) String() string {
	switch c {
	case CapabilityAvailable:
		return "available"
	case CapabilityForbidden:
		return "forbidden"
	case CapabilityUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// Conservative returns whichever of c and other permits less, for combining
// the verdicts several servers gave on one canary: a query can land on any of
// them, so the fleet permits a setting only when every server that answered
// permits it. Forbidden, a server's definitive refusal, outranks Unreachable,
// and Unreachable outranks Available. Unknown is no verdict at all, so it
// yields the other value.
func (c Capability) Conservative(other Capability) Capability {
	if slices.Index(permissiveFirst, other) > slices.Index(permissiveFirst, c) {
		return other
	}
	return c
}

// permissiveFirst orders the verdicts for Conservative, from no verdict to the
// definitive refusal.
var permissiveFirst = []Capability{CapabilityUnknown, CapabilityAvailable, CapabilityUnreachable, CapabilityForbidden}
