//go:build chdb

// chDB-backed regression coverage for the Pool-AK range-mode rework.
//
// Before this rework, `/api/v1/query_range` lowerings for bare
// vector selectors and aggregations applied PromQL's Latest-With-
// Respect-to-T (LWR) collapse once at the request's end_ts, producing
// a single row per series. The matrix-pivot step loop in the handler
// then dropped every step outside the 5-minute staleness window — for
// a 1-hour range query at step=30s, fewer than ~10 of the requested
// 121 step buckets actually received data. Pool-AI's compatibility
// run surfaced this family as 363+ shape-diffs.
//
// The fix: in range mode (request `step > 0`) the bare-selector
// lowering builds a per-step LWR by cross-joining the raw scan with a
// `chplan.StepGrid` anchor source, filtering per-anchor to the
// `(anchor_ts - 5m, anchor_ts]` window, and collapsing
// `argMax(Value, TimeUnix) GROUP BY (series, anchor_ts)`. The outer
// Project re-aliases `anchor_ts → TimeUnix` so the canonical Sample
// contract holds for downstream consumers (aggregations, instant fns,
// arithmetic). Aggregations gain an extra `TimeUnix` group key so
// `sum(metric)` over query_range produces per-step aggregates.
//
// These tests pin the end-to-end matrix shape against chDB for the
// shapes Pool-AI identified as compatibility blockers:
//
//   - Bare selector over query_range → N samples per series matching
//     the step grid.
//   - `sum(metric)` over query_range → N per-step aggregates.
//   - `rate(metric[5m])` over query_range → matrix path via
//     RangeWindow's OuterRange + Step fan-out (RC-critical control
//     for the matrix RangeWindow change in lowerRangeVectorCall).
//   - Empty-window cases drop the step.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// TestQueryRange_RangeMode_BareSelector_ChDB pins the bare-selector
// `/api/v1/query_range` path. Pool-AK's rework emits one row per
// (series, step anchor) where the LWR window has data; the matrix
// pivot then renders one sample per step in the requested range.
//
// Seeded: a single `demo_memory_usage_bytes` sample per step (every
// 30s) for 5 minutes; expected: 11 samples per series at each step.
func TestQueryRange_RangeMode_BareSelector_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	seedRows := make([]string, 0, wantSamples)
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_memory_usage_bytes', map('instance', 'demo'), toDateTime64('%s', 9), %d.0)`,
			ts, 100+i,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"

	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"demo_memory_usage_bytes", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %+v", len(matrix), matrix)
	}
	values := matrix[0].Values
	if len(values) != wantSamples {
		t.Fatalf("expected %d samples (one per step), got %d: %+v",
			wantSamples, len(values), values)
	}
	for i, v := range values {
		// Each step's sample is the LWR-collapsed value at that anchor;
		// with one sample per step the LWR collapses to that sample.
		wantVal := strconv.FormatFloat(float64(100+i), 'f', -1, 64)
		if got := v[1]; got != wantVal {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, wantVal, v)
		}
	}
}

// TestQueryRange_RangeMode_SumAggregation_ChDB pins the
// `sum(metric)` path under `/api/v1/query_range`. The rework injects
// `TimeUnix` into the Aggregate's GROUP BY so each step bucket
// produces its own per-step aggregate row.
//
// Two series with values `(100+i, 200+i)` at each of 11 step
// anchors → `sum` over the full set should be `300 + 2*i` at step i.
func TestQueryRange_RangeMode_SumAggregation_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	seedRows := make([]string, 0, wantSamples*2)
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows,
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'a'), toDateTime64('%s', 9), %d.0)`,
				ts, 100+i),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'b'), toDateTime64('%s', 9), %d.0)`,
				ts, 200+i),
		)
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"sum(demo_memory_usage_bytes)", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series from sum (no by/without), got %d: %+v",
			len(matrix), matrix)
	}
	values := matrix[0].Values
	if len(values) != wantSamples {
		t.Fatalf("expected %d per-step aggregates, got %d: %+v",
			wantSamples, len(values), values)
	}
	for i, v := range values {
		want := strconv.FormatFloat(float64(300+2*i), 'f', -1, 64)
		if got := v[1]; got != want {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, want, v)
		}
	}
}

// TestQueryRange_RangeMode_SumByLabel_ChDB pins `sum by (job) (metric)`
// over query_range. Two distinct `job` series with three instances
// each — `sum by(job)` over query_range should emit 2 series × N
// per-step samples.
func TestQueryRange_RangeMode_SumByLabel_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	// Seed: job=api {a, b, c}; job=db {a, b}. Values constant per
	// (job, instance) — the per-step sum within each job is then a
	// stable constant across all 11 anchors.
	seedRows := []string{}
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		// api job — 3 instances, each Value=1.0 → sum=3.0
		seedRows = append(seedRows,
			fmt.Sprintf(`('up', map('job', 'api', 'instance', 'a'), toDateTime64('%s', 9), 1.0)`, ts),
			fmt.Sprintf(`('up', map('job', 'api', 'instance', 'b'), toDateTime64('%s', 9), 1.0)`, ts),
			fmt.Sprintf(`('up', map('job', 'api', 'instance', 'c'), toDateTime64('%s', 9), 1.0)`, ts),
			// db job — 2 instances, each Value=1.0 → sum=2.0
			fmt.Sprintf(`('up', map('job', 'db', 'instance', 'a'), toDateTime64('%s', 9), 1.0)`, ts),
			fmt.Sprintf(`('up', map('job', 'db', 'instance', 'b'), toDateTime64('%s', 9), 1.0)`, ts),
		)
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"sum by (job) (up)", start, end, step)
	if len(matrix) != 2 {
		t.Fatalf("expected 2 series (one per distinct job), got %d: %+v",
			len(matrix), matrix)
	}
	wantPerJob := map[string]string{"api": "3", "db": "2"}
	for _, ms := range matrix {
		job := ms.Metric["job"]
		want, ok := wantPerJob[job]
		if !ok {
			t.Errorf("unexpected series: %+v", ms.Metric)
			continue
		}
		if len(ms.Values) != wantSamples {
			t.Errorf("job=%s: expected %d samples, got %d: %+v",
				job, wantSamples, len(ms.Values), ms.Values)
			continue
		}
		for i, v := range ms.Values {
			if got := v[1]; got != want {
				t.Errorf("job=%s step %d: value=%q want=%q", job, i, got, want)
			}
		}
	}
}

// TestQueryRange_RangeMode_AvgOverTime_ChDB pins
// `avg_over_time(metric[5m])` under query_range. The rework also
// routes range-vector lowerings to the matrix path (OuterRange + Step
// set) so each step anchor emits its own per-window aggregation.
//
// Seeded: a single demo_memory_usage_bytes gauge sample at each step.
// Anchors land at `end - i*step` for i in `[0, 10]` (11 total); the
// matrix-RangeWindow `length(window_vals) >= 1` predicate drops any
// anchor whose 5-minute lookback window held no sample. Pinning to
// "at least multiple per-step rows" rather than an exact count keeps
// the test resilient to per-CI-clock sub-second drift (seed rows are
// anchored on `time.Now()` which has nanosecond precision while
// `start.Unix()` truncates to seconds).
func TestQueryRange_RangeMode_AvgOverTime_ChDB(t *testing.T) {
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_memory_usage_bytes', map('job', 'api'), toDateTime64('%s', 9), %d.0)`,
			ts, 100+i,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"avg_over_time(demo_memory_usage_bytes[5m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series; got 0")
	}
	// Pre-Pool-AK the matrix RangeWindow path defaulted to a single
	// anchor (Step=0, OuterRange=0) and the matrix pivot extended its
	// one row across the step grid — surfaced as one repeated value
	// for the whole window. With the rework, the SQL fans out per
	// anchor on the step grid: assert > 1 distinct sample point so
	// the regression is pinned without coupling to clock drift.
	if got := len(matrix[0].Values); got < 5 {
		t.Fatalf("expected per-step avg_over_time matrix (>= 5 samples); got %d: %+v",
			got, matrix[0].Values)
	}
	// Per-step values must be monotonically non-decreasing in this
	// seed (each subsequent anchor's window includes one more sample
	// at the high end). A regression where every step carried the
	// same value would surface as all-equal values.
	first := matrix[0].Values[0][1]
	last := matrix[0].Values[len(matrix[0].Values)-1][1]
	if first == last {
		t.Errorf("expected per-step variation across the matrix; got first=last=%q (full row: %+v)",
			first, matrix[0].Values)
	}
}

// TestQueryRange_RangeMode_EmptyWindow_ChDB pins the "no samples in
// LWR window" behaviour for the bare-selector range-mode path. The
// data is seeded only at the very start; later anchors whose window
// `(t-5m, t]` contains no sample MUST not emit a matrix point.
//
// With a 12-minute query span at step=1m, the first 6 anchors (0–5m)
// have the seed sample inside their LWR window; the remaining 7
// anchors (6m–12m) do not. The matrix must therefore expose exactly
// 6 sample points — the Pool-AK guarantee that per-step LWR
// faithfully reflects "Prom drops empty windows".
func TestQueryRange_RangeMode_EmptyWindow_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(12 * time.Minute)
	step := time.Minute

	// One sample at `start`; nothing else.
	ts := start.Format("2006-01-02 15:04:05.000000000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('sparse_metric', map('instance', 'demo'), toDateTime64('%s', 9), 42.0);`, ts)
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"sparse_metric", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %+v", len(matrix), matrix)
	}
	values := matrix[0].Values
	// LWR window is strict-lower-bound (`>` not `>=`) on the lower
	// side, so the sample at exactly anchor=start IS in its own
	// window `(start-5m, start]`. Subsequent anchors at start+i*step
	// see the sample in their window as long as `start > t - 5m`
	// (strict), i.e., `t - start < 5m`. With step=1m the qualifying
	// anchors are t = start, start+1m, ..., start+4m (5 anchors —
	// start+5m fails `t - start < 5m`).
	const wantSamples = 5
	if got := len(values); got != wantSamples {
		t.Fatalf("expected %d samples for sparse seed (within-lookback anchors); got %d: %+v",
			wantSamples, got, values)
	}
	// Every emitted sample carries the same Value (the only seed row).
	for i, v := range values {
		if got := v[1]; got != "42" {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, "42", v)
		}
	}
}

// TestQueryRange_RangeMode_RateMatrix_ChDB pins `rate(metric[5m])`
// over `/api/v1/query_range`. The matrix-RangeWindow path emits one
// row per anchor in [start, end] spaced by step. The pre-Pool-AL bug:
// some matrix-RangeWindow lowerings forgot to thread ctx.step into the
// chplan.RangeWindow, so the emitter defaulted to a single anchor at
// end_ts and the matrix pivot delivered a single repeated value. This
// test pins the per-anchor fan-out and the per-step rate variation.
//
// Seeded: a monotonically-increasing counter starting at 0, increasing
// by 60 every 30s (i.e. constant 2/s rate). At each step anchor the
// rate over the prior 5m sees the linear ramp and emits 2.0 (modulo
// boundary effects on the first few anchors when the window holds
// fewer than 2 samples). The metric name is gauge-routed (no `_total`
// suffix) so the seed lives in `otel_metrics_gauge` alongside the
// other range-mode fixtures.
func TestQueryRange_RangeMode_RateMatrix_ChDB(t *testing.T) {
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_cpu_usage_seconds', map('job', 'api'), toDateTime64('%s', 9), %d.0)`,
			ts, i*60,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"rate(demo_cpu_usage_seconds[5m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series for rate matrix; got 0")
	}
	// Pre-Pool-AL the matrix RangeWindow path could degenerate to a
	// single anchor when Step / OuterRange weren't threaded. Assert
	// multi-anchor fan-out so the regression is pinned without
	// coupling to the exact per-anchor count (which depends on the
	// 2-sample minWindowSize gate for rate).
	if got := len(matrix[0].Values); got < 5 {
		t.Fatalf("expected per-step rate matrix (>= 5 samples); got %d: %+v",
			got, matrix[0].Values)
	}
}

// TestQueryRange_RangeMode_SumOverTimeMatrix_ChDB pins
// `sum_over_time(metric[5m])` over `/api/v1/query_range`. Same matrix
// fan-out story as rate, but for the *_over_time family. Each anchor
// reduces the prior 5-minute window via arraySum.
//
// Seeded: 11 samples of constant value 10 at 30s spacing. Each
// anchor's window holds an increasing subset of those samples; the
// per-step sum grows from 10 (1 sample in window) to 110 (11 samples
// in window).
func TestQueryRange_RangeMode_SumOverTimeMatrix_ChDB(t *testing.T) {
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows,
			fmt.Sprintf(`('demo_memory_usage_bytes', map('job', 'api'), toDateTime64('%s', 9), 10.0)`,
				ts))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"sum_over_time(demo_memory_usage_bytes[5m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series for sum_over_time matrix; got 0")
	}
	// Per-step fan-out: expect at least 5 distinct anchors in the
	// 5-minute query window (step=30s, 11 candidate anchors).
	if got := len(matrix[0].Values); got < 5 {
		t.Fatalf("expected per-step sum_over_time matrix (>= 5 samples); got %d: %+v",
			got, matrix[0].Values)
	}
	// Per-step variation: as anchors advance through the seed, the
	// 5-minute window picks up more samples and the sum grows. A
	// regression where every step carried the same value would surface
	// as first == last.
	first := matrix[0].Values[0][1]
	last := matrix[0].Values[len(matrix[0].Values)-1][1]
	if first == last {
		t.Errorf("expected per-step variation across the sum_over_time matrix; got first=last=%q (full row: %+v)",
			first, matrix[0].Values)
	}
}

// TestQueryRange_RangeMode_VVOnComparison_ChDB pins the 502 the
// compat lane caught for V-V `==/!=/<=` etc. with `on(...)` matching
// under query_range. The pre-Pool-AL emitter aggregated each side by
// the match key WITHOUT bucketing on anchor_ts, so the per-anchor
// matrix collapsed to a single row per match-key. With multiple
// distinct series sharing the on-key (e.g. instance + job + type),
// the runtime uniqueness throwIf fired and CH surfaced it as a 502.
//
// Seeded: two series with the same {instance, job, type} on-key but
// disjoint extra labels (the on() collapse). Without StepAligned the
// throwIf fires (the match key sees 2 distinct Attributes maps per
// anchor); with StepAligned the per-(match-key, anchor) bucket sees
// exactly one Attributes map AT EACH anchor, and the comparison
// surfaces all-true (the metric equals itself element-wise).
//
// Use the same metric on both sides — `metric == on(k...) metric` is
// the canonical Prom self-comparison. With identical values per
// (series, anchor) the result is 1.0 per matched pair when ReturnBool
// is set, or the LHS value where the comparison holds when not.
func TestQueryRange_RangeMode_VVOnComparison_ChDB(t *testing.T) {
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	// Two series share (instance, job, type) but differ in another
	// label — the on() collapse would normally fold them onto the
	// same match-key row and trip the throwIf without step alignment.
	// We pick a single Attributes map for each side because the
	// comparison `metric == on(k) metric` evaluates per-anchor: the
	// surviving rows are those whose (instance, job, type) tuple
	// matches across the two legs. With one tuple per series and
	// identical values, the comparison surfaces 1.0 per surviving
	// pair (the bool-mode variant of `==`).
	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_memory_usage_bytes', map('instance', 'demo', 'job', 'app', 'type', 'rss'), toDateTime64('%s', 9), %d.0)`,
			ts, 100+i,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	// Use `== bool` so each matched pair emits 1.0 — easier to assert
	// than relying on the bare-`==` filter-mode preserving the LHS
	// value, and matches what the compat-lane queries actually
	// requested (the 12 case-class includes bool variants).
	matrix := runRangeModeQueryRange(t, srv.URL,
		"demo_memory_usage_bytes == bool on(instance, job, type) demo_memory_usage_bytes",
		start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("pre-Pool-AL: V-V `on(...)` over query_range surfaces 502 / 0 series; got 0 series")
	}
	// One series (one match-key tuple) with N per-step samples.
	values := matrix[0].Values
	if got := len(values); got < 5 {
		t.Fatalf("expected per-step V-V comparison matrix (>= 5 samples); got %d: %+v",
			got, values)
	}
	// Each surviving pair compares the metric to itself → bool-1 per
	// step. No "1.0" because bool comparisons in Prom emit the integer
	// 1 (rendered by the cerberus pipeline as "1" — same shape as the
	// existing `bool_vv_eq.txtar` fixture's expected_rows).
	for i, v := range values {
		if got := v[1]; got != "1" {
			t.Errorf("step %d: V-V `== bool` value=%q want=%q (full row: %+v)",
				i, got, "1", v)
		}
	}
}

// runRangeModeQueryRange is the test helper shared across the Pool-AK
// range-mode regression cases. Mirrors `runStepLoopRange` but lives
// in this file so the Pool-AK suite is self-contained for any future
// reviewer auditing the per-step matrix contract end-to-end.
func runRangeModeQueryRange(t *testing.T, baseURL, query string, start, end time.Time, step time.Duration) []prom.MatrixSample {
	t.Helper()
	reqURL := fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		baseURL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
	)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET %s: %v", reqURL, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s (pre-Pool-AK: empty matrix or near-empty matrix)",
			resp.StatusCode, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q (errorType=%q error=%q), want success",
			parsed.Status, parsed.ErrorType, parsed.Error)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType: got %q, want matrix", parsed.Data.ResultType)
	}
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var matrix []prom.MatrixSample
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("decode matrix: %v (raw=%s)", err, rawResult)
	}
	return matrix
}
