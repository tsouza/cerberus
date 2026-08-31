package chopt

// Capability is the boot-time, tri-state verdict on whether the connected
// ClickHouse server will actually accept a particular setting (or setting
// family) cerberus wants to stamp. It is a REUSABLE shape: two independent
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
//     (result_cache).
//
// Both exist because a server can be new enough — even, for result_cache,
// simply RUNNING at all, since that setting family predates cerberus's own
// version floor — yet still REFUSE the specific setting (a hardened profile
// that pins/constrains it, a readonly user, or the underlying cache disabled
// server-side), in which case auto-selecting the feature would only earn a
// SETTING_CONSTRAINT_VIOLATION / READONLY rejection at query time.
//
// The resolver treats "capability not Available" EXACTLY like "version too
// old" for a feature that requires it: under auto it is a silent skip + a
// boot WARN, under an explicit list it is FATAL (enforcing) or WARN+skip
// (permissive) — see PermitsExperimentalTSGrid / PermitsResultCache for the
// per-axis "does this verdict allow the feature" reading.
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
	// CapabilityForbidden means the server ANSWERED the canary with a typed
	// rejection of the experimental setting — a constrained profile
	// (SETTING_CONSTRAINT_VIOLATION) or a readonly user (READONLY). The server is
	// reachable and may be new enough, but it will not run the native node, so
	// cerberus falls back to the fan-out path.
	CapabilityForbidden
	// CapabilityUnreachable means the canary could not get a verdict from the
	// server at all (a dial / timeout / breaker-open transport failure, not a
	// typed answer). Treated conservatively like Forbidden — the native family
	// stays off until a re-probe reaches a server that answers, matching the
	// version probe's connectivity fallback.
	CapabilityUnreachable
)

// PermitsExperimentalTSGrid reports whether the server verdict allows the native
// timeSeries*ToGrid family. Only CapabilityAvailable does; every other state
// (Unknown / Forbidden / Unreachable) is conservative.
func (c Capability) PermitsExperimentalTSGrid() bool {
	return c == CapabilityAvailable
}

// PermitsResultCache reports whether the server verdict allows the
// result_cache feature to stamp use_query_cache / query_cache_ttl. Only
// CapabilityAvailable does; every other state (Unknown / Forbidden /
// Unreachable) is conservative — see the type doc for why a version-new-
// enough server can still fail this probe.
func (c Capability) PermitsResultCache() bool {
	return c == CapabilityAvailable
}

// Inconclusive reports whether the canary failed to reach a DEFINITIVE verdict:
// Unreachable (a dial / timeout / breaker-open transport failure) or Unknown
// (the probe never ran). It is the opposite axis from a definitive answer --
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
