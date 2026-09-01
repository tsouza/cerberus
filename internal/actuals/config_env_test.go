package actuals

import "testing"

func TestConfigFromEnv_Defaults(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := DefaultConfig()
	if cfg != want {
		t.Fatalf("expected DefaultConfig when unset, got %+v want %+v", cfg, want)
	}
	if cfg.Enabled {
		t.Fatal("the feature must ship dark: Enabled must default to false")
	}
}

func TestConfigFromEnv_Overrides(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvDriftLowerRatio, "0.05")
	t.Setenv(EnvDriftUpperRatio, "5")
	t.Setenv(EnvMinObservations, "3")
	t.Setenv(EnvEMAAlpha, "0.5")
	t.Setenv(EnvEntryTTL, "10m")
	t.Setenv(EnvQueryLogPollInterval, "30s")
	t.Setenv(EnvQueryLogLookback, "2m")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if !cfg.Enabled || cfg.DriftLowerRatio != 0.05 || cfg.DriftUpperRatio != 5 ||
		cfg.MinObservations != 3 || cfg.EMAAlpha != 0.5 ||
		cfg.EntryTTL.String() != "10m0s" || cfg.QueryLogPollInterval.String() != "30s" ||
		cfg.QueryLogLookback.String() != "2m0s" {
		t.Fatalf("unexpected config from overrides: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected the overridden config to validate, got %v", err)
	}
}

func TestConfigFromEnv_InvalidValueFailsFast(t *testing.T) {
	t.Setenv(EnvMinObservations, "not-a-number")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected a malformed CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS to fail fast")
	}
}
