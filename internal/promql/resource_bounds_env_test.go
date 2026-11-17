package promql

import (
	"strings"
	"testing"
)

// TestResourceBoundsFromEnv_DefaultsUnset confirms that with no
// CERBERUS_PROMQL_* ceiling set, ResourceBoundsFromEnv returns exactly
// DefaultResourceBounds — the maxHistogramMergeCostUnits constant, with the
// classic per-series window ceiling left unset for withDefaults to derive
// from the ClickHouse memory cap.
func TestResourceBoundsFromEnv_DefaultsUnset(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "")
	t.Setenv(EnvClassicBucketWindowSeriesMaxEntries, "")
	t.Setenv(EnvExpHistogramWindowMaxCostUnits, "")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	want := DefaultResourceBounds()
	if got != want {
		t.Fatalf("ResourceBoundsFromEnv() = %+v, want the shipped defaults %+v", got, want)
	}
	if got.HistogramMergeMaxCostUnits != maxHistogramMergeCostUnits {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the maxHistogramMergeCostUnits constant %d",
			got.HistogramMergeMaxCostUnits, maxHistogramMergeCostUnits)
	}
	if got.ClassicBucketWindowSeriesMaxEntries != 0 {
		t.Fatalf("ClassicBucketWindowSeriesMaxEntries = %d, want 0: unset, it is derived from CHQueryMaxMemory",
			got.ClassicBucketWindowSeriesMaxEntries)
	}
}

// TestResourceBoundsFromEnv_HistogramMergeOverride confirms
// CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS overrides
// HistogramMergeMaxCostUnits alone, leaving
// ClassicBucketWindowSeriesMaxEntries unset for its own derivation — the two
// knobs are independent.
func TestResourceBoundsFromEnv_HistogramMergeOverride(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "123456789")
	t.Setenv(EnvClassicBucketWindowSeriesMaxEntries, "")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	if got.HistogramMergeMaxCostUnits != 123456789 {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want 123456789 (the env override)", got.HistogramMergeMaxCostUnits)
	}
	if got.ClassicBucketWindowSeriesMaxEntries != 0 {
		t.Fatalf("ClassicBucketWindowSeriesMaxEntries = %d, want it left unset (0) for the derivation",
			got.ClassicBucketWindowSeriesMaxEntries)
	}
}

// TestResourceBoundsFromEnv_ClassicBucketWindowSeriesOverride confirms the
// per-series window variable sets its ceiling alone, and that unset it
// leaves the ceiling at zero for the derivation.
func TestResourceBoundsFromEnv_ClassicBucketWindowSeriesOverride(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "")
	t.Setenv(EnvClassicBucketWindowSeriesMaxEntries, "")
	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	if got.ClassicBucketWindowSeriesMaxEntries != 0 {
		t.Fatalf("unset per-series window ceiling = %d, want 0 for the derivation", got.ClassicBucketWindowSeriesMaxEntries)
	}

	t.Setenv(EnvClassicBucketWindowSeriesMaxEntries, "5678")
	got, err = ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	if got.ClassicBucketWindowSeriesMaxEntries != 5678 {
		t.Fatalf("per-series window ceiling = %d, want 5678", got.ClassicBucketWindowSeriesMaxEntries)
	}
	if got.HistogramMergeMaxCostUnits != maxHistogramMergeCostUnits {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the untouched default %d",
			got.HistogramMergeMaxCostUnits, maxHistogramMergeCostUnits)
	}
}

// TestResourceBoundsFromEnv_MalformedFailsFast confirms a non-integer value
// on either var is a startup error rather than a silent fallback to the
// default — the same fail-fast contract internal/solver/config_env.go's
// ConfigFromEnv applies, so a typo'd operator override never silently
// widens (or narrows) this production safety rail.
func TestResourceBoundsFromEnv_MalformedFailsFast(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T)
	}{
		{"histogram merge", func(t *testing.T) { t.Setenv(EnvHistogramMergeMaxCostUnits, "not-a-number") }},
		{"classic bucket window series", func(t *testing.T) { t.Setenv(EnvClassicBucketWindowSeriesMaxEntries, "not-a-number") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(t)
			if _, err := ResourceBoundsFromEnv(); err == nil {
				t.Fatal("ResourceBoundsFromEnv() error = nil, want a parse error for the malformed value")
			}
		})
	}
}

// TestResourceBoundsFromEnv_NonPositiveFailsFast confirms an explicit 0 or
// negative override fails fast rather than being silently treated as
// "unset" — a cost-unit ceiling of zero or less is never a legitimate
// operator intent (see envInt64's own doc), so this must not be conflated
// with leaving the var unset.
func TestResourceBoundsFromEnv_NonPositiveFailsFast(t *testing.T) {
	// Each knob is set ALONE, so the rejection is attributable to it — the
	// exp-histogram window knob in particular, whose density-units sibling
	// in internal/config reads an explicit 0 as "derive" while this file
	// rejects it (see EnvExpHistogramWindowMaxCostUnits' own doc).
	for _, key := range []string{
		EnvHistogramMergeMaxCostUnits, EnvExpHistogramWindowMaxCostUnits, EnvClassicBucketWindowSeriesMaxEntries,
	} {
		for _, v := range []string{"0", "-1"} {
			t.Run(key+"="+v, func(t *testing.T) {
				t.Setenv(key, v)
				if _, err := ResourceBoundsFromEnv(); err == nil {
					t.Fatalf("ResourceBoundsFromEnv() error = nil with %s=%q, want a non-positive rejection", key, v)
				}
			})
		}
	}
}

// TestResourceBounds_WithDefaults confirms the zero value resolves the
// constant-defaulted field from DefaultResourceBounds, that the DERIVED
// fields resolve from CHQueryMaxMemory instead (they are deliberately absent
// from DefaultResourceBounds, so the two are not field-by-field equal), and
// that a fully-populated value passes through unchanged — the resolution
// [Lower] / [LowerAt] / [LowerAtRange] / [LowerAtRangeOpts] /
// [LowerMetadataRange] apply at their own lowering-entry seam.
func TestResourceBounds_WithDefaults(t *testing.T) {
	zero := (ResourceBounds{}).withDefaults()
	if got, want := zero.HistogramMergeMaxCostUnits, int64(maxHistogramMergeCostUnits); got != want {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the shipped default %d", got, want)
	}
	if got, want := zero.ClassicBucketWindowSeriesMaxEntries, ClassicBucketWindowSeriesEntriesForMemory(0); got != want {
		t.Fatalf("ClassicBucketWindowSeriesMaxEntries = %d for an unset cap, want the derived %d", got, want)
	}
	if DefaultResourceBounds().ClassicBucketWindowSeriesMaxEntries != 0 {
		t.Fatal("DefaultResourceBounds must leave ClassicBucketWindowSeriesMaxEntries unset: it is derived from CHQueryMaxMemory, not a shipped constant")
	}
	// The exp-histogram ceiling is derived from the deployment's ClickHouse
	// cap rather than defaulted from a constant, so it is absent from
	// DefaultResourceBounds by design. A zero cap means the deployment
	// stamps none, which resolves to the one-GiB grant rather than to
	// "unbounded" — an unbounded ceiling would disable the guard entirely
	// on exactly the deployments that never set a cap.
	if got, want := zero.ExpHistogramWindowMaxCostUnits, expHistogramWindowCostUnitsPerGiB; got != want {
		t.Fatalf("ExpHistogramWindowMaxCostUnits = %d for an unset cap, want the one-GiB grant %d", got, want)
	}
	if DefaultResourceBounds().ExpHistogramWindowMaxCostUnits != 0 {
		t.Fatal("DefaultResourceBounds must leave ExpHistogramWindowMaxCostUnits unset: it is derived from CHQueryMaxMemory, not a shipped constant")
	}
	// A real cap derives a proportionally larger ceiling, so the field is
	// genuinely a function of the cap and not a second constant.
	capped := ResourceBounds{CHQueryMaxMemory: 4 * bytesPerGiB}.withDefaults()
	if got, want := capped.ExpHistogramWindowMaxCostUnits, 4*expHistogramWindowCostUnitsPerGiB; got != want {
		t.Fatalf("ExpHistogramWindowMaxCostUnits = %d for a 4 GiB cap, want %d", got, want)
	}

	// Fully populated means every resolvable field, the derived one
	// included — an explicit ceiling must win over the cap-derived value,
	// which is what makes CERBERUS_PROMQL_EXP_HISTOGRAM_WINDOW_MAX_COST_UNITS
	// an override rather than a suggestion.
	explicit := ResourceBounds{
		HistogramMergeMaxCostUnits:          7,
		ClassicBucketWindowSeriesMaxEntries: 15,
		ExpHistogramWindowMaxCostUnits:      11,
		CHQueryMaxMemory:                    64 * bytesPerGiB,
	}
	if got := explicit.withDefaults(); got != explicit {
		t.Fatalf("a fully-populated ResourceBounds.withDefaults() = %+v, want it unchanged: %+v", got, explicit)
	}

	partial := ResourceBounds{HistogramMergeMaxCostUnits: 7}
	got := partial.withDefaults()
	if got.HistogramMergeMaxCostUnits != 7 {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the explicit 7 preserved", got.HistogramMergeMaxCostUnits)
	}
	if want := ClassicBucketWindowSeriesEntriesForMemory(0); got.ClassicBucketWindowSeriesMaxEntries != want {
		t.Fatalf("ClassicBucketWindowSeriesMaxEntries = %d, want the unset field derived as %d",
			got.ClassicBucketWindowSeriesMaxEntries, want)
	}
}

// TestEnvVarNames pins the exact CERBERUS_* spelling — a rename here is a
// breaking operator-facing change, so any future rename must go through an
// explicit test update, not an accidental drive-by.
func TestEnvVarNames(t *testing.T) {
	for _, tc := range []struct {
		got  string
		want string
	}{
		{EnvHistogramMergeMaxCostUnits, "CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS"},
		{EnvClassicBucketWindowSeriesMaxEntries, "CERBERUS_PROMQL_CLASSIC_BUCKET_WINDOW_SERIES_MAX_ENTRIES"},
		{EnvExpHistogramWindowMaxCostUnits, "CERBERUS_PROMQL_EXP_HISTOGRAM_WINDOW_MAX_COST_UNITS"},
	} {
		if tc.got != tc.want {
			t.Fatalf("env var name = %q, want %q", tc.got, tc.want)
		}
		if !strings.HasPrefix(tc.got, "CERBERUS_") {
			t.Fatalf("env var name %q must carry the CERBERUS_ prefix", tc.got)
		}
	}
}

// TestResourceBoundsFrom_RetiredClassicMergeSettingsIgnored pins the two
// settings of the removed classic-histogram bucket-merge ceiling: set to any
// value, even one that is not a valid ceiling, they leave the resolved bounds
// as they are with neither set, and each one set yields exactly one warning
// naming it.
func TestResourceBoundsFrom_RetiredClassicMergeSettingsIgnored(t *testing.T) {
	base, err := ResourceBoundsFrom(func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResourceBoundsFrom with neither setting: %v", err)
	}
	if w := RetiredSettingWarnings(func(string) string { return "" }); len(w) != 0 {
		t.Fatalf("neither setting set warns %q; want nothing", w)
	}
	for _, name := range []string{EnvClassicBucketMergeMaxEntriesRetired, EnvClassicBucketMergeMaxCostUnitsRetired} {
		for _, value := range []string{"1", "0", "-5", "not-a-number"} {
			get := func(key string) string {
				if key == name {
					return value
				}
				return ""
			}
			got, err := ResourceBoundsFrom(get)
			if err != nil || got != base {
				t.Fatalf("%s=%q: bounds %+v, err %v; want %+v, nil", name, value, got, err, base)
			}
			w := RetiredSettingWarnings(get)
			if len(w) != 1 || !strings.HasPrefix(w[0], name+" is ignored: ") {
				t.Fatalf("%s=%q: warnings %q; want one naming it", name, value, w)
			}
		}
	}
}
