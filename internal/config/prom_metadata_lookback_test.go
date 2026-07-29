package config

import (
	"strings"
	"testing"
	"time"
)

// TestFromEnv_PromMetadataLookback_Default confirms the knob defaults to
// zero. Zero is not "no bound" — it is "the operator has not stated a
// retention", which hands the windowless metadata-discovery horizon to
// internal/api/prom's own fallback. A non-zero default here would duplicate
// that fallback in a second place and drift from it.
func TestFromEnv_PromMetadataLookback_Default(t *testing.T) {
	t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.PromMetadataLookback != 0 {
		t.Errorf("PromMetadataLookback = %s; want 0 (defer to the handler fallback)", cfg.PromMetadataLookback)
	}
}

// TestFromEnv_PromMetadataLookback_Accepted is the compatibility table for
// this knob's duration grammar. It fixes BOTH halves of the shared grammar
// against the same knob:
//
//   - the Go duration forms — every spelling this knob has ever resolved
//     must still resolve to the same duration, because an environment or
//     values file already carrying `2160h` must keep starting the process;
//   - the retention units — `90d` / `2w` / `1y`, which an operator reaches
//     for when stating a retention, must resolve to the duration their hour
//     form spells.
//
// The pairs that repeat a duration under two spellings (2160h / 90d, 336h /
// 2w) are the load-bearing rows: they assert the two halves agree rather than
// merely both parsing.
func TestFromEnv_PromMetadataLookback_Accepted(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		// Go duration syntax — the forms accepted before the grammars merged.
		{"2160h", 2160 * time.Hour},
		{"336h", 336 * time.Hour},
		{"90m", 90 * time.Minute},
		{"0s", 0},
		{"1.5h", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
		{"100us", 100 * time.Microsecond},
		{"100ns", 100 * time.Nanosecond},
		{"1h30m", 90 * time.Minute},
		{"30m1h", 90 * time.Minute},
		{"+1h", time.Hour},
		// Retention units — the day-scale forms Go duration syntax cannot spell.
		{"90d", 2160 * time.Hour},
		{"2w", 336 * time.Hour},
		{"1y", 8760 * time.Hour},
		{"1w2d", 216 * time.Hour},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv(%q): %v", tc.val, err)
			}
			if cfg.PromMetadataLookback != tc.want {
				t.Errorf("PromMetadataLookback(%q) = %s; want %s", tc.val, cfg.PromMetadataLookback, tc.want)
			}
		})
	}
}

// TestFromEnv_PromMetadataLookback_Malformed confirms a value in neither half
// of the shared duration grammar fails fast at startup, naming the env var so
// the operator knows which one to fix.
func TestFromEnv_PromMetadataLookback_Malformed(t *testing.T) {
	for _, val := range []string{"forever", "14 days", "2 weeks", "1M", "-1x"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_PROM_METADATA_LOOKBACK") {
				t.Errorf("error %q does not name the env var", err)
			}
		})
	}
}

// TestFromEnv_PromMetadataLookback_RejectsNegative pins the RANGE guard, not
// the parser. A negative lookback would put the derived window's start AFTER
// "now", so every windowless discovery request would return an empty label /
// metric set — a total silent drop that looks exactly like an empty database
// rather than like a misconfiguration.
//
// The shared duration grammar accepts a sign (Go duration syntax always has),
// so `-1h` reaches the loader as a well-formed value and only the
// non-negative check stops it. Asserting on the guard's own wording is what
// makes this test bite in both directions: it fails if the guard is removed
// (the value would be accepted) and it fails if the rejection silently moves
// back into the parser (the guard would be unreachable dead code again).
func TestFromEnv_PromMetadataLookback_RejectsNegative(t *testing.T) {
	const rangeErr = "must be >= 0"
	for _, val := range []string{"-1h", "-1s", "-1h30m", "-2160h"} {
		t.Run(val, func(t *testing.T) {
			// The guard can only be what rejects these if the parser accepts them.
			if _, err := parseDuration(val); err != nil {
				t.Fatalf("parseDuration(%q) = %v; the range guard is unreachable for this value", val, err)
			}
			t.Setenv("CERBERUS_PROM_METADATA_LOOKBACK", val)
			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv accepted %q; want a range error", val)
			}
			if !strings.Contains(err.Error(), "CERBERUS_PROM_METADATA_LOOKBACK") {
				t.Errorf("error %q does not name the env var", err)
			}
			if !strings.Contains(err.Error(), rangeErr) {
				t.Errorf("error %q is not the range guard's (%q)", err, rangeErr)
			}
		})
	}
}
