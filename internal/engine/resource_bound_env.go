package engine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// resource_bound_env.go reads the operator-facing override surface for
// three chsql sample-fanout resource-bound safety ceilings (issue #2667):
//
//   - CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS — RangeBucketFanout's
//     collapse GROUP BY (internal/chsql/lwr_fanout_bound.go,
//     maxRangeBucketFanoutRows, default 4,000,000).
//   - CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS — RangeLWR's collapse GROUP BY
//     (same file, maxRangeLWRFanoutRows, default 40,000,000).
//   - CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS —
//     emitWindowedArrayExtrapolatedMatrix's regroup GROUP BY
//     (internal/chsql/rate_window_fanout_bound.go, maxRateWindowFanoutRows,
//     default 2,800,000).
//
// Each constant is a compile-time resource-bound safety ceiling gating
// query execution/plan shape that has already cost this repo two real
// production incidents (#2651/#2653/#2665, RangeBucketGridNative's own two
// axes) — a wrong calibration could only be corrected with a full
// code-change-plus-release cycle, because there was no operator-facing
// knob to reach for in between a bug report and the next release.
//
// internal/chsql may not import internal/config (.go-arch-lint.yml:
// chsql: mayDependOn: [chplan, spansscan]), so — mirroring
// internal/solver/config_env.go's own self-contained env-parsing pattern
// for the solver's tuning surface, called directly from cmd/cerberus/main.go
// rather than routed through internal/config's Viper machinery — this file
// owns the parsing itself and hands the caller (cmd/cerberus) plain int64
// values to assign onto the Engine fields below. Engine is already the
// seam that threads chsql.WithDeltaPrefixLookback /
// chsql.WithDeltaPrefixReadEnabled onto the emit context for both route A
// (emitForHead) and route B (routeBExecCtx); this reuses the identical two
// call paths for the three new bounds.
//
// Naming and grammar deliberately match CERBERUS_CH_QUERY_MAX_MEMORY /
// CERBERUS_CH_MAX_OPEN_CONNS's own CERBERUS_CH_* prefix (internal/config):
// these are chsql-adjacent, ClickHouse-query-shape knobs in the same family,
// even though — unlike those two — they are parsed here rather than via
// internal/config, because chsql's own dependency-cone rule keeps this
// surface out of that package's reach.
const (
	// EnvRangeBucketFanoutMaxRows overrides maxRangeBucketFanoutRows.
	EnvRangeBucketFanoutMaxRows = "CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS"
	// EnvRangeLWRFanoutMaxRows overrides maxRangeLWRFanoutRows.
	EnvRangeLWRFanoutMaxRows = "CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS"
	// EnvRateWindowFanoutMaxRows overrides maxRateWindowFanoutRows.
	EnvRateWindowFanoutMaxRows = "CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS"
)

// ResourceBoundOverrides is the resolved operator override for the three
// env vars above. A zero field means "the operator did not set this one" —
// the caller (emitForHead / routeBExecCtx) leaves the corresponding chsql
// ctx value unthreaded so chsql falls back to its own compiled-in,
// calibrated default. Unlike CERBERUS_DELTA_PREFIX_LOOKBACK, where 0 is a
// meaningful explicit opt-out, a fanout row bound of 0 is never a
// legitimate operator intent (it would reject every query outright), so
// reserving 0 as the "unset" sentinel loses no real configuration and
// ResourceBoundsFromEnv rejects an explicit 0 or negative override as a
// startup error instead of silently accepting it.
type ResourceBoundOverrides struct {
	RangeBucketFanoutMaxRows int64
	RangeLWRFanoutMaxRows    int64
	RateWindowFanoutMaxRows  int64
}

// ResourceBoundsFromEnv reads the three CERBERUS_CH_*_MAX_ROWS knobs above.
// An unset var resolves its field to 0 (see ResourceBoundOverrides' own
// doc); a set var is parsed as a base-10 int64 and must be strictly
// positive. A parse failure or a non-positive value is returned as an
// error so a typo — or an operator override that would reject every query
// — fails fast at startup rather than silently falling back to the
// default or silently bricking the query path.
func ResourceBoundsFromEnv() (ResourceBoundOverrides, error) {
	var overrides ResourceBoundOverrides
	var err error
	if overrides.RangeBucketFanoutMaxRows, err = envPositiveInt64(EnvRangeBucketFanoutMaxRows); err != nil {
		return ResourceBoundOverrides{}, err
	}
	if overrides.RangeLWRFanoutMaxRows, err = envPositiveInt64(EnvRangeLWRFanoutMaxRows); err != nil {
		return ResourceBoundOverrides{}, err
	}
	if overrides.RateWindowFanoutMaxRows, err = envPositiveInt64(EnvRateWindowFanoutMaxRows); err != nil {
		return ResourceBoundOverrides{}, err
	}
	return overrides, nil
}

// envPositiveInt64 parses an optional strictly-positive int64 env var,
// returning 0 (the "unset" sentinel — see ResourceBoundOverrides' own doc)
// when key is unset or blank, and a wrapped error both on a malformed
// value and on a non-positive one (0 or negative).
func envPositiveInt64(key string) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("engine: %s: invalid integer %q: %w", key, v, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("engine: %s: must be > 0, got %d", key, n)
	}
	return n, nil
}
