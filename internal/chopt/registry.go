package chopt

// The structural feature table in docs/clickhouse-optimizations.md (id /
// minVersion / stability) is generated from this registry. Regenerate it with
// `just gen-opt-docs`; CI fails any PR whose generated block drifts. See
// cmd/optdocs.
//go:generate go run github.com/tsouza/cerberus/cmd/optdocs -doc ../../docs/clickhouse-optimizations.md

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
	// shapes onto the native timeSeriesRateToGrid aggregate. Its maturity is
	// Experimental, but it is AUTO-SELECTED on capable servers (>= 25.6): the
	// native path is result-correct (more correct than the buggy fan-out for
	// rate) and runs at flat memory, so auto picks it whenever the server can run
	// it. It is also reachable by the legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE
	// alias.
	FeatureTSGridRange = "ts_grid_range"

	// FeatureTSGridResample opts the eligible range-mode instant-vector
	// selection / staleness shape (the query_range bare-selector LWR) onto the
	// native timeSeriesResampleToGridWithStaleness aggregate, retiring the
	// argMax sample-fan-out (internal/chsql.emitRangeLWR). Like ts_grid_range its
	// maturity is Experimental but it is AUTO-SELECTED on capable servers (no
	// legacy env alias — auto picks it, or list it in CERBERUS_CH_OPTIMIZATIONS).
	// It shares the timeSeries*ToGrid family floor (25.6) and the same
	// experimental allow_experimental_time_series_aggregate_functions gate.
	FeatureTSGridResample = "ts_grid_resample"

	// FeatureColumnarResultDecode routes the four-column query_range matrix
	// projection through a dedicated ch-go (low-level) columnar decode path
	// instead of the per-row clickhouse-go/v2 Scan path, building each series'
	// label map once per contiguous run rather than once per row. It is a
	// CLIENT-SIDE decode optimization: it touches no server setting and works
	// on any native-protocol server, so it carries NO version floor
	// (AlwaysAvailable). It is opt-in-only (AutoSelect=false): a perf TRADEOFF (a
	// second ch-go dial, not a version-gated win), so auto MUST NEVER select it;
	// it engages only when listed explicitly in CERBERUS_CH_OPTIMIZATIONS —
	// typically alongside auto (`auto,columnar_result_decode`) to keep the
	// version-gated picks. This is the one feature auto leaves to the operator.
	FeatureColumnarResultDecode = "columnar_result_decode"

	// FeatureTSGridChanges opts eligible changes(<v>[<range>]) query_range
	// shapes onto the native timeSeriesChangesToGrid aggregate (the per-window
	// value-change count), retiring the arrayPopBack/arrayPopFront `c != p`
	// fan-out (internal/chsql.emitRangeWindowChanges). Like the rest of the
	// family its maturity is Experimental but it is AUTO-SELECTED on capable
	// servers (no legacy env alias — auto picks it once the server is >= 25.9, or
	// list it in CERBERUS_CH_OPTIMIZATIONS).
	//
	// IMPORTANT — the floor is 25.9, NOT the 25.6 of rate/resample.
	// timeSeriesChangesToGrid/ResetsToGrid shipped a full quarter later (PR
	// #86010, merged 2025-09-08, ClickHouse 25.9), empirically confirmed ABSENT
	// on the 25.8 chDB substrate. A 25.6 floor here would mis-advertise support
	// on 25.6-25.8 servers and 502 with UNKNOWN_AGGREGATE_FUNCTION. The
	// experimental allow_experimental_time_series_aggregate_functions gate is
	// shared with the rest of the family.
	FeatureTSGridChanges = "ts_grid_changes"

	// FeatureTSGridResets opts eligible resets(<counter>[<range>]) query_range
	// shapes onto the native timeSeriesResetsToGrid aggregate (the per-window
	// counter-reset count), retiring the arrayPopBack/arrayPopFront `c < p`
	// fan-out (internal/chsql.emitRangeWindowResets). Experimental maturity but
	// AUTO-SELECTED on capable servers, same 25.9 floor and same experimental
	// gate as FeatureTSGridChanges (the two are siblings from PR #86010).
	FeatureTSGridResets = "ts_grid_resets"
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

// Stability classifies a registry feature's MATURITY (operator-facing docs /
// support expectations) — it is deliberately DECOUPLED from auto-eligibility,
// which lives on the separate Feature.AutoSelect axis. A feature can be
// Experimental in maturity yet auto-selected by version (the native
// timeSeries*ToGrid aggregates: validated result-correct + flat-memory, so auto
// picks them on capable servers while their docs stay honestly "experimental").
type Stability int

const (
	// Stable features are mature and documented as production-ready.
	Stable Stability = iota
	// Experimental features are honestly young in maturity. This says nothing
	// about whether auto picks them — that is Feature.AutoSelect.
	Experimental
)

// Feature is one registry entry: a stable id, the minimum major.minor server
// version that supports it, its stability class, an auto-eligibility flag, and a
// one-line operator-facing description.
//
// AutoSelect is the auto-eligibility axis, kept distinct from Stability
// (maturity): under the default `auto` selection a feature is enabled iff
// AutoSelect is true AND the probed server satisfies MinVersion. This lets an
// Experimental-maturity feature still be auto-enabled by version (the native
// timeSeries*ToGrid aggregates), while a feature that is a deliberate perf
// TRADEOFF rather than a version-gated win (columnar_result_decode) stays
// AutoSelect=false and is reachable only by explicit listing.
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
	AutoSelect bool
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
		AutoSelect: true,
		Doc:        "stamp optimize_aggregation_in_order=1 when the Aggregate GROUP BY is a sort-key prefix (result-equivalent)",
	},
	{
		ID:         FeatureConditionCache,
		MinVersion: Version{Major: 25, Minor: 3},
		Stability:  Stable,
		AutoSelect: true,
		Doc:        "stamp use_query_condition_cache=1 on predicate-stable read paths (result-equivalent cache, server >= 25.3)",
	},
	{
		ID:         FeatureTSGridRange,
		MinVersion: Version{Major: 25, Minor: 6},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "opt eligible rate(<counter>[<range>]) shapes onto native timeSeriesRateToGrid (experimental maturity, auto-enabled on server >= 25.6)",
	},
	{
		ID:         FeatureTSGridResample,
		MinVersion: Version{Major: 25, Minor: 6},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "opt the range-mode instant-vector staleness shape onto native timeSeriesResampleToGridWithStaleness (experimental maturity, auto-enabled on server >= 25.6)",
	},
	{
		ID:         FeatureColumnarResultDecode,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "decode the query_range matrix shape via ch-go columnar path (client-side, no version floor, opt-in only — never auto)",
	},
	{
		ID:         FeatureTSGridChanges,
		MinVersion: Version{Major: 25, Minor: 9},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "opt eligible changes(<v>[<range>]) shapes onto native timeSeriesChangesToGrid (experimental maturity, auto-enabled on server >= 25.9)",
	},
	{
		ID:         FeatureTSGridResets,
		MinVersion: Version{Major: 25, Minor: 9},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "opt eligible resets(<counter>[<range>]) shapes onto native timeSeriesResetsToGrid (experimental maturity, auto-enabled on server >= 25.9)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range, ts_grid_resample,
// columnar_result_decode, ts_grid_changes, ts_grid_resets). The copy keeps the
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
