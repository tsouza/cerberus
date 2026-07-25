package solver

import (
	"testing"
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

// TestConfigFromEnv_IgnoresRetiredAutotuneKeys pins the graceful-retirement
// contract: a deployment manifest that still sets the removed autotune knobs
// must keep starting up unchanged — ConfigFromEnv reads neither key at all
// (there is no Config field left for them to populate), so setting them has
// no effect on the resolved config. This test must FAIL if ConfigFromEnv
// (or Config) ever grows a field these keys populate again without also
// removing them from RetiredEnvKeys.
func TestConfigFromEnv_IgnoresRetiredAutotuneKeys(t *testing.T) {
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE", "false")
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE_INTERVAL", "30m")

	got, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}

	t.Setenv("CERBERUS_SOLVER_AUTOTUNE", "")
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE_INTERVAL", "")
	want, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}

	if got != want {
		t.Fatalf("setting the retired autotune keys changed the resolved Config: got %+v, want %+v", got, want)
	}
}

// TestStillSetRetiredEnvKeysDetectsAndClearsBoth pins the discoverability
// half: an operator who still sets either retired key gets it named, and a
// clean environment reports none.
func TestStillSetRetiredEnvKeysDetectsAndClearsBoth(t *testing.T) {
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE", "true")
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE_INTERVAL", "")

	got := StillSetRetiredEnvKeys()
	if len(got) != 1 || got[0] != "CERBERUS_SOLVER_AUTOTUNE" {
		t.Fatalf("StillSetRetiredEnvKeys() = %v, want [CERBERUS_SOLVER_AUTOTUNE]", got)
	}

	t.Setenv("CERBERUS_SOLVER_AUTOTUNE", "")
	if got := StillSetRetiredEnvKeys(); len(got) != 0 {
		t.Fatalf("StillSetRetiredEnvKeys() = %v, want none once both are unset", got)
	}
}
