//go:build chdb

// chDB-backed proof that the instant classic-histogram window guards
// (classic_bucket_window_bound.go) fire at real ClickHouse execution, each at
// its exact boundary: the query-wide pre-count against
// ClassicBucketWindowMaxCostUnits, and the per-series entry count against
// ClassicBucketWindowSeriesMaxEntries. It reuses the bucket-merge guard's
// disjoint-layout seed (classic_bucket_merge_bound_chdb_test.go).
package promql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
)

// classicBucketWindowUnbounded is a ceiling no seed below reaches, set on
// the window check a case does not exercise.
const classicBucketWindowUnbounded int64 = 1 << 60

// Per-unit costs of the query-wide estimate, restated from
// classic_bucket_window_bound.go so a change to the model there fails the
// boundary below instead of moving it silently.
const (
	windowSeriesBytes       = 160
	windowLadderEntryBytes  = 40
	windowScannedEntryBytes = 12
	windowFoldBytes         = 440 * 2
)

// windowSeriesEntries is the bucket entries one series of shape collects in
// the window: every sample's finite bounds plus its +Inf rung.
func windowSeriesEntries(sh classicBucketMergeBoundShape) int64 {
	return int64(sh.samples) * int64(sh.width+1)
}

// windowCost is the query-wide estimate for shape. Every series carries
// samples rows of one layout, so the pre-count's terms are exact: N series,
// E = N × samples × (width + 1) scanned entries, R = N × samples rows, the
// ladder M = N × E / R = N × (width + 1) and the mean series E / N.
func windowCost(sh classicBucketMergeBoundShape) int64 {
	n := int64(sh.series)
	ladder := n * int64(sh.width+1)
	entries := n * windowSeriesEntries(sh)
	return windowSeriesBytes*n + windowLadderEntryBytes*ladder +
		windowScannedEntryBytes*(entries-ladder) + windowFoldBytes*windowSeriesEntries(sh)
}

// windowBounds is the resource bounds with the two window ceilings set and
// the bucket-merge ceiling left to its derivation.
func windowBounds(maxCost, maxSeriesEntries int64) promql.LowerOpts {
	return promql.LowerOpts{ResourceBounds: promql.ResourceBounds{
		ClassicBucketWindowMaxCostUnits:     maxCost,
		ClassicBucketWindowSeriesMaxEntries: maxSeriesEntries,
	}}
}

// classicBucketWindowBoundaryShapes discriminate the estimate's terms: a
// single-sample seed (no entries past the ladder), a multi-sample one, a
// one-bound-per-series one and a few wide rows (the fold term dominates).
var classicBucketWindowBoundaryShapes = []struct {
	name  string
	shape classicBucketMergeBoundShape
}{
	{"single-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 1}},
	{"multi-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 6}},
	{"one-bound-rows", classicBucketMergeBoundShape{series: 200, width: 1, samples: 3}},
	{"wide-rows", classicBucketMergeBoundShape{series: 4, width: 300, samples: 2}},
}

// TestClassicBucketWindowBudget_ChDB_CostBoundaryIsTheEstimate pins the
// query-wide check at its exact boundary: a ceiling equal to the estimate
// admits the query, one unit lower refuses it with the window message. The
// admitted runs go first: chDB's Parquet output after a throwIf error is
// malformed (cerberus #3761).
func TestClassicBucketWindowBudget_ChDB_CostBoundaryIsTheEstimate(t *testing.T) {
	for _, tc := range classicBucketWindowBoundaryShapes {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClassicBucketMergeBoundFixture(t, tc.shape)
			cost := windowCost(tc.shape)
			if err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(cost, classicBucketWindowUnbounded)); err != nil {
				t.Fatalf("%+v: a ceiling of exactly the %d estimate must admit the query: %v", tc.shape, cost, err)
			}
			err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(cost-1, classicBucketWindowUnbounded))
			if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowBudgetMessage) {
				t.Fatalf("%+v: a ceiling one below the %d estimate must be refused by the window check: %v", tc.shape, cost, err)
			}
		})
	}
}

// TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries pins
// the per-series check at its exact boundary: a ceiling equal to one
// series' window entries admits the query, one lower refuses it with the
// per-series message.
func TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries(t *testing.T) {
	for _, tc := range classicBucketWindowBoundaryShapes {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClassicBucketMergeBoundFixture(t, tc.shape)
			entries := windowSeriesEntries(tc.shape)
			if err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(classicBucketWindowUnbounded, entries)); err != nil {
				t.Fatalf("%+v: a ceiling of exactly %d entries per series must admit the query: %v", tc.shape, entries, err)
			}
			err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(classicBucketWindowUnbounded, entries-1))
			if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
				t.Fatalf("%+v: a ceiling one below %d entries per series must be refused by the per-series check: %v", tc.shape, entries, err)
			}
		})
	}
}

// TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries seeds one
// series of a single sample past the default per-series ceiling and asserts
// the default bounds refuse it with the per-series message — the shape a
// query-wide count cannot see, since one series of one sample is a small
// scan.
func TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries(t *testing.T) {
	ceiling := promql.ClassicBucketWindowSeriesEntriesForMemory(0)
	shape := classicBucketMergeBoundShape{series: 1, width: int(ceiling), samples: 1}
	if got := windowSeriesEntries(shape); got <= ceiling {
		t.Fatalf("seed %+v carries %d entries; want past the default per-series ceiling %d", shape, got, ceiling)
	}
	if got, costCeiling := windowCost(shape), promql.ClassicBucketWindowCostUnitsForMemory(0); got > costCeiling {
		t.Fatalf("seed %+v estimates %d; want within the default query-wide ceiling %d so the per-series check decides", shape, got, costCeiling)
	}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	err := runClassicBucketMergeBoundInstantQuery(t, fixture)
	if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
		t.Fatalf("seed %+v: the default bounds must refuse it with the per-series message: %v", shape, err)
	}
}
