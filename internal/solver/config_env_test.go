package solver

import (
	"os"
	"strings"
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

// TestConfigFromEnv_AdaptiveDefaultsOn pins the default that makes ModeAuto
// mean "start on route A and escalate on real evidence": with neither the new
// nor the legacy env var set, the failure-driven route memo is ON. It is the
// only half of the routing decision that reacts to what actually happened, and
// it can only ever turn a route-A failure into a slower answer — so unlike
// CERBERUS_CH_OPT_CORPUS_ENABLED, the default-off convention does not apply.
func TestConfigFromEnv_AdaptiveDefaultsOn(t *testing.T) {
	t.Setenv(EnvAdaptiveEnabled, "")
	t.Setenv(EnvLegacyRouteMemoEnabled, "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if !cfg.AdaptiveEnabled {
		t.Errorf("AdaptiveEnabled = false, want true (default-on: it is the only half of the routing decision that reacts to what happened)")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config failed Validate: %v", err)
	}
}

// TestConfigFromEnv_AdaptiveExplicitOff pins that an operator can still turn it
// off — and that doing so is honoured, not silently overridden by the default.
func TestConfigFromEnv_AdaptiveExplicitOff(t *testing.T) {
	t.Setenv(EnvAdaptiveEnabled, "false")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.AdaptiveEnabled {
		t.Errorf("AdaptiveEnabled = true, want false (explicit opt-out must win over the default)")
	}
}

// TestConfigFromEnv_LegacyRouteMemoAliasStillApplies pins the soft deprecation:
// an operator who set the OLD name to false on a previous version must not have
// the feature silently re-enabled by an upgrade that merely renamed the knob.
func TestConfigFromEnv_LegacyRouteMemoAliasStillApplies(t *testing.T) {
	t.Setenv(EnvLegacyRouteMemoEnabled, "false")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if cfg.AdaptiveEnabled {
		t.Errorf("AdaptiveEnabled = true; the deprecated %s=false must still disable it", EnvLegacyRouteMemoEnabled)
	}
}

// TestConfigFromEnv_NewNameWinsOverLegacy pins the precedence when both are set.
func TestConfigFromEnv_NewNameWinsOverLegacy(t *testing.T) {
	t.Setenv(EnvLegacyRouteMemoEnabled, "false")
	t.Setenv(EnvAdaptiveEnabled, "true")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}
	if !cfg.AdaptiveEnabled {
		t.Errorf("AdaptiveEnabled = false; the new name must win over the deprecated alias")
	}
}

// TestDeprecatedEnvWarnings_FiresOnLegacyName pins that the deprecation is
// ANNOUNCED, not silent. A rename nobody is told about is a rename that rots.
func TestDeprecatedEnvWarnings_FiresOnLegacyName(t *testing.T) {
	t.Setenv(EnvLegacyRouteMemoEnabled, "true")

	warns := DeprecatedEnvWarnings()
	if len(warns) == 0 {
		t.Fatal("no deprecation warning for the legacy route-memo env var")
	}
	if !strings.Contains(warns[0], EnvAdaptiveEnabled) {
		t.Errorf("warning %q does not name the replacement %s", warns[0], EnvAdaptiveEnabled)
	}
}

// TestDeprecatedEnvWarnings_SilentWhenUnset: no warning when nobody set it.
// The var is explicitly UNSET rather than left to the ambient environment —
// DeprecatedEnvWarnings keys off LookupEnv, so a developer shell that happens
// to export the legacy name would otherwise decide this test's outcome.
// t.Setenv registers the restore; Unsetenv then removes the key entirely.
func TestDeprecatedEnvWarnings_SilentWhenUnset(t *testing.T) {
	t.Setenv(EnvLegacyRouteMemoEnabled, "")
	if err := os.Unsetenv(EnvLegacyRouteMemoEnabled); err != nil {
		t.Fatalf("unset %s: %v", EnvLegacyRouteMemoEnabled, err)
	}
	if w := DeprecatedEnvWarnings(); len(w) != 0 {
		t.Errorf("unexpected deprecation warnings with nothing set: %v", w)
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

// TestConfigFromEnv_EveryKnobReachesItsOwnField (cerberus issue #2991) pins
// the whole env-to-field ladder at once. ConfigFromEnv threads thirteen
// knobs through a repetitive `if cfg.X, err = envT(EnvX, cfg.X); err != nil`
// chain, and every line of it is one copy-paste away from being silently
// wrong in a way the compiler cannot see: the knobs share only four
// underlying types (int, int64, bool, time.Duration), so pairing EnvMaxK
// with cfg.MaxKWithEstimate — or reading one knob into two fields and
// leaving a third at its default — builds and runs.
//
// Every var is therefore set to a value distinct from every other AND from
// its own default, and every field is checked. A crossed pair lands another
// knob's distinct value; a dropped knob leaves the default. Both read as a
// named mismatch rather than as a coincidence.
func TestConfigFromEnv_EveryKnobReachesItsOwnField(t *testing.T) {
	t.Setenv(EnvRoute, ModeSharded)
	t.Setenv(EnvMinFanout, "11")
	t.Setenv(EnvMinAnchorPairs, "12")
	t.Setenv(EnvMaxK, "13")
	t.Setenv(EnvMinAnchorsPerSlice, "14")
	t.Setenv(EnvParallel, "15")
	t.Setenv(EnvTimeout, "16s")
	t.Setenv(EnvMaxOutputRows, "17")
	t.Setenv(EnvAdaptiveEnabled, "false")
	t.Setenv(EnvRouteMemoEntryTTL, "18m")
	t.Setenv(EnvRouteMemoRevalFrac, "19")
	t.Setenv(EnvEstimateNearEmptyRowFloor, "20")
	t.Setenv(EnvMaxKWithEstimate, "21")
	t.Setenv(EnvEstimateMinRowsPerAdditionalShard, "22")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v", err)
	}

	def := DefaultConfig()
	checks := []struct {
		env  string
		got  any
		want any
		def  any
	}{
		{EnvRoute, cfg.Mode, ModeSharded, def.Mode},
		{EnvMinFanout, cfg.MinFanout, 11, def.MinFanout},
		{EnvMinAnchorPairs, cfg.MinAnchorPairs, 12, def.MinAnchorPairs},
		{EnvMaxK, cfg.MaxK, 13, def.MaxK},
		{EnvMinAnchorsPerSlice, cfg.MinAnchorsPerSlice, 14, def.MinAnchorsPerSlice},
		{EnvParallel, cfg.Parallel, 15, def.Parallel},
		{EnvTimeout, cfg.Timeout, 16 * time.Second, def.Timeout},
		{EnvMaxOutputRows, cfg.MaxOutputRows, int64(17), def.MaxOutputRows},
		{EnvAdaptiveEnabled, cfg.AdaptiveEnabled, false, def.AdaptiveEnabled},
		{EnvRouteMemoEntryTTL, cfg.RouteMemoEntryTTL, 18 * time.Minute, def.RouteMemoEntryTTL},
		{EnvRouteMemoRevalFrac, cfg.RouteMemoReValidationFraction, 19, def.RouteMemoReValidationFraction},
		{EnvEstimateNearEmptyRowFloor, cfg.EstimateNearEmptyRowFloor, int64(20), def.EstimateNearEmptyRowFloor},
		{EnvMaxKWithEstimate, cfg.MaxKWithEstimate, 21, def.MaxKWithEstimate},
		{EnvEstimateMinRowsPerAdditionalShard, cfg.EstimateMinRowsPerAdditionalShard, int64(22), def.EstimateMinRowsPerAdditionalShard},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: field = %v; want %v", c.env, c.got, c.want)
		}
		// The whole design of this test is that "still at the default" is
		// distinguishable from "took the env value". Assert that premise
		// rather than trusting it: a knob whose chosen value happens to
		// equal its own default proves nothing about the wiring.
		if c.want == c.def {
			t.Errorf("%s: chosen value %v equals the default; pick a different one so this test can discriminate", c.env, c.want)
		}
	}
}

// TestConfigFromEnv_MalformedKnobFailsFast pins the parse-vs-silent-default
// contract ConfigFromEnv's doc states: "A parse failure on any knob is
// returned so a typo never silently routes (or never silently disables
// routing)." A knob that swallowed its parse error would boot the gateway on
// a threshold the operator never chose — a wrong routing decision on every
// subsequent query, with nothing in the logs. Each knob is checked
// separately, because the ladder returns on the FIRST error and a knob whose
// error arm was dropped would otherwise be masked by an earlier one.
//
// The returned error must name the offending variable: startup failure text
// is the only thing the operator has to go on.
func TestConfigFromEnv_MalformedKnobFailsFast(t *testing.T) {
	// Values are malformed for the knob's own type, not merely out of range
	// — range is Validate's job, which ConfigFromEnv deliberately does not run.
	cases := []struct{ env, bad string }{
		{EnvMinFanout, "three"},
		{EnvMinAnchorPairs, "3.5"},
		{EnvMaxK, "0x10"},
		{EnvMinAnchorsPerSlice, "1,2"},
		{EnvParallel, "many"},
		{EnvTimeout, "16 seconds"},
		{EnvMaxOutputRows, "1e6"},
		{EnvAdaptiveEnabled, "yes-please"},
		{EnvLegacyRouteMemoEnabled, "sometimes"},
		{EnvRouteMemoEntryTTL, "18 minutes"},
		{EnvRouteMemoRevalFrac, "half"},
		{EnvEstimateNearEmptyRowFloor, "lots"},
		{EnvMaxKWithEstimate, "some"},
		{EnvEstimateMinRowsPerAdditionalShard, "9_000"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.bad)
			cfg, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv() with %s=%q: error = nil, want a parse failure (got cfg %+v)", tc.env, tc.bad, cfg)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("ConfigFromEnv() error = %q; want it to name %s so the operator can find the typo", err, tc.env)
			}
			if !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("ConfigFromEnv() error = %q; want it to quote the rejected value %q", err, tc.bad)
			}
			if cfg != (Config{}) {
				t.Errorf("ConfigFromEnv() returned %+v alongside an error; want the zero Config so a caller that ignores err cannot boot on a half-parsed one", cfg)
			}
		})
	}
}
