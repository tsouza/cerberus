package smoke

import (
	"strings"
	"testing"
	"time"
)

// TestSentinels_Roster asserts every sentinel in the single source of truth
// is fully populated — a sentinel with a blank field would silently issue a
// broken request in the real-ClickHouse integration lane, where a driver
// error is much more expensive to root-cause than this unit check.
func TestSentinels_Roster(t *testing.T) {
	const wantCount = 3
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
		if s.Mechanism == "" {
			t.Fatalf("sentinel %s has empty Mechanism", s.Name)
		}
		if s.Path == "" {
			t.Fatalf("sentinel %s has empty Path", s.Name)
		}
		if s.Params == nil {
			t.Fatalf("sentinel %s has nil Params", s.Name)
		}
		if s.Window <= 0 {
			t.Fatalf("sentinel %s has non-positive Window %v", s.Name, s.Window)
		}
	}
}

func TestSentinels_NativeHistogramParams(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(sentinelWindow)

	native := Sentinels[0]
	if native.Window != sentinelWindow || native.Step != sentinelStep {
		t.Fatalf("native sentinel window/step = %v/%v, want %v/%v", native.Window, native.Step, sentinelWindow, sentinelStep)
	}
	query := native.Params(start, end).Get("query")
	if !strings.Contains(query, NativeHistogramMetric) {
		t.Fatalf("native sentinel query %q does not reference %s", query, NativeHistogramMetric)
	}
	if !strings.Contains(query, "histogram_quantile") {
		t.Fatalf("native sentinel query %q does not call histogram_quantile", query)
	}
}

func TestSentinels_SpillParams(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(sentinelWindow)

	spill := Sentinels[1]
	query := spill.Params(start, end).Get("query")
	if !strings.Contains(query, WideCounterMetric) {
		t.Fatalf("spill sentinel query %q does not reference %s", query, WideCounterMetric)
	}
	if !strings.Contains(query, "session_id") {
		t.Fatalf("spill sentinel query %q does not group by session_id", query)
	}
}

func TestSentinels_CompareParams(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(sentinelWindow)

	compare := Sentinels[2]
	if compare.Step != 0 {
		t.Fatalf("compare sentinel Step = %v, want 0 (Tempo default query-range step)", compare.Step)
	}
	q := compare.Params(start, end).Get("q")
	if !strings.Contains(q, "compare(") {
		t.Fatalf("compare sentinel q %q does not call compare(...)", q)
	}
	if !strings.Contains(q, "status = error") {
		t.Fatalf("compare sentinel q %q does not select the error subpopulation", q)
	}
}
