package config

import (
	"strings"
	"testing"
)

// TestFromEnv_QueryMaxSamples_Default confirms the budget defaults to
// Prometheus's --query.max-samples default (50,000,000) when
// CERBERUS_QUERY_MAX_SAMPLES is unset.
func TestFromEnv_QueryMaxSamples_Default(t *testing.T) {
	t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxQuerySamples != 50_000_000 {
		t.Errorf("MaxQuerySamples = %d; want 50000000 (Prometheus parity default)",
			cfg.ClickHouse.MaxQuerySamples)
	}
}

// TestFromEnv_QueryMaxSamples_Override confirms the env var threads
// through to chclient.Config, including the documented 0 = disabled
// opt-out.
func TestFromEnv_QueryMaxSamples_Override(t *testing.T) {
	cases := []struct {
		val  string
		want int64
	}{
		{"5000000", 5_000_000},
		{"0", 0},
		{"1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.MaxQuerySamples != tc.want {
				t.Errorf("MaxQuerySamples = %d; want %d", cfg.ClickHouse.MaxQuerySamples, tc.want)
			}
		})
	}
}

// TestFromEnv_QueryMaxSamples_Invalid confirms non-integer and negative
// values fail fast at startup rather than silently defaulting.
func TestFromEnv_QueryMaxSamples_Invalid(t *testing.T) {
	for _, val := range []string{"lots", "1.5", "-1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_MAX_SAMPLES", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_QUERY_MAX_SAMPLES") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
