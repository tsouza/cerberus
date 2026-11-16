//go:build chdb

// chDB-backed proof that the instant classic-histogram per-series window
// guard (classic_bucket_window_bound.go) fires at real ClickHouse execution
// at its exact boundary: the per-series entry count against
// ClassicBucketWindowSeriesMaxEntries. It reuses the bucket-merge guard's
// disjoint-layout seed (classic_bucket_merge_bound_chdb_test.go).
package promql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
)

// classicBucketWindowUnbounded is a per-series ceiling no seed reaches, set
// where a case exercises another check.
const classicBucketWindowUnbounded int64 = 1 << 60

// windowSeriesEntries is the bucket entries one series of shape collects in
// the window: every sample's finite bounds plus its +Inf rung.
func windowSeriesEntries(sh classicBucketMergeBoundShape) int64 {
	return int64(sh.samples) * int64(sh.width+1)
}

// windowBounds is the resource bounds with the per-series window ceiling
// set and the bucket-merge ceiling left to its derivation.
func windowBounds(maxSeriesEntries int64) promql.LowerOpts {
	return promql.LowerOpts{ResourceBounds: promql.ResourceBounds{
		ClassicBucketWindowSeriesMaxEntries: maxSeriesEntries,
	}}
}

// classicBucketWindowBoundaryShapes cover a single-sample seed, a
// multi-sample one, a one-bound-per-series one and a few wide rows.
var classicBucketWindowBoundaryShapes = []struct {
	name  string
	shape classicBucketMergeBoundShape
}{
	{"single-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 1}},
	{"multi-sample", classicBucketMergeBoundShape{series: 40, width: 20, samples: 6}},
	{"one-bound-rows", classicBucketMergeBoundShape{series: 200, width: 1, samples: 3}},
	{"wide-rows", classicBucketMergeBoundShape{series: 4, width: 300, samples: 2}},
}

// TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries pins
// the per-series check at its exact boundary: a ceiling equal to one
// series' window entries admits the query, one lower refuses it with the
// per-series message. The admitted runs go first: chDB's Parquet output
// after a throwIf error is malformed (cerberus #3761).
func TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries(t *testing.T) {
	for _, tc := range classicBucketWindowBoundaryShapes {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClassicBucketMergeBoundFixture(t, tc.shape)
			entries := windowSeriesEntries(tc.shape)
			if err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(entries)); err != nil {
				t.Fatalf("%+v: a ceiling of exactly %d entries per series must admit the query: %v", tc.shape, entries, err)
			}
			err := runClassicBucketMergeBoundInstantQueryWithOpts(t, fixture, windowBounds(entries-1))
			if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
				t.Fatalf("%+v: a ceiling one below %d entries per series must be refused by the per-series check: %v", tc.shape, entries, err)
			}
		})
	}
}

// TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries seeds one
// series of a single sample past the default per-series ceiling and asserts
// the default bounds refuse it with the per-series message: one series of
// one sample is a small scan whose fold alone exceeds its share of the cap.
func TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries(t *testing.T) {
	ceiling := promql.ClassicBucketWindowSeriesEntriesForMemory(0)
	shape := classicBucketMergeBoundShape{series: 1, width: int(ceiling), samples: 1}
	if got := windowSeriesEntries(shape); got <= ceiling {
		t.Fatalf("seed %+v carries %d entries; want past the default per-series ceiling %d", shape, got, ceiling)
	}
	fixture := newClassicBucketMergeBoundFixture(t, shape)
	err := runClassicBucketMergeBoundInstantQuery(t, fixture)
	if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
		t.Fatalf("seed %+v: the default bounds must refuse it with the per-series message: %v", shape, err)
	}
}
