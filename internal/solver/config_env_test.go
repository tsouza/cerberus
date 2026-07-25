package solver

import (
	"testing"
	"time"
)

// TestConfigFromEnv_DefaultsToAuto pins the phase-2 flip: with
// CERBERUS_EVAL_ROUTE unset, the production env path routes in "auto" mode (the
// library DefaultConfig stays dark "single", but ConfigFromEnv flips it). The
// resolved config must still pass Validate.
func TestConfigFromEnv_DefaultsToAuto(t *testing.T) {
	t.Setenv(EnvRoute, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.Mode != ModeAuto {
		t.Fatalf("default Mode = %q, want %q (phase-2 flip)", cfg.Mode, ModeAuto)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("auto config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_SinglePins confirms an operator can still pin the dark
// "single" mode to disable routing. Case-insensitive (the env path lowercases).
func TestConfigFromEnv_SinglePins(t *testing.T) {
	for _, v := range []string{"single", "SINGLE", "Single"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(EnvRoute, v)
			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("ConfigFromEnv() error = %v", err)
			}
			if cfg.Mode != ModeSingle {
				t.Fatalf("Mode = %q, want %q", cfg.Mode, ModeSingle)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("single config failed Validate: %v", err)
			}
		})
	}
}

// TestConfigFromEnv_ShardedForce confirms the forced-route value the
// compatibility/prometheus-forced-route CI job sets (CERBERUS_EVAL_ROUTE=sharded)
// resolves to ModeSharded and validates.
func TestConfigFromEnv_ShardedForce(t *testing.T) {
	t.Setenv(EnvRoute, "sharded")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.Mode != ModeSharded {
		t.Fatalf("Mode = %q, want %q", cfg.Mode, ModeSharded)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sharded config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_AutotuneDefaultsOn pins the headline default: the
// self-driving loop is ENABLED unless an operator opts out, with the default
// cadence. The resolved config must validate.
func TestConfigFromEnv_AutotuneDefaultsOn(t *testing.T) {
	t.Setenv(EnvAutotune, "")
	t.Setenv(EnvAutotuneInterval, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if !cfg.Autotune {
		t.Errorf("Autotune = false, want true (default-on)")
	}
	if cfg.AutotuneInterval != defaultAutotuneInterval {
		t.Errorf("AutotuneInterval = %s, want %s", cfg.AutotuneInterval, defaultAutotuneInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("autotune config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_AutotuneDisable confirms the kill-switch: setting
// CERBERUS_SOLVER_AUTOTUNE=false pins the thresholds (fixed-threshold build) and
// a custom interval parses.
func TestConfigFromEnv_AutotuneDisable(t *testing.T) {
	t.Setenv(EnvAutotune, "false")
	t.Setenv(EnvAutotuneInterval, "30m")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.Autotune {
		t.Errorf("Autotune = true, want false (kill-switch)")
	}
	if cfg.AutotuneInterval != 30*time.Minute {
		t.Errorf("AutotuneInterval = %s, want 30m", cfg.AutotuneInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled-autotune config failed Validate: %v", err)
	}
}
