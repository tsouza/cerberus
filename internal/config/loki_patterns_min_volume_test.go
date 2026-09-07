package config

import "testing"

// TestFromEnv_LokiPatternsMinVolume pins CERBERUS_LOKI_PATTERNS_MIN_VOLUME:
// default 30 (matches upstream Loki's minClusterSize, cerberus issue #2081),
// an explicit override, the `0` = "no floor" escape hatch (cerberus's
// pre-#2205 behaviour), and rejection of a negative value.
func TestFromEnv_LokiPatternsMinVolume(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		clearAllEnv(t)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.LokiPatternsMinVolume != 30 {
			t.Errorf("LokiPatternsMinVolume = %d; want 30", cfg.LokiPatternsMinVolume)
		}
	})
	t.Run("override", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envLokiPatternsMinVolume, "5")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.LokiPatternsMinVolume != 5 {
			t.Errorf("LokiPatternsMinVolume = %d; want 5", cfg.LokiPatternsMinVolume)
		}
	})
	t.Run("zero disables the floor", func(t *testing.T) {
		clearAllEnv(t)
		t.Setenv(envLokiPatternsMinVolume, "0")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.LokiPatternsMinVolume != 0 {
			t.Errorf("LokiPatternsMinVolume = %d; want 0 (explicit, not coerced back to the default)", cfg.LokiPatternsMinVolume)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, val := range []string{"-1", "nope"} {
			clearAllEnv(t)
			t.Setenv(envLokiPatternsMinVolume, val)
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv accepted %s=%q; want error", envLokiPatternsMinVolume, val)
			}
		}
	})
}
