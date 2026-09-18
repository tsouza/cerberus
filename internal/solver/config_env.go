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
	// DeprecatedWarningsFrom return a notice (cmd/cerberus logs it once at
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
)

// DataShardCount has NO env var in this package by design: it is sourced
// from internal/chopt.ClusterTopology (see Config.DataShardCount's doc), not
// this package's own env surface. The data-shard fanout cap override
// (CERBERUS_SOLVER_DATA_SHARD_FANOUT_CAP) used to live here too; cerberus
// issue #3128 moved the whole fanout-gate mechanism to internal/chclient, so
// its override now lives on chclient.Config, parsed by internal/config.

// DeprecatedEnvWarnings is [DeprecatedWarningsFrom] over the process
// environment alone.
func DeprecatedEnvWarnings() []string {
	return DeprecatedWarningsFrom(os.Getenv)
}

// DeprecatedWarningsFrom returns a one-line notice for every soft-deprecated
// CERBERUS_* solver setting that get resolves to a non-empty value, for the
// caller to log at startup. Empty when none are set. get is the same
// environment-then-file getter [ConfigFrom] reads through, so a legacy name
// carried by a cerberus.yaml is announced exactly as an exported one is.
//
// Separate from ConfigFrom because this package must not choose a logger;
// cmd/cerberus owns that, and calls this from buildSolver right after
// ConfigFrom. Mirrors the CERBERUS_EXPERIMENTAL_TS_GRID_RANGE ->
// CERBERUS_CH_OPTIMIZATIONS deprecation (internal/chopt/resolve.go).
func DeprecatedWarningsFrom(get func(string) string) []string {
	var warns []string
	if strings.TrimSpace(get(EnvLegacyRouteMemoEnabled)) != "" {
		warns = append(warns, EnvLegacyRouteMemoEnabled+
			" is deprecated; use "+EnvAdaptiveEnabled+
			" (the old name still applies, and the new name wins when both are set)")
	}
	if strings.TrimSpace(get(envRetiredDisableSplitOnMultiDataShard)) != "" {
		warns = append(warns, envRetiredDisableSplitOnMultiDataShard+
			" is retired and has no effect; the sharded-pushdown solver never splits"+
			" a multi-data-shard deployment's dispatch on its own, so the knob"+
			" duplicated CERBERUS_CH_DATA_SHARDS > 1 and is gone")
	}
	return warns
}

// envRetiredDisableSplitOnMultiDataShard is the RETIRED knob the release
// audit removed. It is not in the Env* block above because nothing parses it
// any more — it exists only so an operator whose manifest still carries it
// gets the notice this facility exists to give, instead of the silence a
// plain deletion would have left (retired keys are otherwise ignored, see
// ConfigFromEnv's own doc).
const envRetiredDisableSplitOnMultiDataShard = "CERBERUS_SOLVER_DISABLE_SPLIT_ON_MULTI_DATA_SHARD"

// ConfigFromEnv is [ConfigFrom] over the process environment alone. The
// running gateway resolves through [ConfigFrom] with the loader's
// environment-then-file getter so a cerberus.yaml reaches these knobs; this
// form serves callers with no config file to consult.
func ConfigFromEnv() (Config, error) {
	return ConfigFrom(os.Getenv)
}

// ConfigFrom builds a Config from the CERBERUS_* settings get resolves,
// starting from DefaultConfig and overriding each field from its setting when
// set. get has the shape [schema.DefaultOTelMetricsFrom] takes — os.Getenv,
// or the loader's environment-then-file lookup — because this package may not
// import internal/config (.go-arch-lint.yml) and so cannot ask the loader
// itself. It does NOT call Validate — the caller (cmd/cerberus) runs Validate
// to fail-fast at startup, keeping the parse-vs-validate split the same as
// internal/config. A parse failure on any knob is returned so a typo never
// silently routes (or never silently disables routing).
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
func ConfigFrom(get func(string) string) (Config, error) {
	cfg := DefaultConfig()
	// Unset CERBERUS_EVAL_ROUTE means "auto" for a deployed binary, not the
	// library's dark "single" default.
	cfg.Mode = ModeAuto

	if v := strings.TrimSpace(get(EnvRoute)); v != "" {
		cfg.Mode = strings.ToLower(v)
	}

	var err error
	if cfg.MinFanout, err = settingInt(get, EnvMinFanout, cfg.MinFanout); err != nil {
		return Config{}, err
	}
	if cfg.MinAnchorPairs, err = settingInt(get, EnvMinAnchorPairs, cfg.MinAnchorPairs); err != nil {
		return Config{}, err
	}
	if cfg.MaxK, err = settingInt(get, EnvMaxK, cfg.MaxK); err != nil {
		return Config{}, err
	}
	if cfg.MinAnchorsPerSlice, err = settingInt(get, EnvMinAnchorsPerSlice, cfg.MinAnchorsPerSlice); err != nil {
		return Config{}, err
	}
	if cfg.Parallel, err = settingInt(get, EnvParallel, cfg.Parallel); err != nil {
		return Config{}, err
	}
	if cfg.Timeout, err = settingDuration(get, EnvTimeout, cfg.Timeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutputRows, err = settingInt64(get, EnvMaxOutputRows, cfg.MaxOutputRows); err != nil {
		return Config{}, err
	}
	// The legacy alias is layered FIRST so an explicit new-name setting wins,
	// and so "operator explicitly set the old one to false" is distinguishable
	// from "operator set neither" — a plain bool would conflate them and
	// silently re-enable a feature somebody deliberately turned off.
	if cfg.AdaptiveEnabled, err = settingBool(get, EnvLegacyRouteMemoEnabled, cfg.AdaptiveEnabled); err != nil {
		return Config{}, err
	}
	if cfg.AdaptiveEnabled, err = settingBool(get, EnvAdaptiveEnabled, cfg.AdaptiveEnabled); err != nil {
		return Config{}, err
	}
	if cfg.RouteMemoEntryTTL, err = settingDuration(get, EnvRouteMemoEntryTTL, cfg.RouteMemoEntryTTL); err != nil {
		return Config{}, err
	}
	if cfg.RouteMemoReValidationFraction, err = settingInt(get, EnvRouteMemoRevalFrac, cfg.RouteMemoReValidationFraction); err != nil {
		return Config{}, err
	}
	if cfg.EstimateNearEmptyRowFloor, err = settingInt64(get, EnvEstimateNearEmptyRowFloor, cfg.EstimateNearEmptyRowFloor); err != nil {
		return Config{}, err
	}
	if cfg.MaxKWithEstimate, err = settingInt(get, EnvMaxKWithEstimate, cfg.MaxKWithEstimate); err != nil {
		return Config{}, err
	}
	if cfg.EstimateMinRowsPerAdditionalShard, err = settingInt64(get, EnvEstimateMinRowsPerAdditionalShard, cfg.EstimateMinRowsPerAdditionalShard); err != nil {
		return Config{}, err
	}
	// DataShardCount is deliberately NOT read here — see its own doc: it is
	// sourced from internal/chopt.ClusterTopology by cmd/cerberus, stamped
	// onto cfg AFTER ConfigFromEnv returns.
	return cfg, nil
}

// settingInt parses an int setting through get, returning def when unset and
// a wrapped error when malformed (fail-fast at startup).
func settingInt(get func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

// settingInt64 parses a 64-bit int setting through get.
func settingInt64(get func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

// settingBool parses a boolean setting through get (strconv.ParseBool vocabulary).
func settingBool(get func(string) string, key string, def bool) (bool, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("solver: %s: invalid boolean %q: %w", key, v, err)
	}
	return b, nil
}

// settingDuration parses a Go duration setting through get.
func settingDuration(get func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("solver: %s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}
