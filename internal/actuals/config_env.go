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
// ConfigFromEnv, shared envInt/envFloat/envDuration/envBool parsers local to
// this package (solver's own helpers are unexported and this package must
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
)

// ConfigFromEnv builds a Config from the CERBERUS_QUERY_ACTUALS_* environment,
// starting from DefaultConfig (Enabled=false) and overriding each field from
// its env var when set. Mirrors solver.ConfigFromEnv's own contract: does NOT
// call Validate (the caller runs that at startup), and a parse failure on any
// knob is returned so a typo never silently changes behavior.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()

	var err error
	if cfg.Enabled, err = envBool(EnvEnabled, cfg.Enabled); err != nil {
		return Config{}, err
	}
	if cfg.DriftLowerRatio, err = envFloat(EnvDriftLowerRatio, cfg.DriftLowerRatio); err != nil {
		return Config{}, err
	}
	if cfg.DriftUpperRatio, err = envFloat(EnvDriftUpperRatio, cfg.DriftUpperRatio); err != nil {
		return Config{}, err
	}
	if cfg.MinObservations, err = envInt(EnvMinObservations, cfg.MinObservations); err != nil {
		return Config{}, err
	}
	if cfg.EMAAlpha, err = envFloat(EnvEMAAlpha, cfg.EMAAlpha); err != nil {
		return Config{}, err
	}
	if cfg.EntryTTL, err = envDuration(EnvEntryTTL, cfg.EntryTTL); err != nil {
		return Config{}, err
	}
	if cfg.QueryLogPollInterval, err = envDuration(EnvQueryLogPollInterval, cfg.QueryLogPollInterval); err != nil {
		return Config{}, err
	}
	if cfg.QueryLogLookback, err = envDuration(EnvQueryLogLookback, cfg.QueryLogLookback); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid integer %q: %w", key, v, err)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid float %q: %w", key, v, err)
	}
	return f, nil
}

func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("actuals: %s: invalid boolean %q: %w", key, v, err)
	}
	return b, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("actuals: %s: invalid duration %q: %w", key, v, err)
	}
	return d, nil
}
