package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_PromMetadataLookback_Default confirms the knob defaults to
// zero. Zero is not "no bound" — it is "the operator has not stated a
// retention", which hands the windowless metadata-discovery horizon to
// internal/api/prom's own fallback. A non-zero default here would duplicate
// that fallback in a second place and drift from it.
func TestFromEnv_PromMetadataLookback_Default(t *testing.T) {
	t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.PromMetadataLookback != 0 {
		t.Errorf("PromMetadataLookback = %s; want 0 (defer to the handler fallback)", cfg.PromMetadataLookback)
	}
}

// TestFromEnv_PromMetadataLookback_Override confirms the env value threads
// through to Config verbatim, including the day-scale retention this knob
// exists to carry (written in hours — the parser is Go duration syntax).
func TestFromEnv_PromMetadataLookback_Override(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"2160h", 2160 * time.Hour},
		{"336h", 336 * time.Hour},
		{"90m", 90 * time.Minute},
		{"0s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.PromMetadataLookback != tc.want {
				t.Errorf("PromMetadataLookback = %s; want %s", cfg.PromMetadataLookback, tc.want)
			}
		})
	}
}

// TestFromEnv_PromMetadataLookback_Invalid confirms a non-duration or
// NEGATIVE value fails fast at startup. A negative lookback would put the
// derived window's start AFTER "now", so every windowless discovery request
// would return an empty label / metric set — a total silent-drop that looks
// exactly like an empty database rather than like a misconfiguration.
func TestFromEnv_PromMetadataLookback_Invalid(t *testing.T) {
	for _, val := range []string{"forever", "-1h", "-1s", "14 days"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_PROM_METADATA_LOOKBACK") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
