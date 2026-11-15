package config

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestFromEnv_TempoMaxTraceDuration_Default confirms an unset knob resolves to
// internal/schema's default and lands on the traces schema the Tempo head
// lowers with.
func TestFromEnv_TempoMaxTraceDuration_Default(t *testing.T) {
	t.Setenv(envTempoMaxTraceDuration, "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Traces.MaxTraceDuration != schema.DefaultMaxTraceDuration {
		t.Errorf("Traces.MaxTraceDuration = %s; want %s", cfg.Traces.MaxTraceDuration, schema.DefaultMaxTraceDuration)
	}
}

// TestFromEnv_TempoMaxTraceDuration_Override confirms an explicit value,
// including the zero that turns the widening off, reaches the traces schema.
func TestFromEnv_TempoMaxTraceDuration_Override(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"6h", 6 * time.Hour},
		{"1d", 24 * time.Hour},
		{"90s", 90 * time.Second},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv(envTempoMaxTraceDuration, tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv(%q): %v", tc.val, err)
			}
			if cfg.Traces.MaxTraceDuration != tc.want {
				t.Errorf("Traces.MaxTraceDuration(%q) = %s; want %s", tc.val, cfg.Traces.MaxTraceDuration, tc.want)
			}
		})
	}
}

// TestFromEnv_TempoMaxTraceDuration_Rejected confirms a negative or malformed
// value fails fast at startup, naming the env var.
func TestFromEnv_TempoMaxTraceDuration_Rejected(t *testing.T) {
	for _, val := range []string{"-1h", "an hour"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(envTempoMaxTraceDuration, val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv(%q) succeeded; want an error", val)
			}
			if !strings.Contains(err.Error(), envTempoMaxTraceDuration) {
				t.Errorf("FromEnv(%q) error %q does not name %s", val, err, envTempoMaxTraceDuration)
			}
		})
	}
}
