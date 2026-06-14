package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_CHPool_Defaults pins the explicit pool defaults (#81).
// MaxOpenConns / MaxIdleConns reproduce clickhouse-go/v2's implicit values
// (10 / 5) so the non-sharded path stays behaviour-compatible. ConnMaxLifetime
// DEPARTS from the driver's 1h default: it is 5m, the backstop that bounds how
// long a stale pooled conn to a restarted backend can loiter (ch-pod-kill
// recovery, run 27509796946).
func TestFromEnv_CHPool_Defaults(t *testing.T) {
	t.Setenv("CERBERUS_CH_MAX_OPEN_CONNS", "")
	t.Setenv("CERBERUS_CH_MAX_IDLE_CONNS", "")
	t.Setenv("CERBERUS_CH_CONN_MAX_LIFETIME", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxOpenConns != 10 {
		t.Errorf("MaxOpenConns = %d; want 10 (clickhouse-go implicit default)", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 5 {
		t.Errorf("MaxIdleConns = %d; want 5 (clickhouse-go implicit default)", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("ConnMaxLifetime = %s; want 5m (restart-recovery backstop, not the driver's 1h)", cfg.ClickHouse.ConnMaxLifetime)
	}
}

// TestFromEnv_CHPool_Overrides confirms the env vars thread through to
// chclient.Config — the knob the solver raises for fan-out.
func TestFromEnv_CHPool_Overrides(t *testing.T) {
	t.Setenv("CERBERUS_CH_MAX_OPEN_CONNS", "64")
	t.Setenv("CERBERUS_CH_MAX_IDLE_CONNS", "32")
	t.Setenv("CERBERUS_CH_CONN_MAX_LIFETIME", "30m")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.ClickHouse.MaxOpenConns != 64 {
		t.Errorf("MaxOpenConns = %d; want 64", cfg.ClickHouse.MaxOpenConns)
	}
	if cfg.ClickHouse.MaxIdleConns != 32 {
		t.Errorf("MaxIdleConns = %d; want 32", cfg.ClickHouse.MaxIdleConns)
	}
	if cfg.ClickHouse.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("ConnMaxLifetime = %s; want 30m", cfg.ClickHouse.ConnMaxLifetime)
	}
}

// TestFromEnv_CHPool_Invalid confirms unparseable and non-positive
// values fail fast at startup, naming the offending env var, rather than
// silently producing a degenerate pool.
func TestFromEnv_CHPool_Invalid(t *testing.T) {
	cases := []struct {
		env, val string
	}{
		{"CERBERUS_CH_MAX_OPEN_CONNS", "lots"},
		{"CERBERUS_CH_MAX_OPEN_CONNS", "0"},
		{"CERBERUS_CH_MAX_OPEN_CONNS", "-1"},
		{"CERBERUS_CH_MAX_IDLE_CONNS", "nope"},
		{"CERBERUS_CH_MAX_IDLE_CONNS", "0"},
		{"CERBERUS_CH_MAX_IDLE_CONNS", "-4"},
		{"CERBERUS_CH_CONN_MAX_LIFETIME", "forever"},
		{"CERBERUS_CH_CONN_MAX_LIFETIME", "0s"},
		{"CERBERUS_CH_CONN_MAX_LIFETIME", "-1h"},
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
