package solver

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env var names for the solver tuning surface. CERBERUS_EVAL_ROUTE is the
// master switch (default "auto"; operators pin "single" to disable routing);
// the rest map 1:1 onto the Config fields and default to DefaultConfig's
// conservative values when unset.
const (
	EnvRoute              = "CERBERUS_EVAL_ROUTE"
	EnvMinFanout          = "CERBERUS_SHARD_MIN_FANOUT"
	EnvMinAnchorPairs     = "CERBERUS_SHARD_MIN_ANCHOR_PAIRS"
	EnvMaxK               = "CERBERUS_SHARD_MAX_K"
	EnvMinAnchorsPerSlice = "CERBERUS_SHARD_MIN_ANCHORS_PER_SLICE"
	EnvParallel           = "CERBERUS_SHARD_PARALLEL"
	EnvTimeout            = "CERBERUS_SOLVER_TIMEOUT"
	EnvMaxOutputRows      = "CERBERUS_SHARD_MAX_OUTPUT_ROWS"
	EnvAdaptiveEnabled    = "CERBERUS_SOLVER_ADAPTIVE_ENABLED"
	// EnvLegacyRouteMemoEnabled is the SOFT-DEPRECATED spelling of
	// EnvAdaptiveEnabled. It still works; setting it makes
	// DeprecatedEnvWarnings return a notice (cmd/cerberus logs it once at
	// startup), and the new name wins when both are set. Kept because an
	// operator who explicitly disabled the feature must not have it silently
	// re-enabled by an upgrade that only renamed the knob.
	EnvLegacyRouteMemoEnabled = "CERBERUS_SOLVER_ROUTE_MEMO_ENABLED"
	EnvRouteMemoEntryTTL      = "CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL"
	EnvRouteMemoRevalFrac     = "CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION"

	// EnvEstimateNearEmptyRowFloor, EnvMaxKWithEstimate and
	// EnvEstimateMinRowsPerAdditionalShard map onto Config's issue #2787
	// advisory EXPLAIN ESTIMATE thresholds — see each field's own doc.
	EnvEstimateNearEmptyRowFloor         = "CERBERUS_SHARD_ESTIMATE_NEAR_EMPTY_ROW_FLOOR"
	EnvMaxKWithEstimate                  = "CERBERUS_SHARD_MAX_K_WITH_ESTIMATE"
	EnvEstimateMinRowsPerAdditionalShard = "CERBERUS_SHARD_ESTIMATE_MIN_ROWS_PER_ADDITIONAL_SHARD"

	// EnvDataShardFanoutCapOverride and EnvDisableSplitOnMultiDataShard map
	// onto Config.DataShardFanoutCapOverride / Config.DisableSplitOnMultiDataShard
	// (cerberus issue #3081, epic #3074) — see each field's own doc.
	// DataShardCount itself has NO env var here by design: it is sourced
	// from internal/chopt.ClusterTopology (see Config.DataShardCount's doc),
	// not this package's own env surface.
	EnvDataShardFanoutCapOverride   = "CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP"
	EnvDisableSplitOnMultiDataShard = "CERBERUS_SOLVER_DISABLE_SPLIT_ON_MULTI_DATA_SHARD"
)

// DeprecatedEnvWarnings returns a one-line notice for every soft-deprecated
// CERBERUS_* solver var that is SET in the environment, for the caller to log
// at startup. Empty when none are set.
//
// Separate from ConfigFromEnv because this package must not choose a logger;
// cmd/cerberus owns that, and calls this from buildSolver right after
// ConfigFromEnv. Mirrors the CERBERUS_EXPERIMENTAL_TS_GRID_RANGE ->
// CERBERUS_CH_OPTIMIZATIONS deprecation (internal/chopt/resolve.go).
func DeprecatedEnvWarnings() []string {
	var warns []string
	if _, ok := os.LookupEnv(EnvLegacyRouteMemoEnabled); ok {
		warns = append(warns, EnvLegacyRouteMemoEnabled+
			" is deprecated; use "+EnvAdaptiveEnabled+
			" (the old name still applies, and the new name wins when both are set)")
	}
	return warns
}

// ConfigFromEnv builds a Config from the CERBERUS_* environment, starting
// from DefaultConfig and overriding each field from its env var when set. It
// does NOT call Validate — the caller (cmd/cerberus) runs Validate to fail-fast
// at startup, keeping the parse-vs-validate split the same as internal/config.
// A parse failure on any knob is returned so a typo never silently routes (or
// never silently disables routing).
//
// Only the keys listed above are read; anything else in the environment is
// ignored. Retired knobs therefore stay inert rather than failing startup, so a
// deployment still carrying one in its manifest boots on the configured
// defaults instead of crash-looping (asserted by TestConfigFromEnv_RetiredKnobsIgnored).
//
// DEPLOYED DEFAULT: when CERBERUS_EVAL_ROUTE is unset the
// solver routes in "auto" mode — eligible plans that clear the cost thresholds
// take route B; everything else (ineligible / below-threshold / non-PromQL)
// fails toward the byte-identical route A. Operators pin "single" to disable
// routing entirely. The library default (DefaultConfig, Mode == "single")
// stays dark so in-process unit/spec tests that build it directly are
// unaffected; only this env-driven path flips to auto.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	// Unset CERBERUS_EVAL_ROUTE means "auto" for a deployed binary, not the
	// library's dark "single" default.
	cfg.Mode = ModeAuto

	if v := strings.TrimSpace(os.Getenv(EnvRoute)); v != "" {
		cfg.Mode = strings.ToLower(v)
	}

	var err error
	if cfg.MinFanout, err = envInt(EnvMinFanout, cfg.MinFanout); err != nil {
		return Config{}, err
	}
	if cfg.MinAnchorPairs, err = envInt(EnvMinAnchorPairs, cfg.MinAnchorPairs); err != nil {
		return Config{}, err
	}
	if cfg.MaxK, err = envInt(EnvMaxK, cfg.MaxK); err != nil {
		return Config{}, err
	}
	if cfg.MinAnchorsPerSlice, err = envInt(EnvMinAnchorsPerSlice, cfg.MinAnchorsPerSlice); err != nil {
		return Config{}, err
	}
	if cfg.Parallel, err = envInt(EnvParallel, cfg.Parallel); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = envDuration(EnvTimeout, cfg.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutputRows, err = envInt64(EnvMaxOutputRows, cfg.MaxOutputRows); err != nil {
		return Config{}, err
	}
	// The legacy alias is layered FIRST so an explicit new-name setting wins,
	// and so "operator explicitly set the old one to false" is distinguishable
	// from "operator set neither" — a plain bool would conflate them and
	// silently re-enable a feature somebody deliberately turned off.
	if cfg.AdaptiveEnabled, err = envBool(EnvLegacyRouteMemoEnabled, cfg.AdaptiveEnabled); err != nil {
		return Config{}, err
	}
	if cfg.AdaptiveEnabled, err = envBool(EnvAdaptiveEnabled, cfg.AdaptiveEnabled); err != nil {
		return Config{}, err
	}
	if cfg.RouteMemoEntryTTL, err = envDuration(EnvRouteMemoEntryTTL, cfg.RouteMemoEntryTTL); err != nil {
		return Config{}, err
	}
	if cfg.RouteMemoReValidationFraction, err = envInt(EnvRouteMemoRevalFrac, cfg.RouteMemoReValidationFraction); err != nil {
		return Config{}, err
	}
	if cfg.EstimateNearEmptyRowFloor, err = envInt64(EnvEstimateNearEmptyRowFloor, cfg.EstimateNearEmptyRowFloor); err != nil {
		return Config{}, err
	}
	if cfg.MaxKWithEstimate, err = envInt(EnvMaxKWithEstimate, cfg.MaxKWithEstimate); err != nil {
		return Config{}, err
	}
	if cfg.EstimateMinRowsPerAdditionalShard, err = envInt64(EnvEstimateMinRowsPerAdditionalShard, cfg.EstimateMinRowsPerAdditionalShard); err != nil {
		return Config{}, err
	}
	// DataShardCount is deliberately NOT read here — see its own doc: it is
	// sourced from internal/chopt.ClusterTopology by cmd/cerberus, stamped
	// onto cfg AFTER ConfigFromEnv returns.
	if cfg.DisableSplitOnMultiDataShard, err = envBool(EnvDisableSplitOnMultiDataShard, cfg.DisableSplitOnMultiDataShard); err != nil {
		return Config{}, err
	}
	if cfg.DataShardFanoutCapOverride, err = envOptionalInt64(EnvDataShardFanoutCapOverride, cfg.DataShardFanoutCapOverride); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// envInt parses an int env var, returning def when unset and a wrapped error
// when malformed (fail-fast at startup).
func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

// envInt64 parses a 64-bit int env var.
func envInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

// envOptionalInt64 parses an OPTIONAL 64-bit int env var: unset returns def
// (the incoming Config field, itself nil unless a prior call already set it)
// unchanged, so ConfigFromEnv's zero value stays nil rather than a duplicated
// magic default; set parses and returns a pointer to the value. Unlike
// envInt64, a malformed non-empty value is the only failure mode — there is
// no "missing means the default" ambiguity to resolve, because the default
// itself IS "unset" (nil).
func envOptionalInt64(key string, def *int64) (*int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("solver: %s: invalid integer %q: %w", key, v, err)
	}
	return &n, nil
}

// envBool parses a boolean env var (strconv.ParseBool vocabulary).
func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("solver: %s: invalid boolean %q: %w", key, v, err)
	}
	return b, nil
}

// envDuration parses a Go duration env var.
func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}
