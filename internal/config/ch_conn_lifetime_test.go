package config

import (
	"testing"
	"time"
)

// TestFromEnv_CHConnMaxLifetime_FollowsKeepAlive pins the premise that binds
// the two age-eviction defaults: ConnMaxLifetime is the backstop for detecting
// a dead peer, and TCP keepalive is the primary mechanism for exactly that. So
// the default follows whether keepalive is armed — an aggressive age-evict when
// nothing else can notice a dead peer, a relaxed one when keepalive already
// notices in ~25s.
//
// Collapsing the two values back into one, or decoupling the choice from
// CERBERUS_CH_KEEPALIVE_ENABLED, fails here.
func TestFromEnv_CHConnMaxLifetime_FollowsKeepAlive(t *testing.T) {
	cases := []struct {
		name      string
		keepAlive string
		want      time.Duration
	}{
		{"keepalive on (the default)", "true", defaultCHConnMaxLifetime},
		{"keepalive off", "false", defaultCHConnMaxLifetimeNoKeepAlive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_CONN_MAX_LIFETIME", "")
			t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", tc.keepAlive)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.ConnMaxLifetime != tc.want {
				t.Errorf("ConnMaxLifetime = %s; want %s", cfg.ClickHouse.ConnMaxLifetime, tc.want)
			}
		})
	}
}

// TestFromEnv_CHConnMaxLifetime_ExplicitWins confirms the operator override
// beats BOTH resolved defaults, so neither keepalive state can silently
// reinterpret an explicitly configured age bound.
func TestFromEnv_CHConnMaxLifetime_ExplicitWins(t *testing.T) {
	const explicit = 90 * time.Second
	for _, keepAlive := range []string{"true", "false"} {
		t.Run("keepalive="+keepAlive, func(t *testing.T) {
			t.Setenv("CERBERUS_CH_CONN_MAX_LIFETIME", explicit.String())
			t.Setenv("CERBERUS_CH_KEEPALIVE_ENABLED", keepAlive)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.ClickHouse.ConnMaxLifetime != explicit {
				t.Errorf("ConnMaxLifetime = %s; want the explicit %s", cfg.ClickHouse.ConnMaxLifetime, explicit)
			}
		})
	}
}

// TestCHConnMaxLifetimeDefaults_BoundedByDeadPeerWindow pins the arithmetic the
// two defaults rest on. Without keepalive, age eviction is the ONLY dead-peer
// detector, so its bound must not be looser than the detection window keepalive
// would have provided; with keepalive, the relaxed default must be strictly
// longer than the no-keepalive one or the split buys nothing.
func TestCHConnMaxLifetimeDefaults_BoundedByDeadPeerWindow(t *testing.T) {
	deadPeerWindow := defaultCHKeepAliveIdle + defaultCHKeepAliveInterval*time.Duration(defaultCHKeepAliveCount)
	if defaultCHConnMaxLifetimeNoKeepAlive < deadPeerWindow {
		t.Errorf("no-keepalive ConnMaxLifetime default %s is shorter than the %s keepalive dead-peer window it stands in for",
			defaultCHConnMaxLifetimeNoKeepAlive, deadPeerWindow)
	}
	if defaultCHConnMaxLifetime <= defaultCHConnMaxLifetimeNoKeepAlive {
		t.Errorf("keepalive-on default %s must be longer than the no-keepalive default %s; "+
			"collapsing them back into one value reinstates the churn the split removes",
			defaultCHConnMaxLifetime, defaultCHConnMaxLifetimeNoKeepAlive)
	}
}
