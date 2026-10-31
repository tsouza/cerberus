package nightly

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestSentinels_Roster asserts every sentinel in the single source of truth
// is fully populated — a sentinel with a blank field would silently issue a
// broken request in the real-ClickHouse integration lane, where a driver
// error is much more expensive to root-cause than this unit check.
func TestSentinels_Roster(t *testing.T) {
	const wantCount = 10
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
		if params.Get("query") == "" && params.Get("match[]") == "" {
			t.Fatalf("sentinel %s produced neither a query nor a series selector", s.Name)
		}
		if s.PlanningPeer != nil && s.PlanningPeer(sampleWindowStart, sampleWindowEnd).Get("query") == "" {
			t.Fatalf("sentinel %s has a planning peer that produced no query", s.Name)
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

// TestSentinels_HistogramShapesWantAdaptiveAggregatorOff pins which sentinels
// require the enable_adaptive_aggregator=0 stamp and from which line: the two
// histogram sample-window sentinels on 26.8 and later, none below it, where the
// setting does not exist and the server would reject it.
func TestSentinels_HistogramShapesWantAdaptiveAggregatorOff(t *testing.T) {
	want := map[string]bool{
		"classic_histogram_quantile_by_route": true,
		"native_histogram_quantiles_rate":     true,
	}
	for _, line := range []struct {
		version chopt.Version
		applies bool
	}{
		{chopt.Version{Major: 26, Minor: 6}, false},
		{chopt.Version{Major: 26, Minor: 8}, true},
		{chopt.Version{Major: 26, Minor: 9}, true},
	} {
		for _, s := range Sentinels {
			wanted := s.WantedOn(line.version)
			if !want[s.Name] || !line.applies {
				if len(wanted) != 0 {
					t.Errorf("%s on %s wants %v; want none", s.Name, line.version, wanted)
				}
				continue
			}
			if len(wanted) != 1 || wanted[0].Name != "enable_adaptive_aggregator" || wanted[0].Value != "0" {
				t.Errorf("%s on %s wants %v; want enable_adaptive_aggregator=0", s.Name, line.version, wanted)
			}
		}
	}
}
