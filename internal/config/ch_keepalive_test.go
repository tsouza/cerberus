package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_CHKeepAlive_Defaults pins the TCP-keepalive defaults: enabled,
// 10s idle, 5s interval, 3 probes (≈25s worst-case dead-peer detection). These
// are the ROOT-CAUSE fix for slow breaker recovery after a ClickHouse restart —
// the kernel tears down a half-open socket to a force-killed pod so the next
// query fails fast (broken-conn → retried + evicted) instead of blocking.
func TestFromEnv_CHKeepAlive_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", "")
	t.Setenv("CERBERUS_CH_KEEPALIVE_IDLE", "")
	t.Setenv("CERBERUS_CH_KEEPALIVE_INTERVAL", "")
	t.Setenv("CERBERUS_CH_KEEPALIVE_COUNT", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.ClickHouse.KeepAliveEnabled {
		t.Errorf("KeepAliveEnabled = false; want true (root-cause restart-recovery fix)")
	}
	if cfg.ClickHouse.KeepAliveIdle != 10*time.Second {
		t.Errorf("KeepAliveIdle = %s; want 10s", cfg.ClickHouse.KeepAliveIdle)
	}
	if cfg.ClickHouse.KeepAliveInterval != 5*time.Second {
		t.Errorf("KeepAliveInterval = %s; want 5s", cfg.ClickHouse.KeepAliveInterval)
	}
	if cfg.ClickHouse.KeepAliveProbes != 3 {
		t.Errorf("KeepAliveProbes = %d; want 3", cfg.ClickHouse.KeepAliveProbes)
	}
}

// TestFromEnv_CHKeepAlive_Overrides confirms the four env vars thread through
// to chclient.Config.
func TestFromEnv_CHKeepAlive_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", "true")
	t.Setenv("CERBERUS_CH_KEEPALIVE_IDLE", "30s")
	t.Setenv("CERBERUS_CH_KEEPALIVE_INTERVAL", "7s")
	t.Setenv("CERBERUS_CH_KEEPALIVE_COUNT", "5")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.ClickHouse.KeepAliveEnabled {
		t.Errorf("KeepAliveEnabled = false; want true")
	}
	if cfg.ClickHouse.KeepAliveIdle != 30*time.Second {
		t.Errorf("KeepAliveIdle = %s; want 30s", cfg.ClickHouse.KeepAliveIdle)
	}
	if cfg.ClickHouse.KeepAliveInterval != 7*time.Second {
		t.Errorf("KeepAliveInterval = %s; want 7s", cfg.ClickHouse.KeepAliveInterval)
	}
	if cfg.ClickHouse.KeepAliveProbes != 5 {
		t.Errorf("KeepAliveProbes = %d; want 5", cfg.ClickHouse.KeepAliveProbes)
	}
}

// TestFromEnv_CHKeepAlive_DisabledSkipsTimingValidation confirms that when
// keepalive is OFF the inert timing knobs are not gated — a degenerate idle /
// interval / count is tolerated because the dialer never arms keepalive.
func TestFromEnv_CHKeepAlive_DisabledSkipsTimingValidation(t *testing.T) {
	t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", "false")
	t.Setenv("CERBERUS_CH_KEEPALIVE_IDLE", "0s")
	t.Setenv("CERBERUS_CH_KEEPALIVE_INTERVAL", "0s")
	t.Setenv("CERBERUS_CH_KEEPALIVE_COUNT", "0")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v; disabled keepalive must tolerate inert timing knobs", err)
	}
	if cfg.ClickHouse.KeepAliveEnabled {
		t.Errorf("KeepAliveEnabled = true; want false")
	}
}

// TestFromEnv_CHKeepAlive_Invalid confirms unparseable values, and non-positive
// timing knobs WHILE ENABLED, fail fast naming the offending env var.
func TestFromEnv_CHKeepAlive_Invalid(t *testing.T) {
	cases := []struct {
		env, val string
	}{
		{"CERBERUS_CH_KEEPALIVE_ENABLED", "maybe"},
		{"CERBERUS_CH_KEEPALIVE_IDLE", "forever"},
		{"CERBERUS_CH_KEEPALIVE_IDLE", "0s"},
		{"CERBERUS_CH_KEEPALIVE_IDLE", "-1s"},
		{"CERBERUS_CH_KEEPALIVE_INTERVAL", "nope"},
		{"CERBERUS_CH_KEEPALIVE_INTERVAL", "0s"},
		{"CERBERUS_CH_KEEPALIVE_INTERVAL", "-5s"},
		{"CERBERUS_CH_KEEPALIVE_COUNT", "lots"},
		{"CERBERUS_CH_KEEPALIVE_COUNT", "0"},
		{"CERBERUS_CH_KEEPALIVE_COUNT", "-3"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			// Keepalive must be ENABLED for the timing knobs to be validated.
			t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", "true")
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
