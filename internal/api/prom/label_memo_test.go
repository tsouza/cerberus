package prom

import (
	"maps"
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestLabelMemo_MatchesDirectNormalize pins that the memo returns exactly
// what the direct `NormalizeLabelMap(WithMetricName(...))` call would — for
// the first row of a series AND every cached repeat — so swapping the
// pivots onto the memo is behaviour-preserving.
func TestLabelMemo_MatchesDirectNormalize(t *testing.T) {
	t.Parallel()
	shared := map[string]string{
		"route":              "/api",
		"k8s.namespace.name": "prod", // dotted key the normaliser rewrites
	}
	s := chclient.Sample{MetricName: "http.requests.total", Labels: shared, SeriesID: 1}

	want := format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))
	memo := newLabelMemo(0)
	first := memo.normalize(s)
	if !maps.Equal(first, want) {
		t.Fatalf("first normalize = %v, want %v", first, want)
	}
	// A second row of the same series (same SeriesID + MetricName) must
	// return the cached instance, not a fresh allocation. Maps are
	// reference types, so a sentinel written into the first result is
	// visible through the second iff they are the same underlying map —
	// proving the per-row rebuild is elided (no reflect needed).
	first["__memo_probe__"] = "1"
	second := memo.normalize(s)
	if second["__memo_probe__"] != "1" {
		t.Errorf("repeat normalize returned a fresh map; memo did not cache")
	}
}

// TestLabelMemo_DistinctMetricNamesDoNotCollapse pins that two rows sharing
// an interned Labels map (same SeriesID) but carrying different MetricNames
// — the same attribute set under distinct metric names — get distinct wire
// label sets. The MetricName is part of the memo key precisely to prevent
// the collapse.
func TestLabelMemo_DistinctMetricNamesDoNotCollapse(t *testing.T) {
	t.Parallel()
	shared := map[string]string{"job": "api"}
	memo := newLabelMemo(0)
	a := memo.normalize(chclient.Sample{MetricName: "up", Labels: shared, SeriesID: 7})
	b := memo.normalize(chclient.Sample{MetricName: "down", Labels: shared, SeriesID: 7})
	if a["__name__"] != "up" || b["__name__"] != "down" {
		t.Fatalf("metric names collapsed: a=%v b=%v", a, b)
	}
}

// TestLabelMemo_ZeroSeriesIDNeverAliases pins that a non-interned cursor
// (SeriesID 0 — the slice-backed test double, or any path that bypasses
// internLabels) recomputes every call so two genuinely-distinct series that
// both report SeriesID 0 never alias each other's label set.
func TestLabelMemo_ZeroSeriesIDNeverAliases(t *testing.T) {
	t.Parallel()
	memo := newLabelMemo(0)
	a := memo.normalize(chclient.Sample{
		MetricName: "up", Labels: map[string]string{"job": "a"}, SeriesID: 0,
	})
	b := memo.normalize(chclient.Sample{
		MetricName: "up", Labels: map[string]string{"job": "b"}, SeriesID: 0,
	})
	if a["job"] != "a" || b["job"] != "b" {
		t.Fatalf("zero-SeriesID rows aliased: a=%v b=%v", a, b)
	}
}
