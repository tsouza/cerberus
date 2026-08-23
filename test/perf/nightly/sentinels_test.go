package nightly

import (
	"testing"
	"time"
)

// TestSentinels_Roster asserts every sentinel in the single source of truth
// is fully populated — a sentinel with a blank field would silently issue a
// broken request in the real-ClickHouse integration lane, where a driver
// error is much more expensive to root-cause than this unit check.
func TestSentinels_Roster(t *testing.T) {
	const wantCount = 5
	if len(Sentinels) != wantCount {
		t.Fatalf("len(Sentinels) = %d, want %d", len(Sentinels), wantCount)
	}
	seen := make(map[string]bool, len(Sentinels))
	for _, s := range Sentinels {
		if s.Name == "" {
			t.Fatal("sentinel with empty Name")
		}
		if seen[s.Name] {
			t.Fatalf("duplicate sentinel Name %q", s.Name)
		}
		seen[s.Name] = true
		if s.Family == "" {
			t.Fatalf("sentinel %s has empty Family", s.Name)
		}
		if s.Path == "" {
			t.Fatalf("sentinel %s has empty Path", s.Name)
		}
		if s.Params == nil {
			t.Fatalf("sentinel %s has nil Params", s.Name)
		}
		if s.BaselineHeadroom <= 1.0 {
			t.Fatalf("sentinel %s has BaselineHeadroom %v, want > 1.0 — a zero/blank value would silently "+
				"compute a committed ceiling at or below the calibration measurement itself, permanently failing PRONG (b)",
				s.Name, s.BaselineHeadroom)
		}
		params := s.Params(sampleWindowStart, sampleWindowEnd)
		if params.Get("query") == "" {
			t.Fatalf("sentinel %s produced an empty query", s.Name)
		}
	}
}

func TestSentinels_SampleWindowInsideCapturedSpan(t *testing.T) {
	if !sampleWindowStart.Before(sampleWindowEnd) {
		t.Fatalf("sampleWindowStart %v is not before sampleWindowEnd %v", sampleWindowStart, sampleWindowEnd)
	}
	if sampleWindowEnd.Sub(sampleWindowStart) < time.Hour {
		t.Errorf("sample window %v is suspiciously short for a sentinel query_range", sampleWindowEnd.Sub(sampleWindowStart))
	}
}
