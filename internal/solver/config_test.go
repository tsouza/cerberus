package solver

import (
	"testing"
	"time"
)

func TestDefaultConfig_Valid(t *testing.T) {
	t.Parallel()
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig must validate, got %v", err)
	}
	c := DefaultConfig()
	if c.Mode != ModeSingle {
		t.Fatalf("default Mode = %q, want %q (ship dark)", c.Mode, ModeSingle)
	}
	if c.MinFanout != 16 || c.MinAnchorPairs != 4000 || c.MaxK != 8 ||
		c.MinAnchorsPerSlice != 16 || c.Parallel != 3 || c.MaxOutputRows != 2_000_000 {
		t.Fatalf("default tuning drifted: %+v", c)
	}
	if c.Timeout != 60*time.Second {
		t.Fatalf("default Timeout = %s, want 60s", c.Timeout)
	}
}

func TestConfigValidate_FailFast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"bad mode", func(c *Config) { c.Mode = "scatter" }},
		{"P < 1", func(c *Config) { c.Parallel = 0 }},
		{"MaxK < 2", func(c *Config) { c.MaxK = 1 }},
		{"MinAnchorsPerSlice < 2", func(c *Config) { c.MinAnchorsPerSlice = 1 }},
		{"MinFanout < 1", func(c *Config) { c.MinFanout = 0 }},
		{"MaxOutputRows <= 0", func(c *Config) { c.MaxOutputRows = 0 }},
		{"Timeout <= 0", func(c *Config) { c.Timeout = 0 }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("%s: expected Validate to fail", tc.name)
			}
		})
	}
}
