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

// TestConfigFromEnv_RetiredKnobsIgnored pins the graceful-retirement contract
// for the removed threshold-autotune knobs. ConfigFromEnv reads only the keys it
// recognises, so a deployment whose manifest still carries a retired
// CERBERUS_* var must boot on the configured defaults rather than failing
// startup on an unrecognised key.
func TestConfigFromEnv_RetiredKnobsIgnored(t *testing.T) {
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE", "true")
	t.Setenv("CERBERUS_SOLVER_AUTOTUNE_INTERVAL", "30m")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() with retired knobs set: error = %v, want nil", err)
	}
	if cfg.MinFanout != defaultMinFanout {
		t.Errorf("MinFanout = %d, want the configured default %d", cfg.MinFanout, defaultMinFanout)
	}
	if cfg.MinAnchorPairs != defaultMinAnchorPairs {
		t.Errorf("MinAnchorPairs = %d, want the configured default %d", cfg.MinAnchorPairs, defaultMinAnchorPairs)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config resolved alongside retired knobs failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoDefaultsOff pins the OPPOSITE default from
// Autotune: the failure-driven route memo is a new runtime-behavior-
// changing feature, so — matching CERBERUS_CH_OPT_CORPUS_ENABLED and
// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE's precedent — it stays off unless an
// operator explicitly opts in. An unset env var (a routine upgrade,
// deploying this binary onto an existing config with no manifest change)
// must resolve to disabled, not enabled.
func TestConfigFromEnv_RouteMemoDefaultsOff(t *testing.T) {
	t.Setenv(EnvRouteMemoEnabled, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoEnabled {
		t.Errorf("RouteMemoEnabled = true, want false (default-off)")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoExplicitOn confirms the opt-in path.
func TestConfigFromEnv_RouteMemoExplicitOn(t *testing.T) {
	t.Setenv(EnvRouteMemoEnabled, "true")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if !cfg.RouteMemoEnabled {
		t.Errorf("RouteMemoEnabled = false, want true (explicit opt-in)")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("route-memo-enabled config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoExplicitOff confirms an explicit "false" is
// indistinguishable in effect from leaving the var unset (both resolve to
// disabled) — the option to be explicit about the default must not itself
// change behaviour.
func TestConfigFromEnv_RouteMemoExplicitOff(t *testing.T) {
	t.Setenv(EnvRouteMemoEnabled, "false")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoEnabled {
		t.Errorf("RouteMemoEnabled = true, want false (explicit opt-out)")
	}
}

// TestConfigFromEnv_RouteMemoEntryTTLDefaultsUnset pins the zero-value
// contract: an unset CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL must resolve to
// Config's Go zero value (0), never a duplicated copy of the routememo
// package's own default duration — the routememo package alone owns that
// default; Config only carries an override.
func TestConfigFromEnv_RouteMemoEntryTTLDefaultsUnset(t *testing.T) {
	t.Setenv(EnvRouteMemoEntryTTL, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoEntryTTL != 0 {
		t.Errorf("RouteMemoEntryTTL = %s, want 0 (unset means routememo package default)", cfg.RouteMemoEntryTTL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoEntryTTLExplicitSet confirms an operator-supplied
// duration parses through to Config unchanged.
func TestConfigFromEnv_RouteMemoEntryTTLExplicitSet(t *testing.T) {
	t.Setenv(EnvRouteMemoEntryTTL, "10m")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoEntryTTL != 10*time.Minute {
		t.Errorf("RouteMemoEntryTTL = %s, want 10m", cfg.RouteMemoEntryTTL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit route-memo-entry-ttl config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoReValidationFractionDefaultsUnset mirrors
// TestConfigFromEnv_RouteMemoEntryTTLDefaultsUnset for the fraction knob: an
// unset env var must resolve to Config's Go zero value (0), not a duplicated
// copy of the routememo package's own default divisor.
func TestConfigFromEnv_RouteMemoReValidationFractionDefaultsUnset(t *testing.T) {
	t.Setenv(EnvRouteMemoRevalFrac, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoReValidationFraction != 0 {
		t.Errorf("RouteMemoReValidationFraction = %d, want 0 (unset means routememo package default)", cfg.RouteMemoReValidationFraction)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_RouteMemoReValidationFractionExplicitSet confirms an
// operator-supplied divisor parses through to Config unchanged.
func TestConfigFromEnv_RouteMemoReValidationFractionExplicitSet(t *testing.T) {
	t.Setenv(EnvRouteMemoRevalFrac, "4")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.RouteMemoReValidationFraction != 4 {
		t.Errorf("RouteMemoReValidationFraction = %d, want 4", cfg.RouteMemoReValidationFraction)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit route-memo-revalidation-fraction config failed Validate: %v", err)
	}
}
