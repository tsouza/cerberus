package nightly

import (
	"slices"
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
// require the enable_adaptive_aggregator=0 stamp and from which line: the
// exponential-histogram counter-window sentinel on 26.8 and later, none below it, where the
// setting does not exist and the server would reject it.
func TestSentinels_HistogramShapesWantAdaptiveAggregatorOff(t *testing.T) {
	want := map[string]bool{
		"native_histogram_quantiles_rate": true,
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

// TestCarriesExplicitPrewhere pins the statement prong's reading of a
// dispatched statement: a PREWHERE clause counts in any case, and the word
// inside a string literal or a quoted identifier does not.
func TestCarriesExplicitPrewhere(t *testing.T) {
	for _, tc := range []struct {
		statement string
		want      bool
	}{
		{"SELECT `v` FROM `t` PREWHERE `MetricName` = 'm' WHERE `x` = 1", true},
		{"SELECT v FROM t prewhere MetricName = 'm'", true},
		{"SELECT `v` FROM `t`\nPREWHERE\n`MetricName` = 'm'", true},
		{"SELECT `v` FROM `t` WHERE `MetricName` = 'm' AND `x` = 1", false},
		{"SELECT `v` FROM `t` WHERE `Attributes`['route'] = 'PREWHERE'", false},
		{"SELECT `v` FROM `t` WHERE `a` = 'it''s \\' PREWHERE'", false},
		{"SELECT `PREWHERE` FROM `t` WHERE \"PREWHERE\" = 1", false},
		{"SELECT v FROM t WHERE prewhere_hits > 0", false},
		{"SELECT v FROM t WHERE PREWHEREX > 0", false},
		{"SELECT v FROM t WHERE no_prewhere > 0", false},
		{"SELECT v FROM t WHERE x > 0 SETTINGS optimize_move_to_prewhere = 1", false},
	} {
		if got := carriesExplicitPrewhere(tc.statement); got != tc.want {
			t.Errorf("carriesExplicitPrewhere(%q) = %v; want %v", tc.statement, got, tc.want)
		}
	}
}

// TestSentinels_ServerPrewhereOnLines pins the statement prong to the lines
// it belongs on: both label_values sentinels carry it, and every sentinel
// that carries it does so on exactly the release lines nightly-baseline.json
// calibrates whose version move_to_prewhere's AutoRange covers, so "auto"
// leaves the split to the server there. A drifted AutoRange fails here.
func TestSentinels_ServerPrewhereOnLines(t *testing.T) {
	var autoRange chopt.VersionRange
	for _, f := range chopt.Registry() {
		if f.ID == chopt.FeatureMoveToPrewhere {
			autoRange = f.AutoRange
		}
	}
	var want []string
	for _, v := range mustReadBaseline(t).Versions {
		line, ok := chopt.ParseVersion(v.ClickHouse)
		if !ok {
			t.Fatalf("nightly-baseline.json line %q does not parse", v.ClickHouse)
		}
		if autoRange.Contains(line) {
			want = append(want, v.ClickHouse)
		}
	}
	if len(want) == 0 {
		t.Fatalf("no line nightly-baseline.json calibrates lies in move_to_prewhere's AutoRange %v; the statement prong checks nothing", autoRange)
	}
	carriers := map[string]bool{"label_values_matcher_stock_key": true, "label_values_matcher_stock_key_renamed": true}
	for _, s := range Sentinels {
		if (carriers[s.Name] || len(s.ServerPrewhereOn) != 0) && !slices.Equal(s.ServerPrewhereOn, want) {
			t.Errorf("%s: ServerPrewhereOn = %v; want %v, the calibrated lines in move_to_prewhere's AutoRange %v",
				s.Name, s.ServerPrewhereOn, want, autoRange)
		}
	}
}
