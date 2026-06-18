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

	// FeatureTSGridResample opts the eligible range-mode instant-vector
	// selection / staleness shape (the query_range bare-selector LWR) onto the
	// native timeSeriesResampleToGridWithStaleness aggregate, retiring the
	// argMax sample-fan-out (internal/chsql.emitRangeLWR). Like ts_grid_range it
	// is Experimental and explicit-only (NEVER enabled by auto; no legacy env
	// alias — list it in CERBERUS_CH_OPTIMIZATIONS to enable). It shares the
	// timeSeries*ToGrid family floor (25.6) and the same experimental
	// allow_experimental_time_series_aggregate_functions gate.
	FeatureTSGridResample = "ts_grid_resample"

	// FeatureColumnarResultDecode routes the four-column query_range matrix
	// projection through a dedicated ch-go (low-level) columnar decode path
	// instead of the per-row clickhouse-go/v2 Scan path, building each series'
	// label map once per contiguous run rather than once per row. It is a
	// CLIENT-SIDE decode optimization: it touches no server setting and works
	// on any native-protocol server, so it carries NO version floor
	// (AlwaysAvailable). It is opt-in-only (Experimental stability): a perf
	// tradeoff (a second ch-go dial), so auto never selects it; it engages only
	// when listed explicitly in CERBERUS_CH_OPTIMIZATIONS.
	FeatureColumnarResultDecode = "columnar_result_decode"
)

// AlwaysAvailable is the zero version floor for a feature that depends on no
// server-version gate at all (a purely client-side optimization such as
// columnar_result_decode). Version{} is the additive identity of AtLeast:
// every probed server version satisfies AtLeast(AlwaysAvailable), so listing
// such a feature explicitly never trips the "needs ClickHouse >=X" fail-fast,
// in either enforcing or permissive mode. It is named rather than written as a
// bare Version{} literal so the registry entry reads as an intentional "no
// version requirement" rather than a forgotten / zero-valued field.
var AlwaysAvailable = Version{Major: 0, Minor: 0}

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
// version that supports it, its stability class, and a one-line operator-facing
// description.
//
// Note: the per-feature ClickHouse allow_experimental_* setting is NOT a
// registry field. Stamping that setting lives in the engine plan path: the
// engine inspects the post-optimize plan (planHasTSGridNative) and co-stamps
// allow_experimental_time_series_aggregate_functions=1 via
// chclient.WithTSGridSetting on exactly the queries that use the native node,
// rather than on every query merely because the feature is enabled. Carrying a
// setting name on the registry entry as well would be a dead second source of
// truth, so it is intentionally absent here.
type Feature struct {
	ID         string
	MinVersion Version
	Stability  Stability
	Doc        string
}

// registry is the seeded feature table. It is value data (no init-time
// mutation), so Registry can hand out a defensive copy and callers cannot
// mutate the canonical entries.
var registry = []Feature{
	{
		ID:         FeatureAggregationInOrder,
		MinVersion: Version{Major: 24, Minor: 8},
		Stability:  Stable,
		Doc:        "stamp optimize_aggregation_in_order=1 when the Aggregate GROUP BY is a sort-key prefix (result-equivalent)",
	},
	{
		ID:         FeatureConditionCache,
		MinVersion: Version{Major: 25, Minor: 3},
		Stability:  Stable,
		Doc:        "stamp use_query_condition_cache=1 on predicate-stable read paths (result-equivalent cache, server >= 25.3)",
	},
	{
		ID:         FeatureTSGridRange,
		MinVersion: Version{Major: 25, Minor: 6},
		Stability:  Experimental,
		Doc:        "opt eligible rate(<counter>[<range>]) shapes onto native timeSeriesRateToGrid (experimental, explicit-only)",
	},
	{
		ID:         FeatureTSGridResample,
		MinVersion: Version{Major: 25, Minor: 6},
		Stability:  Experimental,
		Doc:        "opt the range-mode instant-vector staleness shape onto native timeSeriesResampleToGridWithStaleness (experimental, explicit-only)",
	},
	{
		ID:         FeatureColumnarResultDecode,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		Doc:        "decode the query_range matrix shape via ch-go columnar path (client-side, no version floor, opt-in only)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range, ts_grid_resample,
// columnar_result_decode). The copy keeps the canonical entries immutable from
// the caller's side. Exposed so tests can enumerate the gates and the docs
// generator can render the table.
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
