package config

// externalSettings are the CERBERUS_* settings the running gateway parses
// OUTSIDE the typed registry (allEnvKeys). Their owners — internal/promql,
// internal/engine, internal/solver, internal/actuals — may not import
// internal/config (.go-arch-lint.yml), so each parses its own knobs from a
// getter, and the boot path feeds that getter with [Config.Settings] so a
// cerberus.yaml reaches them exactly as an exported variable does.
//
// The list exists for the file's membership check: a flat key is accepted
// only when the registry, the nested-shape table, or this list knows it,
// because a key nothing reads is the failure the rejection exists to catch —
// the file loads cleanly and the setting silently stays on its default.
//
// Each entry restates the owner's exported Env* constant rather than
// referencing it, for the same import-boundary reason the owners parse for
// themselves. TestExternalSettings_MatchTheirOwners pins this list against
// those constants in both directions, so a knob added to an owner without an
// entry here — or an entry here whose owner dropped the knob — fails the
// build rather than the next operator.
var externalSettings = []string{
	// internal/promql/resource_bounds_env.go
	"CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS",
	"CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS",
	"CERBERUS_PROMQL_EXP_HISTOGRAM_WINDOW_MAX_COST_UNITS",

	// internal/engine/resource_bound_env.go
	"CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS",
	"CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS",
	"CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS",
	"CERBERUS_CH_MAX_EMITTED_SQL_BYTES",
	"CERBERUS_CH_RANGE_BUCKET_FANOUT_GROUP_MAX_COST_UNITS",

	// internal/solver/config_env.go
	"CERBERUS_EVAL_ROUTE",
	"CERBERUS_SHARD_MIN_FANOUT",
	"CERBERUS_SHARD_MIN_ANCHOR_PAIRS",
	"CERBERUS_SHARD_MAX_K",
	"CERBERUS_SHARD_MIN_ANCHORS_PER_SLICE",
	"CERBERUS_SHARD_PARALLEL",
	"CERBERUS_SOLVER_TIMEOUT",
	"CERBERUS_SHARD_MAX_OUTPUT_ROWS",
	"CERBERUS_SOLVER_ADAPTIVE_ENABLED",
	"CERBERUS_SOLVER_ROUTE_MEMO_ENABLED",
	"CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL",
	"CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION",
	"CERBERUS_SHARD_ESTIMATE_NEAR_EMPTY_ROW_FLOOR",
	"CERBERUS_SHARD_MAX_K_WITH_ESTIMATE",
	"CERBERUS_SHARD_ESTIMATE_MIN_ROWS_PER_ADDITIONAL_SHARD",

	// internal/actuals/config_env.go
	"CERBERUS_QUERY_ACTUALS_ENABLED",
	"CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO",
	"CERBERUS_QUERY_ACTUALS_DRIFT_UPPER_RATIO",
	"CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS",
	"CERBERUS_QUERY_ACTUALS_EMA_ALPHA",
	"CERBERUS_QUERY_ACTUALS_ENTRY_TTL",
	"CERBERUS_QUERY_ACTUALS_QUERY_LOG_POLL_INTERVAL",
	"CERBERUS_QUERY_ACTUALS_QUERY_LOG_LOOKBACK",
}

// knownSettings is every flat CERBERUS_* key a cerberus.yaml may carry: the
// typed registry, every setting the nested shape binds (the `cerberus
// migrate` surface and the read-side schema shape live only there), and the
// out-of-loader settings above. Building it doubles as the guard against a
// key being claimed by two of the three sources, which would leave the
// second parse dead.
var knownSettings = func() map[string]struct{} {
	known := make(map[string]struct{}, len(allEnvKeys)+len(bindings)+len(externalSettings))
	for _, key := range allEnvKeys {
		known[key] = struct{}{}
	}
	for _, b := range bindings {
		known[b.setting] = struct{}{}
	}
	for _, key := range externalSettings {
		if _, dup := known[key]; dup {
			panic("config: external setting " + key + " is also resolved by the loader")
		}
		known[key] = struct{}{}
	}
	return known
}()
