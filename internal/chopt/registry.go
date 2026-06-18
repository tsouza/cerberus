package chopt

// Feature id constants. Exported so internal/config and internal/engine
// reference the registry by symbol rather than a stringly-typed literal that
// could drift between the registry, the resolver, and the consumers.
const (
	// FeatureAggregationInOrder stamps optimize_aggregation_in_order=1 on a
	// query whose Aggregate GROUP BY is a bare-column prefix of the scanned
	// table's sorting key. Result-equivalent, 24.8-safe (the migration of the
	// dark optimize_aggregation_in_order rule into the registry).
	FeatureAggregationInOrder = "aggregation_in_order"

	// FeatureConditionCache stamps use_query_condition_cache=1 on a
	// predicate-stable read path when the server is >= 25.3. The query
	// condition cache is result-equivalent (a cache), so it ships under auto
	// for supporting servers; below 25.3 it is absent from the set (no-op).
	FeatureConditionCache = "condition_cache"

	// FeatureTSGridRange opts eligible rate(<counter>[<range>]) query_range
	// shapes onto the native timeSeriesRateToGrid aggregate. Experimental and
	// explicit-only: NEVER enabled by auto, reachable only by listing it or by
	// the legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE alias.
	FeatureTSGridRange = "ts_grid_range"
)

// Stability classifies a registry feature. Auto enables stable features only;
// experimental features require explicit listing (preserving cerberus's
// historical "experimental paths off out of the box" default).
type Stability int

const (
	// Stable features are auto-enabled when the server supports them.
	Stable Stability = iota
	// Experimental features are never auto-enabled; they require explicit
	// listing (or the legacy alias for ts_grid_range).
	Experimental
)

// Feature is one registry entry: a stable id, the minimum major.minor server
// version that supports it, its stability class, an optional ClickHouse
// allow_experimental_* setting to co-stamp on exactly the queries that use the
// feature, and a one-line operator-facing description.
type Feature struct {
	ID                  string
	MinVersion          Version
	Stability           Stability
	ExperimentalSetting string
	Doc                 string
}

// registry is the seeded feature table. It is value data (no init-time
// mutation), so Registry can hand out a defensive copy and callers cannot
// mutate the canonical entries.
var registry = []Feature{
	{
		ID:                  FeatureAggregationInOrder,
		MinVersion:          Version{Major: 24, Minor: 8},
		Stability:           Stable,
		ExperimentalSetting: "",
		Doc:                 "stamp optimize_aggregation_in_order=1 when the Aggregate GROUP BY is a sort-key prefix (result-equivalent)",
	},
	{
		ID:                  FeatureConditionCache,
		MinVersion:          Version{Major: 25, Minor: 3},
		Stability:           Stable,
		ExperimentalSetting: "",
		Doc:                 "stamp use_query_condition_cache=1 on predicate-stable read paths (result-equivalent cache, server >= 25.3)",
	},
	{
		ID:                  FeatureTSGridRange,
		MinVersion:          Version{Major: 25, Minor: 6},
		Stability:           Experimental,
		ExperimentalSetting: "allow_experimental_time_series_aggregate_functions",
		Doc:                 "opt eligible rate(<counter>[<range>]) shapes onto native timeSeriesRateToGrid (experimental, explicit-only)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range). The copy keeps the
// canonical entries immutable from the caller's side. Exposed so tests can
// enumerate the gates and the docs generator can render the table.
func Registry() []Feature {
	out := make([]Feature, len(registry))
	copy(out, registry)
	return out
}

// featureByID returns the registry entry for id, or ok=false when id is not a
// known feature (the typo-guard case the resolver turns into a fatal error).
func featureByID(id string) (Feature, bool) {
	for _, f := range registry {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}
