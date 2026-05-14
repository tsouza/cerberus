package promql

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

// helpers ---------------------------------------------------------

// anchor is the wall-clock baseline these tests build datasets around.
// All test timestamps are offsets from anchor so the eval-ts boundary
// math is easy to read in failure logs.
var anchor = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// ts returns a dataset/eval timestamp `offsetSec` seconds after anchor,
// in unix milliseconds (the Prom convention the dataset model uses).
func ts(offsetSec int) int64 { return anchor.Add(time.Duration(offsetSec) * time.Second).UnixMilli() }

// evalSec returns the eval timestamp `offsetSec` seconds after anchor,
// in unix seconds (the property.Query convention).
func evalSec(offsetSec int) int64 { return anchor.Add(time.Duration(offsetSec) * time.Second).Unix() }

// build constructs a one-call dataset out of metric-name → labels →
// (ts_offset_sec, value)... triples.
func build(series ...property.SeriesData) property.Dataset {
	return property.Dataset{Metrics: &property.MetricsModel{Series: series}}
}

// makeSeries builds one SeriesData with samples at `offsetSec`
// seconds after anchor.
func makeSeries(name string, lbls map[string]string, samples ...sampleSpec) property.SeriesData {
	pts := make([]property.Point, len(samples))
	for i, s := range samples {
		pts[i] = property.Point{TimestampMs: ts(s.OffsetSec), Value: s.Val}
	}
	return property.SeriesData{MetricName: name, Labels: lbls, Points: pts}
}

type sampleSpec struct {
	OffsetSec int
	Val       float64
}

// eval is a thin wrapper around Evaluate using DefaultLookbackDelta.
func eval(d property.Dataset, q string, evalOffsetSec int) property.Outcome {
	return Evaluate(d, property.Query{String: q, EvalTs: evalSec(evalOffsetSec)}, Options{})
}

// rows sorts an outcome's rows by labelKey for stable assertions.
func rows(o property.Outcome) []property.OutcomeRow {
	out := append([]property.OutcomeRow(nil), o.Rows...)
	sort.Slice(out, func(i, j int) bool {
		return rowKey(out[i].Labels) < rowKey(out[j].Labels)
	})
	return out
}

func rowKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, m[k]...)
	}
	b = append(b, '}')
	return string(b)
}

// assertRows checks that the outcome has the expected rows by labels +
// value. Timestamps are not asserted (they're always evalTsMs by
// construction).
func assertRows(t *testing.T, o property.Outcome, want []property.OutcomeRow) {
	t.Helper()
	if o.Err != nil {
		t.Fatalf("unexpected oracle error: %v", o.Err)
	}
	got := rows(o)
	w := append([]property.OutcomeRow(nil), want...)
	sort.Slice(w, func(i, j int) bool { return rowKey(w[i].Labels) < rowKey(w[j].Labels) })
	if len(got) != len(w) {
		t.Fatalf("row count: want=%d got=%d\nwant=%v\ngot=%v", len(w), len(got), w, got)
	}
	for i := range got {
		if rowKey(got[i].Labels) != rowKey(w[i].Labels) {
			t.Errorf("row[%d] labels: want=%v got=%v", i, w[i].Labels, got[i].Labels)
		}
		if !valuesClose(got[i].Value, w[i].Value) {
			t.Errorf("row[%d] value: want=%g got=%g", i, w[i].Value, got[i].Value)
		}
	}
}

func valuesClose(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	if d <= eps {
		return true
	}
	scale := math.Abs(a)
	if math.Abs(b) > scale {
		scale = math.Abs(b)
	}
	return d <= eps*scale
}

func row(lbls map[string]string, v float64) property.OutcomeRow {
	return property.OutcomeRow{Labels: lbls, Value: v}
}

// =================================================================
// Selector tests — 10
// =================================================================

func TestSelector_BareSelector_OneSeries(t *testing.T) {
	d := build(makeSeries("up", map[string]string{"job": "api"},
		sampleSpec{60, 1}, sampleSpec{120, 1}))
	o := eval(d, `up{job="api"}`, 150)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"job": "api"}, 1)})
}

func TestSelector_LWR_AtExactBoundary(t *testing.T) {
	// Eval ts EXACTLY at the latest sample's ts — must be included
	// (LWR boundary is `T <= eval_ts`, inclusive).
	d := build(makeSeries("up", map[string]string{"job": "api"},
		sampleSpec{60, 5}))
	o := eval(d, `up{job="api"}`, 60)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"job": "api"}, 5)})
}

func TestSelector_LWR_PastBoundary(t *testing.T) {
	// Eval ts BEFORE the only sample — no series should match.
	d := build(makeSeries("up", map[string]string{"job": "api"},
		sampleSpec{60, 5}))
	o := eval(d, `up{job="api"}`, 30)
	assertRows(t, o, nil)
}

func TestSelector_StalenessGap_Trips(t *testing.T) {
	// Eval ts MORE than 5min after the last sample → stale, drop.
	d := build(makeSeries("up", map[string]string{"job": "api"},
		sampleSpec{60, 5}))
	o := eval(d, `up{job="api"}`, 60+int(DefaultLookbackDelta.Seconds()))
	assertRows(t, o, nil)
}

func TestSelector_StalenessGap_JustInside(t *testing.T) {
	// Eval ts EXACTLY one ms inside the lookback delta — still fresh.
	d := build(makeSeries("up", map[string]string{"job": "api"},
		sampleSpec{60, 5}))
	q := property.Query{
		String: `up{job="api"}`,
		EvalTs: time.UnixMilli(ts(60) + DefaultLookbackDelta.Milliseconds() - 1).Unix(),
	}
	o := Evaluate(d, q, Options{})
	if len(o.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", o.Rows)
	}
}

func TestSelector_LabelMatcher_Equal(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "web"}, sampleSpec{60, 2}),
	)
	o := eval(d, `up{job="api"}`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"job": "api"}, 1)})
}

func TestSelector_LabelMatcher_NotEqual(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "web"}, sampleSpec{60, 2}),
	)
	o := eval(d, `up{job!="api"}`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"job": "web"}, 2)})
}

func TestSelector_LabelMatcher_Regex(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "web"}, sampleSpec{60, 2}),
		makeSeries("up", map[string]string{"job": "batch"}, sampleSpec{60, 3}),
	)
	o := eval(d, `up{job=~"a.+|w.+"}`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 1),
		row(map[string]string{"job": "web"}, 2),
	})
}

func TestSelector_NoMatchingSeries(t *testing.T) {
	d := build(makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 1}))
	o := eval(d, `down{job="api"}`, 90)
	assertRows(t, o, nil)
}

func TestSelector_EmptyLabelSet(t *testing.T) {
	d := build(property.SeriesData{
		MetricName: "up",
		Labels:     map[string]string{},
		Points:     []property.Point{{TimestampMs: ts(60), Value: 7}},
	})
	o := eval(d, `up`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

// =================================================================
// Function tests — 8
// =================================================================

func TestFn_Rate_BasicCounter(t *testing.T) {
	// Five samples 15s apart, values 0..40 (gain of 40 over 60s span).
	// Range [60s] at evalTs=15+60=75s. Window is (15, 75].
	// Samples in window: t=15(10), t=30(20), t=45(30), t=60(40).
	// rate semantics: extrapolatedRate with 4 samples, isCounter=true,
	// isRate=true. Result is ~ (40-10)/60 = 0.5 per second, with
	// edge extrapolation. We assert >0.4 and <0.7.
	d := build(makeSeries("requests_total", map[string]string{"job": "api"},
		sampleSpec{0, 0}, sampleSpec{15, 10}, sampleSpec{30, 20},
		sampleSpec{45, 30}, sampleSpec{60, 40}))
	o := eval(d, `rate(requests_total{job="api"}[60s])`, 75)
	if len(o.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", o.Rows)
	}
	v := o.Rows[0].Value
	if v < 0.4 || v > 0.8 {
		t.Errorf("rate value out of expected range [0.4, 0.8]: got=%g", v)
	}
}

func TestFn_Rate_CounterReset(t *testing.T) {
	// Counter resets from 50 → 10 in the middle of the window.
	// extrapolated rate should still positive (reset is recovered).
	d := build(makeSeries("requests_total", map[string]string{"job": "api"},
		sampleSpec{0, 0}, sampleSpec{15, 50}, sampleSpec{30, 10},
		sampleSpec{45, 30}, sampleSpec{60, 50}))
	o := eval(d, `rate(requests_total{job="api"}[60s])`, 75)
	if len(o.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", o.Rows)
	}
	if o.Rows[0].Value <= 0 {
		t.Errorf("rate after counter reset must be positive, got=%g", o.Rows[0].Value)
	}
}

func TestFn_Increase_AcrossWindow(t *testing.T) {
	// Counter grows 0 → 60. Increase across the window should be
	// ~60 (with edge extrapolation similar to rate but without the
	// /seconds divisor).
	d := build(makeSeries("c", map[string]string{},
		sampleSpec{0, 0}, sampleSpec{15, 15}, sampleSpec{30, 30},
		sampleSpec{45, 45}, sampleSpec{60, 60}))
	o := eval(d, `increase(c[60s])`, 75)
	if len(o.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", o.Rows)
	}
	if o.Rows[0].Value < 40 || o.Rows[0].Value > 80 {
		t.Errorf("increase out of expected range [40, 80]: got=%g", o.Rows[0].Value)
	}
}

func TestFn_Delta_NegativeDelta(t *testing.T) {
	// Gauge drops from 50 → 10 → -delta should be negative.
	d := build(makeSeries("g", map[string]string{},
		sampleSpec{0, 50}, sampleSpec{30, 30}, sampleSpec{60, 10}))
	o := eval(d, `delta(g[60s])`, 75)
	if len(o.Rows) != 1 {
		t.Fatalf("expected 1 row, got %v", o.Rows)
	}
	if o.Rows[0].Value >= 0 {
		t.Errorf("delta must be negative, got=%g", o.Rows[0].Value)
	}
}

func TestFn_SumOverTime_Basic(t *testing.T) {
	d := build(makeSeries("g", map[string]string{},
		sampleSpec{0, 1}, sampleSpec{15, 2}, sampleSpec{30, 3}, sampleSpec{45, 4}))
	// Window (15, 60] → captures samples at 30 and 45 → sum=7.
	o := eval(d, `sum_over_time(g[45s])`, 60)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

func TestFn_AvgOverTime_Basic(t *testing.T) {
	d := build(makeSeries("g", map[string]string{},
		sampleSpec{0, 2}, sampleSpec{30, 4}, sampleSpec{60, 6}))
	// Window (0, 60] → captures t=30 (4) and t=60 (6) → mean=5.
	o := eval(d, `avg_over_time(g[60s])`, 60)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 5)})
}

func TestFn_MinMaxCountOverTime(t *testing.T) {
	d := build(makeSeries("g", map[string]string{},
		sampleSpec{0, 5}, sampleSpec{30, 2}, sampleSpec{60, 10}))
	// Window (0, 60] → samples 2, 10. min=2, max=10, count=2.
	if o := eval(d, `min_over_time(g[60s])`, 60); o.Rows[0].Value != 2 {
		t.Errorf("min: want=2 got=%g", o.Rows[0].Value)
	}
	if o := eval(d, `max_over_time(g[60s])`, 60); o.Rows[0].Value != 10 {
		t.Errorf("max: want=10 got=%g", o.Rows[0].Value)
	}
	if o := eval(d, `count_over_time(g[60s])`, 60); o.Rows[0].Value != 2 {
		t.Errorf("count: want=2 got=%g", o.Rows[0].Value)
	}
}

func TestFn_OverTime_EmptyWindow(t *testing.T) {
	// Sample at t=0; window (60, 120] is empty → no rows.
	d := build(makeSeries("g", map[string]string{}, sampleSpec{0, 5}))
	o := eval(d, `sum_over_time(g[60s])`, 120)
	assertRows(t, o, nil)
}

// =================================================================
// Aggregation tests — 12
// =================================================================

func TestAgg_Sum_NoGrouping(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "web"}, sampleSpec{60, 2}),
		makeSeries("up", map[string]string{"job": "batch"}, sampleSpec{60, 3}),
	)
	o := eval(d, `sum(up)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 6)})
}

func TestAgg_Sum_By(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api", "instance": "a"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "api", "instance": "b"}, sampleSpec{60, 2}),
		makeSeries("up", map[string]string{"job": "web", "instance": "a"}, sampleSpec{60, 3}),
	)
	o := eval(d, `sum by(job) (up)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 3),
		row(map[string]string{"job": "web"}, 3),
	})
}

func TestAgg_Sum_Without(t *testing.T) {
	d := build(
		makeSeries("up", map[string]string{"job": "api", "instance": "a"}, sampleSpec{60, 1}),
		makeSeries("up", map[string]string{"job": "api", "instance": "b"}, sampleSpec{60, 2}),
		makeSeries("up", map[string]string{"job": "web", "instance": "a"}, sampleSpec{60, 3}),
	)
	// without(instance) → group by job.
	o := eval(d, `sum without(instance) (up)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 3),
		row(map[string]string{"job": "web"}, 3),
	})
}

func TestAgg_Avg_By(t *testing.T) {
	// Two series in "job=a" with values 10 and 20 — add an
	// instance label so the underlying SeriesData rows stay
	// distinct.
	d := build(
		makeSeries("g", map[string]string{"job": "a", "inst": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"job": "a", "inst": "2"}, sampleSpec{60, 20}),
		makeSeries("g", map[string]string{"job": "b", "inst": "1"}, sampleSpec{60, 5}),
	)
	o := eval(d, `avg by(job) (g)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "a"}, 15),
		row(map[string]string{"job": "b"}, 5),
	})
}

func TestAgg_Min_NoGrouping(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 3}),
		makeSeries("g", map[string]string{"i": "3"}, sampleSpec{60, 7}),
	)
	o := eval(d, `min(g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 3)})
}

func TestAgg_Max_NoGrouping(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 3}),
	)
	o := eval(d, `max(g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 10)})
}

func TestAgg_Count_NoGrouping(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 3}),
		makeSeries("g", map[string]string{"i": "3"}, sampleSpec{60, 7}),
	)
	o := eval(d, `count(g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 3)})
}

func TestAgg_Sum_EmptyGroup(t *testing.T) {
	// No matching series → empty output.
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 1}))
	o := eval(d, `sum(other_metric)`, 90)
	assertRows(t, o, nil)
}

func TestAgg_Topk_2(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 3}),
		makeSeries("g", map[string]string{"i": "3"}, sampleSpec{60, 7}),
	)
	o := eval(d, `topk(2, g)`, 90)
	// Top-2: i=1 (10), i=3 (7).
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"i": "1"}, 10),
		row(map[string]string{"i": "3"}, 7),
	})
}

func TestAgg_Bottomk_1(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 10}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 3}),
		makeSeries("g", map[string]string{"i": "3"}, sampleSpec{60, 7}),
	)
	o := eval(d, `bottomk(1, g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"i": "2"}, 3)})
}

func TestAgg_SumByEmpty(t *testing.T) {
	// `by ()` aggregates to one row with empty labels.
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 1}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 2}),
	)
	o := eval(d, `sum by() (g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 3)})
}

func TestAgg_PerSeries_LWR(t *testing.T) {
	// CRITICAL: aggregation MUST first reduce each series to its LWR
	// sample, THEN combine. Naive impl (sum all stored samples) would
	// return 1+2+3+4 = 10 instead of just-the-latest 2+4 = 6.
	d := build(
		property.SeriesData{
			MetricName: "g",
			Labels:     map[string]string{"i": "1"},
			Points: []property.Point{
				{TimestampMs: ts(0), Value: 1},
				{TimestampMs: ts(60), Value: 2},
			},
		},
		property.SeriesData{
			MetricName: "g",
			Labels:     map[string]string{"i": "2"},
			Points: []property.Point{
				{TimestampMs: ts(0), Value: 3},
				{TimestampMs: ts(60), Value: 4},
			},
		},
	)
	o := eval(d, `sum(g)`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 6)})
}

// =================================================================
// Binary tests — 8
// =================================================================

func TestBinary_ScalarScalar(t *testing.T) {
	d := build()
	o := eval(d, `2 + 3`, 60)
	// Top-level scalar surfaces as a single row with empty labels.
	if len(o.Rows) != 1 || o.Rows[0].Value != 5 {
		t.Fatalf("want value=5, got %v", o.Rows)
	}
}

func TestBinary_ScalarVector(t *testing.T) {
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 5}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 10}),
	)
	o := eval(d, `g * 2`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"i": "1"}, 10),
		row(map[string]string{"i": "2"}, 20),
	})
}

func TestBinary_VectorScalar_Filter(t *testing.T) {
	// Comparison without bool: rows where the comparison is false
	// are DROPPED (not turned into 0). g > 6 should keep only g=10.
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 5}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 10}),
	)
	o := eval(d, `g > 6`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"i": "2"}, 10)})
}

func TestBinary_VectorScalar_Bool(t *testing.T) {
	// Same op with bool modifier: every row stays, value is 0/1.
	d := build(
		makeSeries("g", map[string]string{"i": "1"}, sampleSpec{60, 5}),
		makeSeries("g", map[string]string{"i": "2"}, sampleSpec{60, 10}),
	)
	o := eval(d, `g > bool 6`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"i": "1"}, 0),
		row(map[string]string{"i": "2"}, 1),
	})
}

func TestBinary_VectorVector_Match(t *testing.T) {
	// One-to-one matching on the implicit "all labels match" rule.
	d := build(
		makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 10}),
		makeSeries("a", map[string]string{"job": "web"}, sampleSpec{60, 20}),
		makeSeries("b", map[string]string{"job": "api"}, sampleSpec{60, 1}),
		makeSeries("b", map[string]string{"job": "web"}, sampleSpec{60, 2}),
	)
	o := eval(d, `a + b`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api"}, 11),
		row(map[string]string{"job": "web"}, 22),
	})
}

func TestBinary_VectorVector_GroupLeft(t *testing.T) {
	// One a-series, two b-series. group_left means LHS is the "many"
	// side. Match on `job` only (ignoring=instance).
	d := build(
		makeSeries("a", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 10}),
		makeSeries("a", map[string]string{"job": "api", "instance": "y"}, sampleSpec{60, 20}),
		makeSeries("b", map[string]string{"job": "api"}, sampleSpec{60, 100}),
	)
	o := eval(d, `a + on(job) group_left() b`, 90)
	// LHS keeps its instance labels in the output.
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api", "instance": "x"}, 110),
		row(map[string]string{"job": "api", "instance": "y"}, 120),
	})
}

func TestBinary_VectorVector_GroupRight(t *testing.T) {
	d := build(
		makeSeries("a", map[string]string{"job": "api"}, sampleSpec{60, 100}),
		makeSeries("b", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 10}),
		makeSeries("b", map[string]string{"job": "api", "instance": "y"}, sampleSpec{60, 20}),
	)
	o := eval(d, `a + on(job) group_right() b`, 90)
	// RHS keeps its instance labels in the output.
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"job": "api", "instance": "x"}, 110),
		row(map[string]string{"job": "api", "instance": "y"}, 120),
	})
}

func TestBinary_VectorVector_OnIgnore(t *testing.T) {
	// Same instances on both sides; match on `job` only via ignoring.
	d := build(
		makeSeries("a", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 10}),
		makeSeries("b", map[string]string{"job": "api", "instance": "x"}, sampleSpec{60, 1}),
	)
	o := eval(d, `a + ignoring(instance) b`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{"job": "api"}, 11)})
}

// =================================================================
// Histogram tests — 6
// =================================================================

// makeHist builds a classic-histogram dataset for the given bucket
// boundaries and cumulative counts at t=60s.
func makeHist(buckets []histBucketSpec) property.Dataset {
	out := make([]property.SeriesData, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, makeSeries("h_bucket", map[string]string{"le": b.LE},
			sampleSpec{60, b.Count}))
	}
	return build(out...)
}

type histBucketSpec struct {
	LE    string
	Count float64
}

func TestHistogram_Quantile_0_5(t *testing.T) {
	d := makeHist([]histBucketSpec{
		{LE: "1", Count: 5},
		{LE: "5", Count: 10},
		{LE: "+Inf", Count: 10},
	})
	o := eval(d, `histogram_quantile(0.5, h_bucket)`, 90)
	if len(o.Rows) != 1 {
		t.Fatalf("want 1 row, got %v", o.Rows)
	}
	// rank = 0.5 * 10 = 5. Bucket-0 has count=5, so we hit b=0,
	// bucketStart=0, bucketEnd=1, count=5, rank=5. Result = 0 + (1-0)*(5/5) = 1.
	if !valuesClose(o.Rows[0].Value, 1) {
		t.Errorf("median: want=1, got=%g", o.Rows[0].Value)
	}
}

func TestHistogram_Quantile_0_95(t *testing.T) {
	d := makeHist([]histBucketSpec{
		{LE: "1", Count: 5},
		{LE: "5", Count: 10},
		{LE: "+Inf", Count: 10},
	})
	o := eval(d, `histogram_quantile(0.95, h_bucket)`, 90)
	if len(o.Rows) != 1 {
		t.Fatalf("want 1 row, got %v", o.Rows)
	}
	// rank = 0.95 * 10 = 9.5. b=1: count[1]=10, count[0]=5. b=1.
	// bucketStart=1, bucketEnd=5, bucketCount=5, rank-prev=4.5.
	// Result = 1 + (5-1)*(4.5/5) = 1 + 3.6 = 4.6.
	if !valuesClose(o.Rows[0].Value, 4.6) {
		t.Errorf("p95: want=4.6, got=%g", o.Rows[0].Value)
	}
}

func TestHistogram_Quantile_0(t *testing.T) {
	d := makeHist([]histBucketSpec{
		{LE: "1", Count: 5},
		{LE: "5", Count: 10},
		{LE: "+Inf", Count: 10},
	})
	o := eval(d, `histogram_quantile(0, h_bucket)`, 90)
	// rank = 0, b=0, bucketStart=0, bucketEnd=1, count=5, rank=0.
	// Result = 0 + (1)*(0/5) = 0.
	if len(o.Rows) != 1 || !valuesClose(o.Rows[0].Value, 0) {
		t.Fatalf("p0: want=0, got=%v", o.Rows)
	}
}

func TestHistogram_Quantile_1(t *testing.T) {
	d := makeHist([]histBucketSpec{
		{LE: "1", Count: 5},
		{LE: "5", Count: 10},
		{LE: "+Inf", Count: 10},
	})
	o := eval(d, `histogram_quantile(1.0, h_bucket)`, 90)
	// rank = 10 = total. b ends at len-1 (the +Inf bucket). Returns
	// the lower bound of the last finite bucket = 5.
	if len(o.Rows) != 1 || !valuesClose(o.Rows[0].Value, 5) {
		t.Fatalf("p100: want=5, got=%v", o.Rows)
	}
}

func TestHistogram_Quantile_EmptyBuckets(t *testing.T) {
	d := build()
	o := eval(d, `histogram_quantile(0.5, h_bucket)`, 90)
	// No buckets, no output.
	assertRows(t, o, nil)
}

func TestHistogram_Quantile_SingleBucket(t *testing.T) {
	d := makeHist([]histBucketSpec{
		{LE: "+Inf", Count: 5},
	})
	o := eval(d, `histogram_quantile(0.5, h_bucket)`, 90)
	// Only the +Inf bucket; len < 2 → NaN per Prom.
	if len(o.Rows) != 1 {
		t.Fatalf("want 1 row, got %v", o.Rows)
	}
	if !math.IsNaN(o.Rows[0].Value) {
		t.Errorf("single-bucket: want NaN, got %g", o.Rows[0].Value)
	}
}

// =================================================================
// Modifier tests — 6
// =================================================================

func TestMod_PositiveOffset(t *testing.T) {
	// `g offset 60s` at eval=120 looks back at 120-60=60s.
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 7}))
	o := eval(d, `g offset 60s`, 120)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

func TestMod_ZeroOffset(t *testing.T) {
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 7}))
	o := eval(d, `g offset 0s`, 60)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

func TestMod_NegativeOffset(t *testing.T) {
	// `g offset -60s` at eval=60 looks back at 60-(-60)=120s.
	d := build(makeSeries("g", map[string]string{},
		sampleSpec{60, 5}, sampleSpec{120, 9}))
	o := eval(d, `g offset -60s`, 60)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 9)})
}

func TestMod_AtTimestamp(t *testing.T) {
	// `@<unix_seconds>` overrides the effective eval ts. The @ value
	// is in unix seconds, so we build the query string with the
	// dataset sample's effective unix ts. Eval ts is far past (where
	// the staleness rule would otherwise hide the sample), but @
	// brings the effective lookup right back to the sample's time.
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 7}))
	sampleUnixSec := anchor.Add(60 * time.Second).Unix()
	q := fmt.Sprintf(`g @%d`, sampleUnixSec)
	o := eval(d, q, 90+24*3600 /* well past lookback */)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

func TestMod_AtStart(t *testing.T) {
	// For instant queries, @start() == eval ts == 90 (which is fresh
	// with respect to the t=60 sample).
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 7}))
	o := eval(d, `g @start()`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}

func TestMod_AtEnd(t *testing.T) {
	d := build(makeSeries("g", map[string]string{}, sampleSpec{60, 7}))
	o := eval(d, `g @end()`, 90)
	assertRows(t, o, []property.OutcomeRow{row(map[string]string{}, 7)})
}
