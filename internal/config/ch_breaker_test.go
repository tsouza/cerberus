package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_CHBreaker_Defaults pins the breaker defaults (#95) to the
// previously-hardcoded constants in internal/chclient/breaker.go, so
// out-of-the-box behaviour is byte-unchanged: threshold 5, window 10s,
// open-interval 5s, enabled (Disabled=false).
func TestFromEnv_CHBreaker_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_CH_BREAKER_ENABLED", "")
	t.Setenv("CERBERUS_CH_BREAKER_THRESHOLD", "")
	t.Setenv("CERBERUS_CH_BREAKER_WINDOW", "")
	t.Setenv("CERBERUS_CH_BREAKER_OPEN_INTERVAL", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.BreakerDisabled {
		t.Error("BreakerDisabled = true; want false (breaker enabled by default)")
	}
	if cfg.ClickHouse.BreakerThreshold != 5 {
		t.Errorf("BreakerThreshold = %d; want 5 (GA default)", cfg.ClickHouse.BreakerThreshold)
	}
	if cfg.ClickHouse.BreakerWindow != 10*time.Second {
		t.Errorf("BreakerWindow = %s; want 10s (GA default)", cfg.ClickHouse.BreakerWindow)
	}
	if cfg.ClickHouse.BreakerOpenInterval != 5*time.Second {
		t.Errorf("BreakerOpenInterval = %s; want 5s (GA default)", cfg.ClickHouse.BreakerOpenInterval)
	}
}

// TestFromEnv_CHBreaker_Overrides confirms the env vars thread through to
// chclient.Config.
func TestFromEnv_CHBreaker_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_CH_BREAKER_THRESHOLD", "8")
	t.Setenv("CERBERUS_CH_BREAKER_WINDOW", "30s")
	t.Setenv("CERBERUS_CH_BREAKER_OPEN_INTERVAL", "2s")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.BreakerDisabled {
		t.Error("BreakerDisabled = true; want false")
	}
	if cfg.ClickHouse.BreakerThreshold != 8 {
		t.Errorf("BreakerThreshold = %d; want 8", cfg.ClickHouse.BreakerThreshold)
	}
	if cfg.ClickHouse.BreakerWindow != 30*time.Second {
		t.Errorf("BreakerWindow = %s; want 30s", cfg.ClickHouse.BreakerWindow)
	}
	if cfg.ClickHouse.BreakerOpenInterval != 2*time.Second {
		t.Errorf("BreakerOpenInterval = %s; want 2s", cfg.ClickHouse.BreakerOpenInterval)
	}
}

// TestFromEnv_CHBreaker_Disabled confirms CERBERUS_CH_BREAKER_ENABLED=false
// flips Disabled true. The threshold/window/interval knobs still parse and
// validate, but the breaker is a no-op at runtime.
func TestFromEnv_CHBreaker_Disabled(t *testing.T) {
	t.Setenv("CERBERUS_CH_BREAKER_ENABLED", "false")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.ClickHouse.BreakerDisabled {
		t.Error("BreakerDisabled = false; want true (CERBERUS_CH_BREAKER_ENABLED=false)")
	}
}

// TestFromEnv_CHBreaker_Invalid confirms unparseable and out-of-range
// values fail fast at startup, naming the offending env var.
func TestFromEnv_CHBreaker_Invalid(t *testing.T) {
	cases := []struct {
		env, val string
	}{
		{"CERBERUS_CH_BREAKER_ENABLED", "maybe"},
		{"CERBERUS_CH_BREAKER_THRESHOLD", "lots"},
		{"CERBERUS_CH_BREAKER_THRESHOLD", "0"},
		{"CERBERUS_CH_BREAKER_THRESHOLD", "-1"},
		{"CERBERUS_CH_BREAKER_WINDOW", "forever"},
		{"CERBERUS_CH_BREAKER_WINDOW", "0s"},
		{"CERBERUS_CH_BREAKER_WINDOW", "-1s"},
		{"CERBERUS_CH_BREAKER_OPEN_INTERVAL", "soon"},
		{"CERBERUS_CH_BREAKER_OPEN_INTERVAL", "0s"},
		{"CERBERUS_CH_BREAKER_OPEN_INTERVAL", "-5s"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%q; want error", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name the env var %s", err, tc.env)
			}
		})
	}
}
