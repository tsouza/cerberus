package loki

import (
	"testing"
	"time"
)

// TestBodyTTLBoundaryCrossed pins the pure boundary comparison
// HeaderBodyTTLWindow's mitigation is built on (cerberus issue #2769): a
// zero/negative bodyTTL ("column TTL off") never reports a crossing
// regardless of how old start is, and a positive bodyTTL reports a
// crossing exactly when start is at or before now - bodyTTL (a row at
// that instant is the OLDEST row whose Body has NOT yet expired — one that
// old or older means the window touches an aged-out row).
func TestBodyTTLBoundaryCrossed(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	sevenDays := 7 * 24 * time.Hour

	cases := []struct {
		name    string
		bodyTTL time.Duration
		start   time.Time
		want    bool
	}{
		{"ttl_disabled_zero", 0, now.Add(-30 * 24 * time.Hour), false},
		{"ttl_disabled_negative", -time.Hour, now.Add(-30 * 24 * time.Hour), false},
		{"window_well_within_ttl", sevenDays, now.Add(-time.Hour), false},
		{"window_starts_exactly_at_boundary", sevenDays, now.Add(-sevenDays), true},
		{"window_starts_one_second_past_boundary", sevenDays, now.Add(-sevenDays).Add(-time.Second), true},
		{"window_starts_one_second_before_boundary", sevenDays, now.Add(-sevenDays).Add(time.Second), false},
		{"window_starts_far_in_the_past", sevenDays, now.Add(-365 * 24 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyTTLBoundaryCrossed(tc.bodyTTL, tc.start, now); got != tc.want {
				t.Errorf("bodyTTLBoundaryCrossed(%v, %v, now) = %v, want %v", tc.bodyTTL, tc.start, got, tc.want)
			}
		})
	}
}
