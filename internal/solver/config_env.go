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
	EnvMemoryApportion    = "CERBERUS_SHARD_MEMORY_APPORTION"
	EnvAutotune           = "CERBERUS_SOLVER_AUTOTUNE"
	EnvAutotuneInterval   = "CERBERUS_SOLVER_AUTOTUNE_INTERVAL"
)

// ConfigFromEnv builds a Config from the CERBERUS_* environment, starting
// from DefaultConfig and overriding each field from its env var when set. It
// does NOT call Validate — the caller (cmd/cerberus) runs Validate to fail-fast
// at startup, keeping the parse-vs-validate split the same as internal/config.
// A parse failure on any knob is returned so a typo never silently routes (or
// never silently disables routing).
//
// PRODUCTION DEFAULT: when CERBERUS_EVAL_ROUTE is unset the
// solver routes in "auto" mode — eligible plans that clear the cost thresholds
// take route B; everything else (ineligible / below-threshold / non-PromQL)
// fails toward the byte-identical route A. Operators pin "single" to disable
// routing entirely. The library default (DefaultConfig, Mode == "single")
// stays dark so in-process unit/spec tests that build it directly are
// unaffected; only this env-driven prod path flips to auto.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	// Unset CERBERUS_EVAL_ROUTE means "auto" in production, not the library's
	// dark "single" default.
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
	if cfg.MemoryApportion, err = envBool(EnvMemoryApportion, cfg.MemoryApportion); err != nil {
		return Config{}, err
	}
	if cfg.Autotune, err = envBool(EnvAutotune, cfg.Autotune); err != nil {
		return Config{}, err
	}
	if cfg.AutotuneInterval, err = envDuration(EnvAutotuneInterval, cfg.AutotuneInterval); err != nil {
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
