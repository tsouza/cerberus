package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_QueryTimeout_Default confirms the per-query wall-clock cap
// defaults to 2 minutes when CERBERUS_QUERY_TIMEOUT is unset — mirroring
// upstream Prometheus's --query.timeout default so Grafana / Prom clients
// see the budget they already expect.
func TestFromEnv_QueryTimeout_Default(t *testing.T) {
	t.Setenv("CERBERUS_QUERY_TIMEOUT", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.QueryTimeout != 2*time.Minute {
		t.Errorf("QueryTimeout = %s; want 2m (Prometheus default)", cfg.ClickHouse.QueryTimeout)
	}
}

// TestFromEnv_QueryTimeout_Override confirms the env var threads through
// to chclient.Config, including the documented 0 = disabled opt-out and
// sub-minute / sub-second durations.
func TestFromEnv_QueryTimeout_Override(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"2m", 2 * time.Minute},
		{"30s", 30 * time.Second},
		{"500ms", 500 * time.Millisecond},
		{"5m", 5 * time.Minute},
		{"0s", 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_TIMEOUT", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.QueryTimeout != tc.want {
				t.Errorf("QueryTimeout = %s; want %s", cfg.ClickHouse.QueryTimeout, tc.want)
			}
		})
	}
}

// TestFromEnv_QueryTimeout_Invalid confirms non-duration and negative
// values fail fast at startup rather than silently defaulting.
func TestFromEnv_QueryTimeout_Invalid(t *testing.T) {
	for _, val := range []string{"forever", "-1s", "2 minutes"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_QUERY_TIMEOUT", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_QUERY_TIMEOUT") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}
