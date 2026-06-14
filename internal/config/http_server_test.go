package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_HTTPServer_Defaults pins the streaming-safe HTTP server
// timeouts: header timeout is the promoted 5s, read/write are 0 (unlimited)
// so /tail and long matrices stream uninterrupted, idle is 120s.
func TestFromEnv_HTTPServer_Defaults(t *testing.T) {
	clearAllEnv(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	hs := cfg.HTTPServer
	if hs.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v; want 5s (promoted)", hs.ReadHeaderTimeout)
	}
	if hs.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v; want 0 (streaming-safe)", hs.ReadTimeout)
	}
	if hs.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v; want 0 (streaming-safe)", hs.WriteTimeout)
	}
	if hs.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v; want 120s", hs.IdleTimeout)
	}
	if hs.MaxHeaderBytes != 0 {
		t.Errorf("MaxHeaderBytes = %d; want 0 (Go default)", hs.MaxHeaderBytes)
	}
}

// TestFromEnv_HTTPServer_Overrides confirms every knob threads through.
func TestFromEnv_HTTPServer_Overrides(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(envHTTPReadTimeout, "30s")
	t.Setenv(envHTTPReadHdrTimeout, "3s")
	t.Setenv(envHTTPWriteTimeout, "45s")
	t.Setenv(envHTTPIdleTimeout, "60s")
	t.Setenv(envHTTPMaxHeaderBytes, "1048576")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	hs := cfg.HTTPServer
	if hs.ReadTimeout != 30*time.Second || hs.ReadHeaderTimeout != 3*time.Second ||
		hs.WriteTimeout != 45*time.Second || hs.IdleTimeout != 60*time.Second ||
		hs.MaxHeaderBytes != 1048576 {
		t.Errorf("HTTPServer = %+v; overrides not applied", hs)
	}
}

// TestFromEnv_HTTPServer_Invalid rejects negative durations / sizes.
func TestFromEnv_HTTPServer_Invalid(t *testing.T) {
	cases := []struct{ env, val string }{
		{envHTTPReadTimeout, "-1s"},
		{envHTTPReadHdrTimeout, "-1s"},
		{envHTTPWriteTimeout, "-1s"},
		{envHTTPIdleTimeout, "-1s"},
		{envHTTPMaxHeaderBytes, "-1"},
		{envHTTPReadTimeout, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			clearAllEnv(t)
			t.Setenv(tc.env, tc.val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %s=%q; want error", tc.env, tc.val)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

// TestFromEnv_LokiTailWriteTimeout covers the promoted /tail write timeout.
func TestFromEnv_LokiTailWriteTimeout(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		clearAllEnv(t)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.LokiTailWriteTimeout != 10*time.Second {
			t.Errorf("LokiTailWriteTimeout = %v; want 10s", cfg.LokiTailWriteTimeout)
		}
	})
	t.Run("override", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envLokiTailWriteTO, "25s")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.LokiTailWriteTimeout != 25*time.Second {
			t.Errorf("LokiTailWriteTimeout = %v; want 25s", cfg.LokiTailWriteTimeout)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, val := range []string{"0s", "-5s", "nope"} {
			clearAllEnv(t)
			t.Setenv(envLokiTailWriteTO, val)
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv accepted %s=%q; want error", envLokiTailWriteTO, val)
			}
		}
	})
}
