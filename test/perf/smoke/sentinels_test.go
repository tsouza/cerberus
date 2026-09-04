package smoke

import (
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
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

// TestSentinels_FloorsArePartitioned asserts SentinelsForFloor covers the
// whole corpus exactly once across the declared floors — a sentinel assigned
// to a floor the harness never boots would silently never run, which is the
// coverage-loss failure mode this corpus exists to prevent.
func TestSentinels_FloorsArePartitioned(t *testing.T) {
	floors := []ServerFloor{FloorBase, FloorJoinSpill}
	total := 0
	for _, floor := range floors {
		got := SentinelsForFloor(floor)
		if len(got) == 0 {
			t.Errorf("floor %s has no sentinels — the harness boots a real ClickHouse for it regardless", floor)
		}
		for _, s := range got {
			if s.Floor != floor {
				t.Errorf("SentinelsForFloor(%s) returned %s, whose Floor is %s", floor, s.Name, s.Floor)
			}
		}
		total += len(got)
	}
	if total != len(Sentinels) {
		t.Errorf("the declared floors cover %d of %d sentinels — one is assigned to a floor the harness "+
			"never boots, so it would silently never run", total, len(Sentinels))
	}
}

// TestSentinels_JoinSpillStampIsAsserted pins the one thing that makes the
// join_spill sentinel falsifiable: it must require the
// max_bytes_before_external_join stamp, sized at half the live memory cap
// (internal/engine/spill.go's spillThreshold). Without that requirement the
// sentinel would measure only peak memory and HTTP status — both identical
// whether the stamp fired or the mechanism was deleted outright.
func TestSentinels_JoinSpillStampIsAsserted(t *testing.T) {
	joinSpill := SentinelsForFloor(FloorJoinSpill)
	if len(joinSpill) != 1 {
		t.Fatalf("SentinelsForFloor(FloorJoinSpill) returned %d sentinels, want 1", len(joinSpill))
	}
	s := joinSpill[0]

	query := s.Params(time.Unix(0, 0), time.Unix(0, 0).Add(sentinelWindow)).Get("query")
	if !strings.Contains(query, " / on (session_id) ") {
		t.Errorf("join_spill sentinel query %q is not a vector-vector match — it would not lower to "+
			"chplan.VectorJoin, so chplan.HasJoin would never match it and the stamp could not fire", query)
	}

	const cap1GiB int64 = 1 << 30
	got := s.RequiredSettings(cap1GiB)
	want := map[string]string{settingMaxBytesBeforeExternalJoin: "536870912"} // 1 GiB / 2
	if len(got) != len(want) {
		t.Fatalf("join_spill sentinel RequiredSettings = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("join_spill sentinel RequiredSettings[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestSentinels_SortedSlabOverTimeStampIsAsserted pins the one thing that
// makes the sorted-slab sentinel falsifiable — it must require the
// max_block_size=1 stamp applySortedSlabOverTimeMemoryBound (cerberus#3046)
// stamps — plus the opt-in wiring that makes the sentinel actually REACH
// that plan shape at all: chopt.FeatureSortedSlabOverTime is AutoSelect:
// false, so without an explicit Optimizations listing and a RequiredFeature
// activation guard, this sentinel would run against the harness's ordinary
// "auto" lane and never build a chplan.RangeWindow.SortedSlabOverTime node
// in the first place — a vacuous pass indistinguishable from the mechanism
// never existing.
func TestSentinels_SortedSlabOverTimeStampIsAsserted(t *testing.T) {
	base := SentinelsForFloor(FloorBase)
	var s *Sentinel
	for i := range base {
		if base[i].Name == "sorted_slab_over_time_memory_bound" {
			s = &base[i]
			break
		}
	}
	if s == nil {
		t.Fatalf("SentinelsForFloor(FloorBase) has no sorted_slab_over_time_memory_bound sentinel")
	}

	if s.Optimizations != OptInSortedSlabOverTime {
		t.Errorf("sorted-slab sentinel Optimizations = %q, want %q — without the explicit opt-in "+
			"listing the sentinel resolves against the harness's default \"auto\" lane, where "+
			"AutoSelect:false keeps chopt.FeatureSortedSlabOverTime permanently off", s.Optimizations, OptInSortedSlabOverTime)
	}
	if s.RequiredFeature != chopt.FeatureSortedSlabOverTime {
		t.Errorf("sorted-slab sentinel RequiredFeature = %q, want %q", s.RequiredFeature, chopt.FeatureSortedSlabOverTime)
	}

	query := s.Params(time.Unix(0, 0), time.Unix(0, 0).Add(sentinelWindow)).Get("query")
	if !strings.Contains(query, "sum_over_time(") {
		t.Errorf("sorted-slab sentinel query %q does not call sum_over_time(...)", query)
	}
	if !strings.Contains(query, SortedSlabOverTimeGaugeMetric) {
		t.Errorf("sorted-slab sentinel query %q does not reference %s", query, SortedSlabOverTimeGaugeMetric)
	}

	if anchors := int(s.Window / s.Step); anchors != sortedSlabOverTimeAnchorCount {
		t.Errorf("sorted-slab sentinel Window/Step = %v/%v -> %d anchors, want %d (cerberus#3046's own "+
			"OOM reproduction scale)", s.Window, s.Step, anchors, sortedSlabOverTimeAnchorCount)
	}

	const cap1GiB int64 = 1 << 30
	got := s.RequiredSettings(cap1GiB)
	want := map[string]string{settingMaxBlockSize: wantSortedSlabOverTimeMaxBlockSize}
	if len(got) != len(want) {
		t.Fatalf("sorted-slab sentinel RequiredSettings = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("sorted-slab sentinel RequiredSettings[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestSentinels_RequiredSettingsIsNilSafe pins that a sentinel declaring no
// required settings yields an empty map rather than panicking — the harness
// ranges over the result for every sentinel, not just the ones that declare
// the field.
func TestSentinels_RequiredSettingsIsNilSafe(t *testing.T) {
	for _, s := range Sentinels {
		if s.RequiredQuerySettings != nil {
			continue
		}
		if got := s.RequiredSettings(1 << 30); len(got) != 0 {
			t.Errorf("sentinel %s declares no RequiredQuerySettings but RequiredSettings returned %v", s.Name, got)
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
