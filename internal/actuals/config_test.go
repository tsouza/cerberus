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
		{"lookback equal to poll interval", func(c *Config) { c.Enabled = true; c.QueryLogLookback = c.QueryLogPollInterval }},
		{"lookback below poll interval", func(c *Config) { c.Enabled = true; c.QueryLogLookback = c.QueryLogPollInterval / 2 }},
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

// TestConfig_ValidateAcceptsAnInvertedLookbackWhileDisabled pins the one
// bound that is gated on Enabled: cerberus boots by calling Validate BEFORE
// it consults Enabled (cmd/cerberus's buildActualsTracker), so enforcing a
// NEW rule unconditionally would stop an existing deployment that carries an
// inverted lookback/poll pair on a tracker it never turned on. With the
// tracker on, the same pair is still rejected — the case above proves that,
// so this is a scoped exemption rather than the rule going missing.
func TestConfig_ValidateAcceptsAnInvertedLookbackWhileDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	cfg.QueryLogLookback = cfg.QueryLogPollInterval / 2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled tracker must not fail boot over a field nothing reads: %v", err)
	}
}
