package loki

import (
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 7 unit tests for toMatrixStepGrid — the per-anchor pivot
// helper /loki/api/v1/query_range hands to the metric path. The
// matrix-shape RangeWindow emitter (internal/chsql/range_window.go::
// emitWindowedArrayMatrix) already fans the per-step grid into one
// row per (series, anchor) and drops empty-window anchors at the
// `WHERE length(window_vals) >= N` filter, so the pivot is a trivial
// row → sample copy with no step-grid iteration or lookback
// carry-forward (mirrors api/prom's matrixFromCursor). The cases
// below pin: standard alignment, single sample, empty buckets,
// multi-stream interleaving, boundary inclusion, out-of-range
// clipping, and label-set canonicalisation.

// stepGridFixture wires the boilerplate every case shares: epoch
// anchor, helper to add a sample at a step-offset, deterministic
// output ordering for assertion (toMatrixStepGrid iterates a map,
// so the output series order is non-deterministic).
type stepGridFixture struct {
	epoch time.Time
}

func newFixture() stepGridFixture {
	// Fixed anchor avoids `time.Now`-flake. Aligned on a step
	// boundary (HH:00:00) so step-aligned offsets land cleanly.
	return stepGridFixture{
		epoch: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	}
}

func (f stepGridFixture) at(offsetSec int) time.Time {
	return f.epoch.Add(time.Duration(offsetSec) * time.Second)
}

// sortByMetric returns the matrix sorted by canonical-key of Metric.
// Necessary because toMatrixStepGrid groups by map and emits series
// in non-deterministic order — assertions need a stable comparison.
func sortByMetric(m []MatrixSample) []MatrixSample {
	out := make([]MatrixSample, len(m))
	copy(out, m)
	sort.Slice(out, func(i, j int) bool {
		return format.CanonicalKey(out[i].Metric) < format.CanonicalKey(out[j].Metric)
	})
	return out
}

// floatStamp mirrors the helper toMatrixStepGrid uses internally so
// assertions don't drift from the wire format.
func floatStamp(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1e3
}

// TestToMatrixStepGrid_StandardAlignment — the canonical case from
// the issue plan: start=0, end=300s, step=30s. 11 step anchors at
// 0, 30, …, 300. Every anchor has a fresh sample at exactly that
// instant, so every anchor emits.
func TestToMatrixStepGrid_StandardAlignment(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	// One sample per step anchor (11 total), each carrying the
	// step index as its value so we can assert ordering.
	var samples []chclient.Sample
	for i := 0; i <= 10; i++ {
		samples = append(samples, chclient.Sample{
			Labels:    labels,
			Timestamp: f.at(i * 30),
			Value:     float64(i),
		})
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(300), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 11 {
		t.Fatalf("expected 11 step samples, got %d: %+v", len(got[0].Values), got[0].Values)
	}
	// Each step i must emit (stamp_i, "i"): the step-anchor
	// timestamp and the latest-at-or-before value.
	for i := 0; i <= 10; i++ {
		wantStamp := floatStamp(f.at(i * 30))
		if got[0].Values[i][0] != wantStamp {
			t.Errorf("step %d: stamp=%v, want %v", i, got[0].Values[i][0], wantStamp)
		}
		wantVal := strconv.FormatFloat(float64(i), 'f', -1, 64)
		if got[0].Values[i][1] != wantVal {
			t.Errorf("step %d: value=%v, want %v", i, got[0].Values[i][1], wantVal)
		}
	}
}

// TestToMatrixStepGrid_SingleSample — one sample inside the request
// window. The matrix-shape RangeWindow already produced exactly one
// row at the sample's anchor; the pivot copies it through as a
// single point at the sample's own timestamp (no lookback carry-
// forward across empty anchors — that LVF behaviour was the cause
// of the loki-compat ~250-anchor inflation on sparse `by (level)`
// queries).
func TestToMatrixStepGrid_SingleSample(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 42},
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(330), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 1 {
		t.Fatalf("expected 1 emitted point (single per-anchor row), got %d: %+v", len(got[0].Values), got[0].Values)
	}
	if got[0].Values[0][1] != "42" {
		t.Errorf("value=%v, want \"42\"", got[0].Values[0][1])
	}
	if got[0].Values[0][0] != floatStamp(f.at(0)) {
		t.Errorf("stamp=%v, want %v", got[0].Values[0][0], floatStamp(f.at(0)))
	}
}

// TestToMatrixStepGrid_LeadingEmptyBuckets — only the samples the
// SQL emitted survive the pivot; nothing back-fills the leading
// (or trailing) anchors that had no per-anchor row. Two input
// samples at t=120 and t=150 → two output points.
func TestToMatrixStepGrid_LeadingEmptyBuckets(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(120), Value: 7},
		{Labels: labels, Timestamp: f.at(150), Value: 8},
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(180), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 2 {
		t.Fatalf("expected 2 emitted points (one per per-anchor row), got %d: %+v", len(got[0].Values), got[0].Values)
	}
	wantStamps := []float64{
		floatStamp(f.at(120)),
		floatStamp(f.at(150)),
	}
	for i, want := range wantStamps {
		if got[0].Values[i][0] != want {
			t.Errorf("point %d: stamp=%v, want %v", i, got[0].Values[i][0], want)
		}
	}
	wantVals := []string{"7", "8"}
	for i, want := range wantVals {
		if got[0].Values[i][1] != want {
			t.Errorf("point %d: value=%v, want %v", i, got[0].Values[i][1], want)
		}
	}
}

// TestToMatrixStepGrid_AllEmpty — series whose samples all fall
// AFTER `end` produce no values for any step anchor and are dropped
// entirely from output (the `len(ms.Values) > 0` guard).
func TestToMatrixStepGrid_AllEmpty(t *testing.T) {
	t.Parallel()

	f := newFixture()
	samples := []chclient.Sample{
		// All samples sit past `end` — never visible at any anchor.
		{Labels: map[string]string{"job": "api"}, Timestamp: f.at(1000), Value: 1},
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(300), 30*time.Second)
	if len(got) != 0 {
		t.Fatalf("expected empty matrix (series dropped), got %d series: %+v", len(got), got)
	}
}

// TestToMatrixStepGrid_MultiStreamInterleaving — three distinct
// label sets with different sample density. Pins:
//  1. distinct label sets emit distinct series,
//  2. per-series rows pass through independently (one series's
//     sample timeline doesn't bleed into another's),
//  3. label-set identity is the CanonicalKey shape,
//  4. per-series points equal the per-series input row count.
func TestToMatrixStepGrid_MultiStreamInterleaving(t *testing.T) {
	t.Parallel()

	f := newFixture()
	jobA := map[string]string{"job": "api"}
	jobB := map[string]string{"job": "batch"}
	jobC := map[string]string{"job": "cron"}

	// Dense series A: sample at every 30s step from 0 to 120.
	// Sparse series B: samples only at 60 and 90.
	// One-shot series C: a single sample at 30s.
	samples := []chclient.Sample{
		{Labels: jobA, Timestamp: f.at(0), Value: 1},
		{Labels: jobA, Timestamp: f.at(30), Value: 2},
		{Labels: jobA, Timestamp: f.at(60), Value: 3},
		{Labels: jobA, Timestamp: f.at(90), Value: 4},
		{Labels: jobA, Timestamp: f.at(120), Value: 5},
		{Labels: jobB, Timestamp: f.at(60), Value: 10},
		{Labels: jobB, Timestamp: f.at(90), Value: 11},
		{Labels: jobC, Timestamp: f.at(30), Value: 99},
	}
	// Shuffle the input order to stress the per-series pre-sort
	// (the pivot still sorts each series's rows by Timestamp
	// before emitting so wire order stays ascending).
	samples = []chclient.Sample{
		samples[4], samples[7], samples[0], samples[5],
		samples[2], samples[6], samples[1], samples[3],
	}

	got := sortByMetric(toMatrixStepGrid(samples, f.at(0), f.at(120), 30*time.Second))
	if len(got) != 3 {
		t.Fatalf("expected 3 series, got %d: %+v", len(got), got)
	}

	// Sorted by canonical key: jobA, jobB, jobC ("api" < "batch" < "cron").
	if got[0].Metric["job"] != "api" {
		t.Errorf("got[0].job=%q, want api", got[0].Metric["job"])
	}
	if got[1].Metric["job"] != "batch" {
		t.Errorf("got[1].job=%q, want batch", got[1].Metric["job"])
	}
	if got[2].Metric["job"] != "cron" {
		t.Errorf("got[2].job=%q, want cron", got[2].Metric["job"])
	}

	// Series A: 5 input rows → 5 output points.
	if len(got[0].Values) != 5 {
		t.Errorf("series A: expected 5 values, got %d: %+v", len(got[0].Values), got[0].Values)
	}
	// Series B: 2 input rows → 2 output points (60, 90).
	if len(got[1].Values) != 2 {
		t.Errorf("series B: expected 2 values, got %d: %+v", len(got[1].Values), got[1].Values)
	}
	if got[1].Values[0][1] != "10" || got[1].Values[1][1] != "11" {
		t.Errorf("series B values=%+v, want [10 11]", got[1].Values)
	}
	// Series C: 1 input row → 1 output point at t=30s.
	if len(got[2].Values) != 1 {
		t.Errorf("series C: expected 1 value, got %d: %+v", len(got[2].Values), got[2].Values)
	}
	if got[2].Values[0][1] != "99" {
		t.Errorf("series C value=%v, want 99", got[2].Values[0][1])
	}
}

// TestToMatrixStepGrid_BoundaryInclusion — pins the inclusive
// boundary semantics encoded in the loop:
//
//	`for t := start; !t.After(end); t = t.Add(step)`
//
// `start` is included; `end` is included; sample at `t = anchor`
// is visible at that anchor (`!After(t)` ≡ `<=`).
func TestToMatrixStepGrid_BoundaryInclusion(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	// Three samples, one at each boundary + one inside.
	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 1},   // == start
		{Labels: labels, Timestamp: f.at(60), Value: 2},  // mid
		{Labels: labels, Timestamp: f.at(120), Value: 3}, // == end
	}

	// step=60s, walk = 0, 60, 120 (3 anchors, start AND end included).
	got := toMatrixStepGrid(samples, f.at(0), f.at(120), 60*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 3 {
		t.Fatalf("expected 3 step anchors (start+end inclusive), got %d", len(got[0].Values))
	}
	// Each anchor sees its own sample (latest-at-or-before == the
	// sample sitting exactly at the anchor).
	wantVals := []string{"1", "2", "3"}
	for i, want := range wantVals {
		if got[0].Values[i][1] != want {
			t.Errorf("step %d: value=%v, want %v", i, got[0].Values[i][1], want)
		}
	}
}

// TestToMatrixStepGrid_OutOfRangeClip — rows whose Timestamp falls
// outside `[start, end]` are clipped from the output (a drifted
// server-side `now64(9)` on the instant fallback shape can land a
// row past `end`; the pivot must not surface it on the wire).
func TestToMatrixStepGrid_OutOfRangeClip(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(-30), Value: 1}, // before start
		{Labels: labels, Timestamp: f.at(0), Value: 2},   // == start (kept)
		{Labels: labels, Timestamp: f.at(60), Value: 3},  // inside (kept)
		{Labels: labels, Timestamp: f.at(120), Value: 4}, // == end (kept)
		{Labels: labels, Timestamp: f.at(150), Value: 5}, // past end
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(120), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 3 {
		t.Fatalf("expected 3 in-range values, got %d: %+v", len(got[0].Values), got[0].Values)
	}
	wantStamps := []float64{
		floatStamp(f.at(0)),
		floatStamp(f.at(60)),
		floatStamp(f.at(120)),
	}
	for i, want := range wantStamps {
		if got[0].Values[i][0] != want {
			t.Errorf("point %d: stamp=%v, want %v", i, got[0].Values[i][0], want)
		}
	}
	wantVals := []string{"2", "3", "4"}
	for i, want := range wantVals {
		if got[0].Values[i][1] != want {
			t.Errorf("point %d: value=%v, want %v", i, got[0].Values[i][1], want)
		}
	}
}

// TestToMatrixStepGrid_EmptyInput — zero samples produces zero
// matrix entries (not an empty matrix with N step-anchored series).
func TestToMatrixStepGrid_EmptyInput(t *testing.T) {
	t.Parallel()

	f := newFixture()
	got := toMatrixStepGrid(nil, f.at(0), f.at(300), 30*time.Second)
	if len(got) != 0 {
		t.Errorf("expected empty result for nil input, got %d series", len(got))
	}

	got = toMatrixStepGrid([]chclient.Sample{}, f.at(0), f.at(300), 30*time.Second)
	if len(got) != 0 {
		t.Errorf("expected empty result for empty input, got %d series", len(got))
	}
}

// TestToMatrixStepGrid_OutOfOrderSamples — samples handed in with
// shuffled timestamps must produce the same output as ordered
// samples (the pre-walk sort.Slice path).
func TestToMatrixStepGrid_OutOfOrderSamples(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	// Ordered baseline.
	ordered := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 1},
		{Labels: labels, Timestamp: f.at(30), Value: 2},
		{Labels: labels, Timestamp: f.at(60), Value: 3},
	}
	// Same samples in reverse.
	reversed := []chclient.Sample{
		ordered[2], ordered[1], ordered[0],
	}

	a := toMatrixStepGrid(ordered, f.at(0), f.at(60), 30*time.Second)
	b := toMatrixStepGrid(reversed, f.at(0), f.at(60), 30*time.Second)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 series each, got %d and %d", len(a), len(b))
	}
	if len(a[0].Values) != len(b[0].Values) {
		t.Fatalf("ordered=%d values, reversed=%d", len(a[0].Values), len(b[0].Values))
	}
	for i := range a[0].Values {
		if a[0].Values[i] != b[0].Values[i] {
			t.Errorf("step %d differs: ordered=%v, reversed=%v",
				i, a[0].Values[i], b[0].Values[i])
		}
	}
}

// TestToMatrixStepGrid_LabelSetIdentity — two samples whose label
// maps share identical entries but were constructed independently
// must collapse into a single series (CanonicalKey grouping).
func TestToMatrixStepGrid_LabelSetIdentity(t *testing.T) {
	t.Parallel()

	f := newFixture()

	samples := []chclient.Sample{
		{Labels: map[string]string{"job": "api", "env": "prod"}, Timestamp: f.at(0), Value: 1},
		// Same labels, different map instance, different key insertion
		// order — CanonicalKey is deterministic across both.
		{Labels: map[string]string{"env": "prod", "job": "api"}, Timestamp: f.at(30), Value: 2},
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(30), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected label-equivalent samples to collapse into 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 2 {
		t.Errorf("expected 2 step values from collapsed series, got %d", len(got[0].Values))
	}
}

// TestToMatrixStepGrid_NoCarryForwardOverEmptyAnchors — the
// loki-compat regression scenario: a sparse `by (level)` series
// where the inner SQL filter (`WHERE length(window_vals) >= 1`)
// has already dropped empty-window anchors, leaving N rows scattered
// across the request window. The pivot must emit exactly N points,
// NOT back-fill the dropped anchors with the previous sample's
// value via the pre-existing last-value-forward lookback. Two
// samples at t=0 and t=600 with a step grid that would have visited
// every 60s anchor in between → 2 output points, not 11.
func TestToMatrixStepGrid_NoCarryForwardOverEmptyAnchors(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"level": "info"}

	// Two non-adjacent samples (10 minutes apart). Under the old
	// LVF semantics with a 5m lookback, anchors at 0, 60, …, 300
	// would all carry forward the first sample (6 points), then
	// anchors at 600, 660, …, 900 would carry forward the second
	// (6 points) — 12 total. The pass-through pivot emits 2.
	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 1},
		{Labels: labels, Timestamp: f.at(600), Value: 2},
	}

	got := toMatrixStepGrid(samples, f.at(0), f.at(900), 60*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 2 {
		t.Fatalf("expected 2 emitted points (NO carry-forward), got %d: %+v", len(got[0].Values), got[0].Values)
	}
	if got[0].Values[0][0] != floatStamp(f.at(0)) || got[0].Values[0][1] != "1" {
		t.Errorf("point 0=%+v, want (%v, \"1\")", got[0].Values[0], floatStamp(f.at(0)))
	}
	if got[0].Values[1][0] != floatStamp(f.at(600)) || got[0].Values[1][1] != "2" {
		t.Errorf("point 1=%+v, want (%v, \"2\")", got[0].Values[1], floatStamp(f.at(600)))
	}
}

// TestToMatrixStepGrid_StepLargerThanWindow — when step > (end - start)
// only the start anchor is in-range. Pins that the loop runs once
// (start is always emitted if it has a preceding sample), not zero
// times.
func TestToMatrixStepGrid_StepLargerThanWindow(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 1},
	}

	// step=120s, window=[0, 60s]. Only anchor 0 is `!After(end)`.
	got := toMatrixStepGrid(samples, f.at(0), f.at(60), 120*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 1 {
		t.Errorf("expected single anchor emission, got %d: %+v",
			len(got[0].Values), got[0].Values)
	}
}
