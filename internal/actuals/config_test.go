package actuals

import "testing"

func TestConfig_ValidateAcceptsDefault(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig should validate, got %v", err)
	}
}

func TestConfig_ValidateRejectsBadFields(t *testing.T) {
	base := DefaultConfig()

	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"non-positive lower ratio", func(c *Config) { c.DriftLowerRatio = 0 }},
		{"upper not above lower", func(c *Config) { c.DriftUpperRatio = c.DriftLowerRatio }},
		{"zero min observations", func(c *Config) { c.MinObservations = 0 }},
		{"zero ema alpha", func(c *Config) { c.EMAAlpha = 0 }},
		{"ema alpha above one", func(c *Config) { c.EMAAlpha = 1.5 }},
		{"non-positive entry ttl", func(c *Config) { c.EntryTTL = 0 }},
		{"non-positive poll interval", func(c *Config) { c.QueryLogPollInterval = 0 }},
		{"non-positive lookback", func(c *Config) { c.QueryLogLookback = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected Validate to reject %+v", cfg)
			}
		})
	}
}
