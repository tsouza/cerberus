package promql

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// resource_bounds_env.go closes cerberus issue #2667: the two cross-series
// histogram-merge resource-bound cost ceilings
// (maxHistogramMergeCostUnits, histogram_merge_bound.go;
// maxClassicBucketMergeCostUnits, classic_bucket_merge_bound.go) were
// hardcoded Go constants with no operator-facing override, the same problem
// class #2651/#2653/#2665 fixed for internal/chsql's
// range_bucket_grid_native_bound.go: a wrong calibration for real production
// traffic could only be corrected with a full code-change + release cycle.
//
// internal/promql may not import internal/config or internal/solver
// (.go-arch-lint.yml: "promql: mayDependOn: [chplan]"), so this file is a
// small, self-contained CERBERUS_* setting-parsing surface, mirroring
// internal/solver/config_env.go's own shape (its own settingInt64 helper,
// duplicated here rather than shared across the architecture boundary) but
// owned entirely by this package. It reads through a getter the caller
// supplies — the loader's environment-then-file lookup at boot, so a
// cerberus.yaml reaches these knobs — and internal/config's own registry of
// out-of-loader settings names each key here so the file accepts it. The
// parsed values are threaded down as explicit parameters —
// [LowerOpts.ResourceBounds] -> [lowerCtx.resourceBounds] -> the
// budget-guard call sites — never read from the environment at guard time,
// keeping every guard function a pure function of its arguments.
const (
	// EnvHistogramMergeMaxCostUnits overrides maxHistogramMergeCostUnits
	// (histogram_merge_bound.go): the native-histogram across-series merge
	// cost ceiling that closed cerberus issue #2385 after 19 real
	// production OOMs. Lowering it trades admitted series-count/width for
	// headroom; raising it trades headroom for admitted series-count/width
	// — it does not change the real ClickHouse memory cost a merge pays,
	// only which merges are refused before paying it. See that constant's
	// own doc for the calibration the shipped default protects.
	EnvHistogramMergeMaxCostUnits = "CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS"

	// EnvClassicBucketMergeMaxCostUnits overrides
	// maxClassicBucketMergeCostUnits (classic_bucket_merge_bound.go): the
	// classic-histogram across-series bucket-merge cost ceiling. See that
	// constant's own doc for the calibration the shipped default protects.
	EnvClassicBucketMergeMaxCostUnits = "CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS"

	// EnvExpHistogramWindowMaxCostUnits overrides the
	// samples-per-series-per-window pre-rejection's ceiling
	// (exp_histogram_window_sample_bound.go, cerberus issue #3252).
	//
	// Unlike the two above it has no fixed default: unset, the ceiling is
	// DERIVED from CERBERUS_CH_QUERY_MAX_MEMORY by
	// [ExpHistogramWindowCostUnitsForMemory], because the units count a
	// proxy for BYTES and the byte ceiling is itself configurable — the
	// same reasoning CERBERUS_RANGE_BUCKET_GRID_NATIVE_MAX_DENSITY_UNITS
	// carries. Setting a positive value pins it and opts out of the
	// derivation.
	//
	// One deliberate difference from that sibling: an EXPLICIT `0` is
	// rejected at startup here (envInt64's non-positive check, the rule
	// every CERBERUS_PROMQL_*_MAX_COST_UNITS knob in this file shares —
	// docs/configuration.md's "all seven reject a non-positive override"),
	// where the density-units sibling reads `0` as "derive". Only an UNSET
	// variable derives here. The two are not aligned because the rule is
	// per-file: the sibling's loader treats 0 as its documented default,
	// this file's treats every non-positive value as a mistake, and a knob
	// that alone accepted 0 would be the odd one out of its own family.
	EnvExpHistogramWindowMaxCostUnits = "CERBERUS_PROMQL_EXP_HISTOGRAM_WINDOW_MAX_COST_UNITS"
)

// ResourceBounds carries the operator-tunable cost ceilings for promql's
// cross-series histogram-merge resource-bound guards. The zero value is NOT
// a safe "unlimited" sentinel — histogramMergeCostOverBudgetExpr and
// classicBucketMergeCostOverBudgetExpr both reject whenever the emitted cost
// EXCEEDS the configured ceiling, so a zero-valued field rejects nearly
// every real merge rather than admitting every one. Callers should build a
// ResourceBounds through [DefaultResourceBounds] or
// [ResourceBoundsFromEnv] rather than a bare literal. Neither fills
// ExpHistogramWindowMaxCostUnits — that field is derived from
// CHQueryMaxMemory, not shipped as a constant — so every lowering entry
// point ([Lower] / [LowerAt] / [LowerAtRange] / [LowerAtRangeOpts] /
// [LowerMetadataRange] and the metadata catalog path) resolves whatever it
// was handed through [ResourceBounds.withDefaults] before any guard reads
// it; no guard defaults a zero ceiling on its own.
type ResourceBounds struct {
	// HistogramMergeMaxCostUnits bounds `rows x (posWidth^2 + negWidth^2)`
	// for the native-histogram across-series merge
	// (histogramMergeCostOverBudgetExpr, histogram_merge_bound.go).
	// Defaults to maxHistogramMergeCostUnits — see that constant's own doc
	// for the real-ClickHouse calibration behind the default, and cerberus
	// issue #2385's 19-real-OOM history the default must not regress.
	HistogramMergeMaxCostUnits int64

	// ClassicBucketMergeMaxCostUnits bounds `totalBucketVolume x
	// widestRowBucketWidth` for the classic-histogram across-series bucket
	// merge (classicBucketMergeCostOverBudgetExpr,
	// classic_bucket_merge_bound.go). Defaults to
	// maxClassicBucketMergeCostUnits — see that constant's own doc for the
	// real-ClickHouse calibration behind the default.
	ClassicBucketMergeMaxCostUnits int64

	// ExpHistogramWindowMaxCostUnits bounds `S x W x (S + W)` for one
	// exponential-histogram window group — S samples, W widest stored
	// bucket array — as a pre-rejection above the reduction that builds
	// the window's arrays (exp_histogram_window_sample_bound.go, cerberus
	// issue #3252). Its default is not a constant: [withDefaults] derives
	// it from [ResourceBounds.CHQueryMaxMemory], which the boot path fills
	// from CERBERUS_CH_QUERY_MAX_MEMORY.
	ExpHistogramWindowMaxCostUnits int64

	// CHQueryMaxMemory is the ClickHouse per-query memory cap the
	// deployment stamps, in bytes, and it is an INPUT to this struct
	// rather than a bound of its own: it is what
	// ExpHistogramWindowMaxCostUnits is derived from when no explicit
	// override is set. Zero means the deployment stamps no cap, which
	// [ExpHistogramWindowCostUnitsForMemory] answers for.
	CHQueryMaxMemory int64
}

// DefaultResourceBounds returns the shipped, load-bearing defaults —
// maxHistogramMergeCostUnits and maxClassicBucketMergeCostUnits, the same
// named Go constants every lowering path used before this file existed.
// Every caller that does not opt into [ResourceBoundsFromEnv] (the spec
// harness, [Lower] / [LowerAt] / [LowerAtRange], and any [LowerOpts] built
// by hand without setting ResourceBounds) gets these, so behaviour stays
// byte-identical to the pre-config-knob guards.
func DefaultResourceBounds() ResourceBounds {
	return ResourceBounds{
		HistogramMergeMaxCostUnits:     maxHistogramMergeCostUnits,
		ClassicBucketMergeMaxCostUnits: maxClassicBucketMergeCostUnits,
	}
}

// withDefaults returns a copy of b with any zero-valued field filled from
// [DefaultResourceBounds] — the same "unset means the shipped default"
// resolution [RangeLowerers.withDefaults] applies for the native-lowering
// dispatch table (lower_strategy.go), applied once at the single
// lowering-entry seam (lower.go's [Lower] / [LowerAt] / [LowerAtRange] /
// [LowerAtRangeOpts] / [LowerMetadataRange]) rather than re-derived at each
// of the many budget-guard call sites this threads through.
func (b ResourceBounds) withDefaults() ResourceBounds {
	def := DefaultResourceBounds()
	if b.HistogramMergeMaxCostUnits == 0 {
		b.HistogramMergeMaxCostUnits = def.HistogramMergeMaxCostUnits
	}
	if b.ClassicBucketMergeMaxCostUnits == 0 {
		b.ClassicBucketMergeMaxCostUnits = def.ClassicBucketMergeMaxCostUnits
	}
	if b.ExpHistogramWindowMaxCostUnits == 0 {
		// Derived, not defaulted: see [EnvExpHistogramWindowMaxCostUnits].
		b.ExpHistogramWindowMaxCostUnits = ExpHistogramWindowCostUnitsForMemory(b.CHQueryMaxMemory)
	}
	return b
}

// ResourceBoundsFromEnv is [ResourceBoundsFrom] over the process
// environment alone. The running gateway resolves through
// [ResourceBoundsFrom] with the loader's environment-then-file getter so a
// cerberus.yaml reaches these knobs; this form serves callers with no config
// file to consult.
func ResourceBoundsFromEnv() (ResourceBounds, error) {
	return ResourceBoundsFrom(os.Getenv)
}

// ResourceBoundsFrom builds a ResourceBounds from the CERBERUS_* settings
// get resolves, starting from [DefaultResourceBounds] and overriding each
// field from its setting when set. get has the shape
// [schema.DefaultOTelMetricsFrom] takes — os.Getenv, or the loader's
// environment-then-file lookup — because internal/promql may not import
// internal/config (.go-arch-lint.yml) and so cannot ask the loader itself.
// A parse failure is returned so a typo never silently widens (or narrows)
// a production safety rail — mirrors internal/solver/config_env.go's
// ConfigFrom fail-fast contract. The caller (cmd/cerberus, at boot) is
// responsible for threading the result into every
// [LowerOpts.ResourceBounds] the running deployment builds, exactly as it
// threads the boot-wired RangeLowerers table today.
func ResourceBoundsFrom(get func(string) string) (ResourceBounds, error) {
	cfg := DefaultResourceBounds()
	var err error
	if cfg.HistogramMergeMaxCostUnits, err = settingInt64(get, EnvHistogramMergeMaxCostUnits, cfg.HistogramMergeMaxCostUnits); err != nil {
		return ResourceBounds{}, err
	}
	if cfg.ClassicBucketMergeMaxCostUnits, err = settingInt64(get, EnvClassicBucketMergeMaxCostUnits, cfg.ClassicBucketMergeMaxCostUnits); err != nil {
		return ResourceBounds{}, err
	}
	// Left at 0 unless explicitly pinned: [withDefaults] derives it from
	// CHQueryMaxMemory, which the caller fills from the loaded config.
	if cfg.ExpHistogramWindowMaxCostUnits, err = settingInt64(get, EnvExpHistogramWindowMaxCostUnits, 0); err != nil {
		return ResourceBounds{}, err
	}
	return cfg, nil
}

// settingInt64 parses a 64-bit int setting through get, returning def when
// unset and a wrapped error when malformed OR non-positive (fail-fast at
// startup) — a self-contained cousin of internal/solver/config_env.go's own
// helper of the same name, tightened with the positivity check
// internal/engine/resource_bound_env.go's settingPositiveInt64 already applies
// for cerberus issue #2667's chsql-side siblings
// (CERBERUS_CH_*_MAX_ROWS): a cost-unit ceiling of zero or less is never a
// legitimate operator override — histogramMergeCostOverBudgetExpr /
// classicBucketMergeCostOverBudgetExpr both reject once the emitted cost
// EXCEEDS the ceiling, so zero would reject nearly every real merge and a
// negative value would reject all of them — so an operator who sets one to
// 0 or below almost certainly meant something else, and this fails fast at
// startup rather than silently bricking the histogram-quantile query path.
// internal/promql may not import internal/solver, internal/engine, or
// internal/config per .go-arch-lint.yml, so each package that needs
// CERBERUS_* env parsing carries its own small, dependency-free copy
// rather than sharing one across an architecture boundary.
func settingInt64(get func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("promql: %s: invalid integer %q: %w", key, v, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("promql: %s: must be > 0, got %d", key, n)
	}
	return n, nil
}
