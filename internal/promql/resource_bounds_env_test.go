package promql

import (
	"strings"
	"testing"
)

// TestResourceBoundsFromEnv_DefaultsUnset confirms that with neither
// CERBERUS_PROMQL_*_MAX_COST_UNITS var set, ResourceBoundsFromEnv returns
// exactly DefaultResourceBounds — the same maxHistogramMergeCostUnits /
// maxClassicBucketMergeCostUnits constants every lowering path enforced
// before cerberus issue #2667 added this override surface.
func TestResourceBoundsFromEnv_DefaultsUnset(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "")
	t.Setenv(EnvClassicBucketMergeMaxCostUnits, "")

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
	if got.ClassicBucketMergeMaxCostUnits != maxClassicBucketMergeCostUnits {
		t.Fatalf("ClassicBucketMergeMaxCostUnits = %d, want the maxClassicBucketMergeCostUnits constant %d",
			got.ClassicBucketMergeMaxCostUnits, maxClassicBucketMergeCostUnits)
	}
}

// TestResourceBoundsFromEnv_HistogramMergeOverride confirms
// CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS overrides
// HistogramMergeMaxCostUnits alone, leaving ClassicBucketMergeMaxCostUnits at
// its own default — the two knobs are independent (cerberus issue #2667
// covers two DISJOINT constants, not one shared ceiling).
func TestResourceBoundsFromEnv_HistogramMergeOverride(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "123456789")
	t.Setenv(EnvClassicBucketMergeMaxCostUnits, "")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	if got.HistogramMergeMaxCostUnits != 123456789 {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want 123456789 (the env override)", got.HistogramMergeMaxCostUnits)
	}
	if got.ClassicBucketMergeMaxCostUnits != maxClassicBucketMergeCostUnits {
		t.Fatalf("ClassicBucketMergeMaxCostUnits = %d, want the untouched default %d",
			got.ClassicBucketMergeMaxCostUnits, maxClassicBucketMergeCostUnits)
	}
}

// TestResourceBoundsFromEnv_ClassicBucketMergeOverride is the mirror of
// TestResourceBoundsFromEnv_HistogramMergeOverride for the other knob.
func TestResourceBoundsFromEnv_ClassicBucketMergeOverride(t *testing.T) {
	t.Setenv(EnvHistogramMergeMaxCostUnits, "")
	t.Setenv(EnvClassicBucketMergeMaxCostUnits, "42")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	if got.ClassicBucketMergeMaxCostUnits != 42 {
		t.Fatalf("ClassicBucketMergeMaxCostUnits = %d, want 42 (the env override)", got.ClassicBucketMergeMaxCostUnits)
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
		{"classic bucket merge", func(t *testing.T) { t.Setenv(EnvClassicBucketMergeMaxCostUnits, "not-a-number") }},
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
	for _, v := range []string{"0", "-1"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvHistogramMergeMaxCostUnits, v)
			t.Setenv(EnvClassicBucketMergeMaxCostUnits, v)
			if _, err := ResourceBoundsFromEnv(); err == nil {
				t.Fatalf("ResourceBoundsFromEnv() error = nil for value %q, want a non-positive rejection", v)
			}
		})
	}
}

// TestResourceBounds_WithDefaults confirms the zero value resolves each
// constant-defaulted field from DefaultResourceBounds, that the DERIVED
// field resolves from CHQueryMaxMemory instead (it is deliberately absent
// from DefaultResourceBounds, so the two are not field-by-field equal), and
// that a fully-populated value passes through unchanged — the resolution
// [Lower] / [LowerAt] / [LowerAtRange] / [LowerAtRangeOpts] /
// [LowerMetadataRange] apply at their own lowering-entry seam.
func TestResourceBounds_WithDefaults(t *testing.T) {
	zero := (ResourceBounds{}).withDefaults()
	if got, want := zero.HistogramMergeMaxCostUnits, int64(maxHistogramMergeCostUnits); got != want {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the shipped default %d", got, want)
	}
	if got, want := zero.ClassicBucketMergeMaxCostUnits, int64(maxClassicBucketMergeCostUnits); got != want {
		t.Fatalf("ClassicBucketMergeMaxCostUnits = %d, want the shipped default %d", got, want)
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
		HistogramMergeMaxCostUnits:     7,
		ClassicBucketMergeMaxCostUnits: 9,
		ExpHistogramWindowMaxCostUnits: 11,
		CHQueryMaxMemory:               64 * bytesPerGiB,
	}
	if got := explicit.withDefaults(); got != explicit {
		t.Fatalf("a fully-populated ResourceBounds.withDefaults() = %+v, want it unchanged: %+v", got, explicit)
	}

	partial := ResourceBounds{HistogramMergeMaxCostUnits: 7}
	got := partial.withDefaults()
	if got.HistogramMergeMaxCostUnits != 7 {
		t.Fatalf("HistogramMergeMaxCostUnits = %d, want the explicit 7 preserved", got.HistogramMergeMaxCostUnits)
	}
	if got.ClassicBucketMergeMaxCostUnits != maxClassicBucketMergeCostUnits {
		t.Fatalf("ClassicBucketMergeMaxCostUnits = %d, want the unset field filled with the default %d",
			got.ClassicBucketMergeMaxCostUnits, maxClassicBucketMergeCostUnits)
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
		{EnvClassicBucketMergeMaxCostUnits, "CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS"},
	} {
		if tc.got != tc.want {
			t.Fatalf("env var name = %q, want %q", tc.got, tc.want)
		}
		if !strings.HasPrefix(tc.got, "CERBERUS_") {
			t.Fatalf("env var name %q must carry the CERBERUS_ prefix", tc.got)
		}
	}
}
