package chplan

import (
	"testing"
	"time"
)

// TestOffsetSpan pins the instants a grid of offset windows reads: the
// span covers each window both as absolute instants and as an input that
// applies the offset itself, so a negative offset reaches past end and
// never inverts the span.
func TestOffsetSpan(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	cases := []struct {
		name           string
		offset, reach  time.Duration
		wantLo, wantHi time.Time
	}{
		{"no offset", 0, 5 * time.Minute, start.Add(-5 * time.Minute), end},
		{"positive offset", time.Hour, 5 * time.Minute, start.Add(-time.Hour - 5*time.Minute), end},
		{"negative offset", -time.Hour, 5 * time.Minute, start.Add(-5 * time.Minute), end.Add(time.Hour)},
		{"negative offset past reach and span", -time.Hour, time.Minute, start.Add(-time.Minute), end.Add(time.Hour)},
	}
	for _, tc := range cases {
		lo, hi := OffsetSpan(start, end, tc.offset, tc.reach)
		if !lo.Equal(tc.wantLo) || !hi.Equal(tc.wantHi) {
			t.Errorf("%s: OffsetSpan = [%s, %s], want [%s, %s]", tc.name, lo, hi, tc.wantLo, tc.wantHi)
		}
		if !lo.Before(hi) {
			t.Errorf("%s: span [%s, %s] is inverted", tc.name, lo, hi)
		}
		// Every window an anchor reads, absolute or with the offset applied
		// again, lies inside the span.
		for _, anchor := range []time.Time{start, end} {
			for _, w := range [][2]time.Time{
				{anchor.Add(-tc.offset - tc.reach), anchor.Add(-tc.offset)},
				{anchor.Add(-tc.reach), anchor},
			} {
				if w[0].Before(lo) || w[1].After(hi) {
					t.Errorf("%s: window [%s, %s] leaves the span [%s, %s]", tc.name, w[0], w[1], lo, hi)
				}
			}
		}
	}
}
