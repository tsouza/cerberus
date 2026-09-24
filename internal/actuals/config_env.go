package actuals

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env var names for the actuals tuning surface (issue #2789). Mirrors
// internal/solver/config_env.go's own layout: one const block, one
// ConfigFrom, shared settingInt/settingFloat/settingDuration/settingBool
// parsers local to this package (solver's own helpers are unexported and this package must
// not import solver — see .go-arch-lint.yml).
const (
	EnvEnabled              = "CERBERUS_QUERY_ACTUALS_ENABLED"
	EnvDriftLowerRatio      = "CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO"
	EnvDriftUpperRatio      = "CERBERUS_QUERY_ACTUALS_DRIFT_UPPER_RATIO"
	EnvMinObservations      = "CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS"
	EnvEMAAlpha             = "CERBERUS_QUERY_ACTUALS_EMA_ALPHA"
	EnvEntryTTL             = "CERBERUS_QUERY_ACTUALS_ENTRY_TTL"
	EnvQueryLogPollInterval = "CERBERUS_QUERY_ACTUALS_QUERY_LOG_POLL_INTERVAL"
	EnvQueryLogLookback     = "CERBERUS_QUERY_ACTUALS_QUERY_LOG_LOOKBACK"
	EnvQueryLogSettleDelay  = "CERBERUS_QUERY_ACTUALS_QUERY_LOG_SETTLE_DELAY"
)

// ConfigFromEnv is [ConfigFrom] over the process environment alone. The
// running gateway resolves through [ConfigFrom] with the loader's
// environment-then-file getter so a cerberus.yaml reaches these knobs; this
// form serves callers with no config file to consult.
func ConfigFromEnv() (Config, error) {
	return ConfigFrom(os.Getenv)
}

// ConfigFrom builds a Config from the CERBERUS_QUERY_ACTUALS_* settings get
// resolves, starting from DefaultConfig (Enabled=false) and overriding each
// field from its setting when set. get has the shape
// [schema.DefaultOTelMetricsFrom] takes — os.Getenv, or the loader's
// environment-then-file lookup — because this package may not import
// internal/config (.go-arch-lint.yml) and so cannot ask the loader itself.
// Mirrors solver.ConfigFrom's own contract: does NOT call Validate (the
// caller runs that at startup), and a parse failure on any knob is returned
// so a typo never silently changes behavior.
func ConfigFrom(get func(string) string) (Config, error) {
	cfg := DefaultConfig()

	var err error
	if cfg.Enabled, err = settingBool(get, EnvEnabled, cfg.Enabled); err != nil {
		return Config{}, err
	}
	if cfg.DriftLowerRatio, err = settingFloat(get, EnvDriftLowerRatio, cfg.DriftLowerRatio); err != nil {
		return Config{}, err
	}
	if cfg.DriftUpperRatio, err = settingFloat(get, EnvDriftUpperRatio, cfg.DriftUpperRatio); err != nil {
		return Config{}, err
	}
	if cfg.MinObservations, err = settingInt(get, EnvMinObservations, cfg.MinObservations); err != nil {
		return Config{}, err
	}
	if cfg.EMAAlpha, err = settingFloat(get, EnvEMAAlpha, cfg.EMAAlpha); err != nil {
		return Config{}, err
	}
	if cfg.EntryTTL, err = settingDuration(get, EnvEntryTTL, cfg.EntryTTL); err != nil {
		return Config{}, err
	}
	if cfg.QueryLogPollInterval, err = settingDuration(get, EnvQueryLogPollInterval, cfg.QueryLogPollInterval); err != nil {
		return Config{}, err
	}
	if cfg.QueryLogLookback, err = settingDuration(get, EnvQueryLogLookback, cfg.QueryLogLookback); err != nil {
		return Config{}, err
	}
	if cfg.QueryLogSettleDelay, err = settingDuration(get, EnvQueryLogSettleDelay, cfg.QueryLogSettleDelay); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func settingInt(get func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

func settingFloat(get func(string) string, key string, def float64) (float64, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid float %q: %w", key, v, err)
	}
	return f, nil
}

func settingBool(get func(string) string, key string, def bool) (bool, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("actuals: %s: invalid boolean %q: %w", key, v, err)
	}
	return b, nil
}

func settingDuration(get func(string) string, key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}
