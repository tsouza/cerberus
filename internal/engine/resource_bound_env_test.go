package engine

import (
	"strings"
	"testing"
)

// TestResourceBoundsFromEnv_DefaultsToZeroUnset pins the "operator did not
// override" sentinel: with none of the three CERBERUS_CH_*_MAX_ROWS vars
// set, every field resolves to 0 — the value applyResourceBoundOverrides
// treats as "thread nothing onto ctx, let chsql fall back to its own
// compiled-in default" (see ResourceBoundOverrides' own doc).
func TestResourceBoundsFromEnv_DefaultsToZeroUnset(t *testing.T) {
	t.Setenv(EnvRangeBucketFanoutMaxRows, "")
	t.Setenv(EnvRangeLWRFanoutMaxRows, "")
	t.Setenv(EnvRateWindowFanoutMaxRows, "")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	want := ResourceBoundOverrides{}
	if got != want {
		t.Fatalf("ResourceBoundsFromEnv() = %+v, want the zero value %+v", got, want)
	}
}

// TestResourceBoundsFromEnv_OverridesEachIndependently confirms each of the
// three env vars maps onto its own field, independent of the other two —
// the exact "an operator override actually reaches the resolved config"
// contract issue #2667 exists to establish.
func TestResourceBoundsFromEnv_OverridesEachIndependently(t *testing.T) {
	t.Setenv(EnvRangeBucketFanoutMaxRows, "123")
	t.Setenv(EnvRangeLWRFanoutMaxRows, "456")
	t.Setenv(EnvRateWindowFanoutMaxRows, "789")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	want := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows: 123,
		RangeLWRFanoutMaxRows:    456,
		RateWindowFanoutMaxRows:  789,
	}
	if got != want {
		t.Fatalf("ResourceBoundsFromEnv() = %+v, want %+v", got, want)
	}
}

// TestResourceBoundsFromEnv_RejectsMalformedValue confirms a typo'd env var
// fails startup with a wrapped, identifiable error rather than silently
// falling back to 0 (which would silently discard the operator's actual
// intent to override).
func TestResourceBoundsFromEnv_RejectsMalformedValue(t *testing.T) {
	for _, key := range []string{EnvRangeBucketFanoutMaxRows, EnvRangeLWRFanoutMaxRows, EnvRateWindowFanoutMaxRows} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(EnvRangeBucketFanoutMaxRows, "")
			t.Setenv(EnvRangeLWRFanoutMaxRows, "")
			t.Setenv(EnvRateWindowFanoutMaxRows, "")
			t.Setenv(key, "not-a-number")

			_, err := ResourceBoundsFromEnv()
			if err == nil {
				t.Fatalf("ResourceBoundsFromEnv() with %s=not-a-number: error = nil, want an error", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name the offending var %q", err.Error(), key)
			}
		})
	}
}

// TestResourceBoundsFromEnv_RejectsNonPositiveValue confirms 0 and a
// negative value are both rejected as startup errors rather than silently
// accepted as "0 rows" or "-1 rows" — a fanout row bound that low would
// reject every query outright, which is never a legitimate operator
// intent (see ResourceBoundOverrides' own doc).
func TestResourceBoundsFromEnv_RejectsNonPositiveValue(t *testing.T) {
	for _, v := range []string{"0", "-1", "-1000000"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvRangeBucketFanoutMaxRows, v)
			t.Setenv(EnvRangeLWRFanoutMaxRows, "")
			t.Setenv(EnvRateWindowFanoutMaxRows, "")

			_, err := ResourceBoundsFromEnv()
			if err == nil {
				t.Fatalf("ResourceBoundsFromEnv() with %s=%s: error = nil, want an error", EnvRangeBucketFanoutMaxRows, v)
			}
		})
	}
}

// TestEngine_ResourceBoundOverrides confirms the Engine method that feeds
// emitForHead / routeBExecCtx packages exactly the three Engine fields, so a
// production wiring bug (mixing up which field maps to which knob) would
// show up here rather than only via an end-to-end chDB query.
func TestEngine_ResourceBoundOverrides(t *testing.T) {
	e := &Engine{
		RangeBucketFanoutMaxRows: 111,
		RangeLWRFanoutMaxRows:    222,
		RateWindowFanoutMaxRows:  333,
	}
	got := e.resourceBoundOverrides()
	want := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows: 111,
		RangeLWRFanoutMaxRows:    222,
		RateWindowFanoutMaxRows:  333,
	}
	if got != want {
		t.Fatalf("resourceBoundOverrides() = %+v, want %+v", got, want)
	}
}

// TestEngine_ResourceBoundOverrides_ZeroValue confirms an Engine built
// without these fields wired (e.g. Engine{} in a test, or the Tempo head,
// which never lowers any of the three guarded node kinds) packages the
// zero ResourceBoundOverrides — the "thread nothing" sentinel.
func TestEngine_ResourceBoundOverrides_ZeroValue(t *testing.T) {
	e := &Engine{}
	got := e.resourceBoundOverrides()
	if got != (ResourceBoundOverrides{}) {
		t.Fatalf("resourceBoundOverrides() on a zero Engine = %+v, want the zero value", got)
	}
}
