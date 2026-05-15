package loki

import (
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 7 unit tests for toMatrixStepGrid — the per-step bucketing
// helper /loki/api/v1/query_range hands to the metric path. Pool-CI's
// coverage audit (#375) flagged this function at 26.7%; the cases
// below pin every branch of the step walk: standard alignment, single
// sample, empty buckets, multi-stream interleaving, boundary
// inclusion, 5-min lookback, and label-set canonicalisation.

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

// TestToMatrixStepGrid_SingleSample — one sample in a 5-minute
// window. The sample sits at step anchor t=0 (epoch). The 5-min
// lookback means every subsequent anchor within 300s of t=0 also
// emits the same value (staleness carry-forward). Anchor at t=330s
// is >300s away → dropped.
func TestToMatrixStepGrid_SingleSample(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}

	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 42},
	}

	// Step every 30s out to 330s (12 anchors: 0, 30, …, 330).
	got := toMatrixStepGrid(samples, f.at(0), f.at(330), 30*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	// Anchors 0..300 (inclusive, 11 of them) emit the carried-
	// forward value 42; anchor 330 is >300s past the sample so
	// the lookback drops it.
	if len(got[0].Values) != 11 {
		t.Fatalf("expected 11 carried-forward steps (0..300s of lookback), got %d", len(got[0].Values))
	}
	// All carried-forward values must equal "42".
	for i, v := range got[0].Values {
		if v[1] != "42" {
			t.Errorf("step %d: value=%v, want \"42\"", i, v[1])
		}
	}
	// The stamps are the step anchors, not the sample timestamp.
	for i, v := range got[0].Values {
		want := floatStamp(f.at(i * 30))
		if v[0] != want {
			t.Errorf("step %d: stamp=%v, want %v", i, v[0], want)
		}
	}
}

// TestToMatrixStepGrid_LeadingEmptyBuckets — when no sample exists
// at-or-before a step anchor, the step is dropped (NOT zero-filled).
// First sample at t=120s; anchors at 0, 30, 60, 90 fall before any
// sample → cursor == 0 → continue branch.
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
	// Anchors with output: 120, 150, 180. Anchors 0, 30, 60, 90
	// have no preceding sample → dropped (NOT zero-filled).
	if len(got[0].Values) != 3 {
		t.Fatalf("expected 3 emitted steps (120, 150, 180), got %d: %+v", len(got[0].Values), got[0].Values)
	}
	wantStamps := []float64{
		floatStamp(f.at(120)),
		floatStamp(f.at(150)),
		floatStamp(f.at(180)),
	}
	for i, want := range wantStamps {
		if got[0].Values[i][0] != want {
			t.Errorf("step %d: stamp=%v, want %v", i, got[0].Values[i][0], want)
		}
	}
	// Latest-at-or-before semantics: t=120 sees value 7,
	// t=150 sees 8 (newer), t=180 carries forward 8.
	wantVals := []string{"7", "8", "8"}
	for i, want := range wantVals {
		if got[0].Values[i][1] != want {
			t.Errorf("step %d: value=%v, want %v", i, got[0].Values[i][1], want)
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
//  2. per-series cursor advances independently (one series's
//     sample timeline doesn't bleed into another's),
//  3. label-set identity is the CanonicalKey shape.
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
	// Interleave the input order to stress the sort.Slice path
	// inside toMatrixStepGrid (the function pre-sorts each series
	// by timestamp before walking).
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

	// Series A: 5 anchors emit (0, 30, 60, 90, 120).
	if len(got[0].Values) != 5 {
		t.Errorf("series A: expected 5 values, got %d: %+v", len(got[0].Values), got[0].Values)
	}
	// Series B: anchors 60, 90, 120 emit (120 carries forward 11
	// via lookback). Anchors 0 and 30 are leading-empty.
	if len(got[1].Values) != 3 {
		t.Errorf("series B: expected 3 values, got %d: %+v", len(got[1].Values), got[1].Values)
	}
	if got[1].Values[0][1] != "10" || got[1].Values[1][1] != "11" || got[1].Values[2][1] != "11" {
		t.Errorf("series B values=%+v, want [10 11 11]", got[1].Values)
	}
	// Series C: anchors 30, 60, 90, 120 all carry forward 99
	// (single sample at 30s, lookback covers them all). Anchor
	// 0 has no preceding sample → dropped.
	if len(got[2].Values) != 4 {
		t.Errorf("series C: expected 4 values, got %d: %+v", len(got[2].Values), got[2].Values)
	}
	for i, v := range got[2].Values {
		if v[1] != "99" {
			t.Errorf("series C step %d: value=%v, want 99", i, v[1])
		}
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

// TestToMatrixStepGrid_LookbackDrop — a sample older than 5 minutes
// at a given anchor doesn't carry forward: the staleness window is
// strictly `> lookback` (the >, not >=, branch is the one this
// pins). One sample at t=0; anchors at 5m exactly (carried) and
// 5m1s (dropped).
func TestToMatrixStepGrid_LookbackDrop(t *testing.T) {
	t.Parallel()

	f := newFixture()
	labels := map[string]string{"job": "api"}
	samples := []chclient.Sample{
		{Labels: labels, Timestamp: f.at(0), Value: 1},
	}

	// First call: range [0, 300s], step 300s. Two anchors: 0, 300s.
	// Anchor 300s is exactly lookback away (`t.Sub(latest) == lookback`)
	// — the `>` comparison keeps it.
	got := toMatrixStepGrid(samples, f.at(0), f.at(300), 300*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 2 {
		t.Errorf("expected 2 values at lookback boundary; got %d: %+v",
			len(got[0].Values), got[0].Values)
	}

	// Second call: range [0, 301s], step 301s. Anchor at 301s is
	// 1 second past the lookback — dropped via the `>` branch.
	got = toMatrixStepGrid(samples, f.at(0), f.at(301), 301*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected 1 series, got %d", len(got))
	}
	if len(got[0].Values) != 1 {
		t.Errorf("expected only anchor 0 to emit (anchor 301s dropped by lookback); got %d: %+v",
			len(got[0].Values), got[0].Values)
	}
	// The one surviving anchor is at t=0.
	if got[0].Values[0][0] != floatStamp(f.at(0)) {
		t.Errorf("surviving stamp=%v, want %v", got[0].Values[0][0], floatStamp(f.at(0)))
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
