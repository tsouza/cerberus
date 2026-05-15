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

// TestQueryRange_RangeMode_Deriv_ChDB pins `deriv(metric[5m])` over
// query_range. Before the fix, the deriv emitter routed through
// emitWindowedArrayPairs whose OuterRange > 0 branch unconditionally
// errored with "predict_linear over subquery not yet supported" — Pool-
// AK's range-mode rework set OuterRange/Step on every range-vector
// call, so deriv 502'd on every compatibility run.
//
// Seed: linear ramp with slope 1 unit / 30s = 0.033... /s. Every per-
// step anchor's 5-minute lookback window therefore captures a least-
// squares-perfect fit with slope ≈ 0.0333.
func TestQueryRange_RangeMode_Deriv_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_disk_usage_bytes', map('instance', 'demo'), toDateTime64('%s', 9), %d.0)`,
			ts, 100+i,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"deriv(demo_disk_usage_bytes[5m])", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %+v", len(matrix), matrix)
	}
	values := matrix[0].Values
	if len(values) < 2 {
		t.Fatalf("expected at least 2 deriv samples (one per step with >=2 in window); got %d: %+v",
			len(values), values)
	}
	// Per-step slope must hover around 1/30s = 0.0333... — values
	// across the matrix should all be that constant for a perfect
	// linear ramp. Format-equality keeps this hermetic; the simpleLinear
	// Regression aggregate returns the bit-stable Float64 "1/30".
	const wantValue = "0.03333333333333333"
	for i, v := range values {
		if got := v[1]; got != wantValue {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, wantValue, v)
		}
	}
}

// TestQueryRange_RangeMode_IRate_ChDB pins `irate(metric[5m])` over
// query_range. Same root cause as the deriv case — the irate emitter
// routes through emitWindowedArrayPairs which used to hard-error in
// matrix mode. The fix routes irate through the new
// emitWindowedArrayPairsMatrix variant; the anchor isn't used by the
// irate value expression (the rate is computed from the last two
// samples' own timestamps, not from the eval anchor) so behaviour
// across anchors is constant for a perfectly-uniform counter ramp.
//
// Seed: counter increases by 1 every 30s → irate = 1/30 = 0.0333... /s.
func TestQueryRange_RangeMode_IRate_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	// irate is a counter rate; seed an otel_metrics_sum-shaped table.
	sumDDL := strings.ReplaceAll(gaugeDDL, "otel_metrics_gauge", "otel_metrics_sum")
	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_cpu_usage_seconds_total', map('instance', 'demo'), toDateTime64('%s', 9), %d.0)`,
			ts, 100+i,
		))
	}
	seed := sumDDL + "\nINSERT INTO otel_metrics_sum VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"irate(demo_cpu_usage_seconds_total[5m])", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %+v", len(matrix), matrix)
	}
	values := matrix[0].Values
	if len(values) < 2 {
		t.Fatalf("expected at least 2 irate samples (one per step with >=2 in window); got %d: %+v",
			len(values), values)
	}
	const wantValue = "0.03333333333333333"
	for i, v := range values {
		if got := v[1]; got != wantValue {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, wantValue, v)
		}
	}
}

// TestQueryRange_RangeMode_LabelReplace_NonMatchingRegex_ChDB pins
// `label_replace(v, dst, "value-$1", src, "<no-capture-groups>")` over
// query_range. Before the fix, the replacement `value-\1` was passed
// verbatim to CH's replaceRegexpOne; CH validates the substitution
// against the regex's capture-group count at SQL-parse time and rejects
// `\N` references that exceed it (Code 36) even when the surrounding
// if(match(...)) short-circuits the replaceRegexpOne call.
//
// PromQL semantics: when the regex doesn't match `src`, dst is left
// unchanged on the input map. The fix counts the regex's capture
// groups at lowering time and drops out-of-range backrefs from the CH
// replacement; CH then accepts the SQL, match() never fires, and the
// input map flows through untouched — restoring the wire shape Prom
// returns.
func TestQueryRange_RangeMode_LabelReplace_NonMatchingRegex_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('demo_num_cpus', map('instance', 'demo.promlabs.com:10000', 'job', 'demo'), toDateTime64('%s', 9), 4.0)`,
			ts,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	query := `label_replace(demo_num_cpus, "job", "value-$1", "instance", "non-matching-regex")`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series (input unchanged), got %d: %+v", len(matrix), matrix)
	}
	// Regex doesn't match → `job` stays "demo", not "value-...".
	if got := matrix[0].Metric["job"]; got != "demo" {
		t.Errorf("job: got %q, want %q (full metric: %+v)", got, "demo", matrix[0].Metric)
	}
	if got := matrix[0].Metric["instance"]; got != "demo.promlabs.com:10000" {
		t.Errorf("instance: got %q, want %q (full metric: %+v)",
			got, "demo.promlabs.com:10000", matrix[0].Metric)
	}
	// Every per-step value must be 4.0 — the seed is constant.
	for i, v := range matrix[0].Values {
		if got := v[1]; got != "4" {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, "4", v)
		}
	}
}

// TestQueryRange_RangeMode_QuantileOverTime_ChDB pins
// `quantile_over_time(phi, metric[range])` under query_range. The
// `quantile_over_time` lowering routes through `lowerQuantileOverTime`
// (parameterised first arg) rather than the generic
// `lowerRangeVectorCall`; the matrix-fan-out gate landed via Pool-AM
// (#348). This test pins the per-anchor matrix fan-out so any future
// regression in the gate surfaces immediately rather than at compat
// time. Pre-Pool-AM cerberus emitted `now64(9)` for every anchor and
// the matrix pivot collapsed to a single row per series — surfaced
// as 42 compatibility-lane diffs across the {phi, range} variant
// grid (Pool-AI's `quantile_over_time(*, demo_memory_usage_bytes[*])`
// family).
//
// Seeded: a single demo_memory_usage_bytes sample per step (every
// 30s). The 5-minute window holds an increasing number of samples as
// we advance through the step grid, so a `quantile(0.5)` over each
// window produces a strictly monotonic series. A regression where
// the matrix path stayed dormant would surface as either a single
// repeated value (one anchor at end_ts) or all-zero rows.
func TestQueryRange_RangeMode_QuantileOverTime_ChDB(t *testing.T) {
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
		"quantile_over_time(0.5, demo_memory_usage_bytes[5m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series; got 0")
	}
	values := matrix[0].Values
	// Pre-Pool-AM the matrix path stayed dormant — assert per-step
	// fan-out (>= 5 distinct anchor rows). Single-row regressions
	// (Step=0, OuterRange=0 collapse) surface as `len(values) == 1`.
	if got := len(values); got < 5 {
		t.Fatalf("expected per-step quantile_over_time matrix (>= 5 samples); got %d: %+v",
			got, values)
	}
	// As each anchor's lookback adds one more high-end sample, the
	// median moves monotonically upward. All-equal values would
	// indicate the matrix path is still emitting `now64(9)` for
	// every anchor and folding to one sample.
	first := values[0][1]
	last := values[len(values)-1][1]
	if first == last {
		t.Errorf("expected per-step variation across the matrix; got first=last=%q (full row: %+v)",
			first, values)
	}
}

// TestQueryRange_RangeMode_QuantileOverTimeOutOfRange_ChDB pins the
// out-of-range phi compat-lane shapes (phi=-0.5 → -Inf, phi=1.5 →
// +Inf) under query_range. Pool-D / Pool-V (PR #322 + #328) folded
// the out-of-range phi to a value-rewrite Project on top of the
// RangeWindow; Pool-AM (#348) threads the request's step / OuterRange
// onto that RangeWindow so the value-rewrite + anchor_ts forwarding
// both land on every per-step row.
//
// The post-Project replaces Value with ±Inf; the per-step row count
// should match the valid-phi control case (data structure is
// identical, only the Value scalar changes). chDB renders ±Inf as
// `-Inf` / `+Inf` via the `(±1.0/0)` inline forms.
func TestQueryRange_RangeMode_QuantileOverTimeOutOfRange_ChDB(t *testing.T) {
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
		"quantile_over_time(-0.5, demo_memory_usage_bytes[5m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series for phi=-0.5; got 0")
	}
	values := matrix[0].Values
	if got := len(values); got < 5 {
		t.Fatalf("expected per-step matrix for out-of-range phi (>= 5 samples); got %d: %+v",
			got, values)
	}
	// Every row's Value should render as -Inf — chDB matches Prom's
	// JSON wire format and emits "-Inf" for math.Inf(-1).
	for i, v := range values {
		if got := v[1]; got != "-Inf" {
			t.Errorf("step %d: value=%q want=-Inf (full row: %+v)", i, got, v)
		}
	}
}

// TestQueryRange_RangeMode_Clamp_ChDB pins
// `clamp(metric, min, max)` under query_range. The bare-selector
// inner is the per-step LWR Project (canonical 4-column shape) and
// `projectValueOverInner` wraps it with another canonical Project
// that re-writes Value with greatest(min, least(max, Value)). The
// per-step TimeUnix passes through both Project layers via bare
// ColumnRef references, so each anchor emits its own row.
//
// Seeded: 11 samples spaced by step, values 100..110. Min=105,
// max=108 should clamp each step's value into [105, 108].
func TestQueryRange_RangeMode_Clamp_ChDB(t *testing.T) {
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
		"clamp(demo_memory_usage_bytes, 105, 108)", start, end, step)
	if len(matrix) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %+v", len(matrix), matrix)
	}
	values := matrix[0].Values
	// Pin "per-step fan-out fires" rather than an exact count — the
	// helper truncates start/end to second resolution via `.Unix()`,
	// so seed rows anchored on `time.Now()` (nanosecond resolution)
	// can land just outside the request grid's earliest step.
	// Pre-Pool-AK cerberus collapsed clamp to a single anchor when the
	// inner per-step LWR was bypassed — surfaced as `len(values) == 1`.
	if got := len(values); got < 5 {
		t.Fatalf("expected per-step clamp matrix (>= 5 samples); got %d: %+v",
			got, values)
	}
	// Values 100..110 clamped to [105, 108]:
	//   100..104 → 105, 105..108 → unchanged, 109..110 → 108.
	// Assert each clamped row falls inside the [105, 108] band and
	// the series carries both bound-hits (min-clamped + max-clamped)
	// in distinct rows — proves the per-step rewrite fires.
	seenMin, seenMax := false, false
	for i, v := range values {
		raw := v[1]
		switch raw {
		case "105":
			seenMin = true
		case "108":
			seenMax = true
		case "106", "107":
			// inside band, fine
		default:
			t.Errorf("step %d: value=%q outside [105, 108] band (full row: %+v)", i, raw, v)
		}
	}
	if !seenMin || !seenMax {
		t.Errorf("expected the clamped matrix to hit both bounds; seenMin=%v seenMax=%v (full row: %+v)",
			seenMin, seenMax, values)
	}
}

// TestQueryRange_RangeMode_ClampInverted_ChDB pins Prom's "empty when
// maxVal < minVal" semantic on `clamp(metric, large, small)`. Prom's
// funcClamp short-circuits to an empty Vector when `maxVal < minVal`
// (see prometheus/promql/functions.go::clamp). Pre-Pool-AM, cerberus
// emitted `greatest(min, least(max, Value))` which would force every
// sample to `min` (a constant). Pool-AM detects degenerate bounds at
// lowering and wraps the inner tree with a `Filter(LitBool{false})`
// so no rows survive to the matrix pivot. Surfaced as the compat-
// lane diff on
// `clamp(demo_memory_usage_bytes, 1000000000000, 0)`.
func TestQueryRange_RangeMode_ClampInverted_ChDB(t *testing.T) {
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
		"clamp(demo_memory_usage_bytes, 1000000000000, 0)", start, end, step)
	if len(matrix) != 0 {
		t.Fatalf("expected empty matrix for max<min clamp; got %d series: %+v",
			len(matrix), matrix)
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

// histogramDDL is the OTel-CH classic-histogram-shaped table the chDB-
// backed range-mode tests below seed before exercising
// `histogram_quantile(phi, ...)` under `/api/v1/query_range`. Mirrors
// `gaugeDDL`'s minimal-but-MergeTree shape so the chsql emitter's
// PREWHERE promotion path runs end-to-end against ClickHouse semantics
// (Memory engine rejects PREWHERE).
const histogramDDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// TestQueryRange_RangeMode_HistogramQuantileClassic_ChDB pins
// `histogram_quantile(phi, sum by(le)(rate(<bucket>[r])))` over
// `/api/v1/query_range` against the OTel-CH classic-histogram table.
// Pre-fix, the lowering emitted `now64(9)` for every anchor's TimeUnix
// so the matrix pivot collapsed N anchors onto a single "now" point —
// Pool-AK flagged this as the `histogram_quantile classic-bucket still
// hardcodes now64(9) in range mode` follow-up to the per-step LWR
// rework (#347).
//
// The fix routes range-mode histogram_quantile through a per-step plan:
// CrossJoin(StepGrid, Filter(Scan)) + per-anchor LWR window + Aggregate
// by (anchor_ts, user_labels) + HistogramQuantile with anchor_ts in the
// GroupBy. The outer Project re-aliases anchor_ts → TimeUnix so the
// matrix pivot lands one quantile sample per step.
//
// Seed: classic-histogram rows whose BucketCounts ([le=0.1, le=0.5,
// le=+Inf]) grow over time. With buckets [0.1, 0.5, +Inf] and counts
// [a, b, c] the cumulative is [a, a+b, a+b+c]; for phi=0.95 the target
// `0.95 * (a+b+c)` lands inside the second or third bucket depending
// on the per-anchor sum. The lookback is 5m so each step anchor sums
// every bucket-row before it (rates over the window). The resulting
// values move monotonically across the step grid — a regression where
// the now64(9) collapse silently returned would surface as one
// repeated quantile value (single anchor at end_ts) or zero samples.
func TestQueryRange_RangeMode_HistogramQuantileClassic_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const seedSamples = 11

	// Seed: one histogram row per (anchor, series) carrying a
	// monotonically growing BucketCounts vector. The bucket bounds
	// [0.1, 0.5] (plus the implicit +Inf trailing bucket) stay
	// constant; the per-bucket counts go up by step. Per-anchor
	// rate-window sums therefore grow with the step index, and the
	// p95 interpolation lands inside one of the explicit-bounds
	// buckets (not at the highest finite edge) so any sub-bucket
	// regression surfaces in the value comparison.
	seedRows := make([]string, 0, seedSamples)
	for i := 0; i < seedSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		// BucketCounts at index i: [10+i, 20+i, 30+i] — the trailing
		// +Inf bucket dominates so p95 reads into the (0.5, +Inf]
		// bucket and the emitter returns the highest explicit bound
		// (0.5) per the spec's "phi crosses +Inf → return last
		// explicit bound" branch. The shape is stable across steps,
		// but the step-anchor TimeUnix MUST change with each row.
		seedRows = append(seedRows, fmt.Sprintf(
			`('http_server_request_duration', map('service', 'api'), toDateTime64('%s', 9), [%d, %d, %d], [0.1, 0.5])`,
			ts, 10+i, 20+i, 30+i,
		))
	}
	seed := histogramDDL + "\nINSERT INTO otel_metrics_histogram VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	matrix := runRangeModeQueryRange(t, srv.URL,
		"histogram_quantile(0.95, sum by(le)(rate(http_server_request_duration[5m])))",
		start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected at least 1 series; got 0")
	}
	values := matrix[0].Values
	// Pre-fix the lowering emitted now64(9) for every per-step anchor
	// so the matrix pivot collapsed onto one row — assert per-step
	// fan-out (>= 2 distinct sample rows; the task description's lower
	// bound).
	if got := len(values); got < 2 {
		t.Fatalf("expected per-step histogram_quantile matrix (>= 2 samples); got %d: %+v",
			got, values)
	}
	// Per-step Timestamp must increase strictly — a regression where
	// every anchor reused now64(9) would surface as identical
	// timestamps repeated across the matrix.
	for i := 1; i < len(values); i++ {
		prev, _ := values[i-1][0].(float64)
		curr, _ := values[i][0].(float64)
		if curr <= prev {
			t.Errorf("step %d: timestamp not strictly increasing: prev=%v curr=%v (full row: %+v)",
				i, prev, curr, values)
		}
	}
	// Every per-step quantile value must be the highest explicit
	// bound (0.5) — the seed's bucket distribution forces phi=0.95 to
	// land inside the (+Inf) overflow bucket, and the emitter's
	// `idx == length(cum)` branch returns ExplicitBounds[length(eb)]
	// = 0.5 per the upstream Prom spec. A regression that emitted a
	// different value (e.g. interpolating across the +Inf bucket) or
	// repeated a stale value across all steps would diverge here.
	for i, v := range values {
		if got := v[1]; got != "0.5" {
			t.Errorf("step %d: value=%q want=%q (full row: %+v)", i, got, "0.5", v)
		}
	}
}

// TestQueryRange_RangeMode_VVOnCompare_ChDB pins the bare V-V
// `on(...)` comparison binop family (==, !=, <, >, <=, >=) under
// `/api/v1/query_range` — the no-`bool`-modifier sibling of
// TestQueryRange_RangeMode_VVOnComparison_ChDB. Before this fix every
// shape in the family returned `server_error: 502` because the
// comparison emit path projected the per-pair comparison result
// `(L.Value <op> R.Value)` — a CH UInt8 — into the canonical Value
// column, and clickhouse-go's scan into `*float64` rejected the type
// with the 502 surfacing as 12 compat-lane failures on the
// `demo_memory_usage_bytes` selector (six bare ops × two LHS shapes —
// bare selector and `sum by()` `group_left(job)` join).
//
// The fix routes plain V-V comparisons through the existing step-
// aligned VectorJoin (PR #348's StepAligned conjunct) and projects
// `L.Value` into the output column with a `WHERE (L.Value <op>
// R.Value)` predicate so Prom's "preserve LHS where comparison
// holds" semantics fire and the output Value stays Float64. This
// test seeds two series with comparable values, runs every op, and
// asserts each step's matrix-output value is one of the LHS sample
// values that satisfied the per-anchor comparison.
//
// The chDB lane is the right home: this is a wire-format regression
// (clickhouse-go scan refusing UInt8 → *float64), not a SQL-shape
// regression, so the stub lane wouldn't catch a future regression
// that breaks only the scan path.
func TestQueryRange_RangeMode_VVOnCompare_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	// Two series with constant per-step values (100, 200). Comparing
	// each series with itself across the on(instance, job, type)
	// match yields rows where L.Value == R.Value (always true), so
	// `==` keeps every pair, `<`/`>` drop everything, and `<=`/`>=`
	// behave like `==`. Mirrors the compatibility lane's PromLabs
	// demo seed (instance/job/type labels).
	seedRows := make([]string, 0, wantSamples*2)
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows,
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'a', 'job', 'demo', 'type', 'used'), toDateTime64('%s', 9), 100.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'b', 'job', 'demo', 'type', 'used'), toDateTime64('%s', 9), 200.0)`, ts),
		)
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	// Each entry pins the per-pair filter behaviour of one of the
	// 12 compat-lane shapes (the 6 bare comparison ops, all over the
	// same on(instance, job, type) shape). When the comparison
	// passes, the LHS Value flows through; when it fails, no row is
	// emitted at that step.
	type vvCmpCase struct {
		// name is the t.Run subtest name and doubles as the trace tag.
		name string
		// query is the PromQL expression executed against the seeded
		// matrix. The bare comparison without `bool` triggers
		// Prom's V-V comparison filter rule.
		query string
		// wantSeriesCount is the number of distinct series the
		// matrix should carry after the filter.
		wantSeriesCount int
		// wantValuePerSeries is the per-instance Value sample we
		// expect at each step in the result series. Each series
		// must carry exactly `wantSamples` rows of the matching
		// value when the comparison passes (== / <= / >=).
		wantValuePerSeries map[string]string
	}
	for _, tc := range []vvCmpCase{
		{
			name:               "eq",
			query:              `demo_memory_usage_bytes == on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    2,
			wantValuePerSeries: map[string]string{"a": "100", "b": "200"},
		},
		{
			name:               "le",
			query:              `demo_memory_usage_bytes <= on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    2,
			wantValuePerSeries: map[string]string{"a": "100", "b": "200"},
		},
		{
			name:               "ge",
			query:              `demo_memory_usage_bytes >= on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    2,
			wantValuePerSeries: map[string]string{"a": "100", "b": "200"},
		},
		{
			name:               "ne",
			query:              `demo_memory_usage_bytes != on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    0,
			wantValuePerSeries: nil,
		},
		{
			name:               "lt",
			query:              `demo_memory_usage_bytes < on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    0,
			wantValuePerSeries: nil,
		},
		{
			name:               "gt",
			query:              `demo_memory_usage_bytes > on(instance, job, type) demo_memory_usage_bytes`,
			wantSeriesCount:    0,
			wantValuePerSeries: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matrix := runRangeModeQueryRange(t, srv.URL,
				tc.query, start, end, step)
			if len(matrix) != tc.wantSeriesCount {
				t.Fatalf("query=%q: expected %d series, got %d: %+v",
					tc.query, tc.wantSeriesCount, len(matrix), matrix)
			}
			if tc.wantSeriesCount == 0 {
				// Filtered-out shape — nothing more to check.
				return
			}
			for _, ms := range matrix {
				inst := ms.Metric["instance"]
				want, ok := tc.wantValuePerSeries[inst]
				if !ok {
					t.Errorf("unexpected series instance=%q: %+v", inst, ms.Metric)
					continue
				}
				if len(ms.Values) != wantSamples {
					t.Errorf("instance=%s: expected %d samples, got %d: %+v",
						inst, wantSamples, len(ms.Values), ms.Values)
					continue
				}
				for i, v := range ms.Values {
					if got := v[1]; got != want {
						t.Errorf("instance=%s step %d: value=%q want=%q (full row: %+v)",
							inst, i, got, want, v)
					}
				}
			}
		})
	}
}

// TestQueryRange_RangeMode_VVOnCompareGroupLeft_ChDB pins the
// `sum by(instance, type) (metric) <cmp> on(instance, type)
// group_left(job) metric` family (the second LHS shape in the
// compat-lane's 12-failure batch). The `group_left(job)` modifier
// copies the `job` label onto the LHS-derived output rows.
//
// Pre-fix: the emit path projected `(L.Value <op> R.Value)` as the
// Value column → UInt8 → clickhouse-go scan failure → 502.
//
// Post-fix: the bare comparison routes through `WHERE (L.Value <op>
// R.Value)` with `L.Value AS Value`; the matrix output carries the
// LHS aggregate value where the comparison holds.
//
// Seeded: two `(instance, type)` groups (a/b × used) with a single
// job per group. The LHS aggregate `sum by(instance, type)` folds
// the per-group rows down to one value; the RHS bare selector
// preserves the job label. `group_left(job)` then copies the job
// label onto the LHS output without trip the uniqueness throwIf
// (one row per (instance, type, job) tuple on each side).
func TestQueryRange_RangeMode_VVOnCompareGroupLeft_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	// Two (instance, type) groups, each with a single demo job. The
	// LHS `sum by(instance, type)` produces one row per group (same
	// value as the single underlying row); the RHS bare selector
	// produces one row per group with `job=demo`. `group_left(job)`
	// copies `job` from RHS onto the LHS output.
	seedRows := make([]string, 0, wantSamples*2)
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows,
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'a', 'job', 'demo', 'type', 'used'), toDateTime64('%s', 9), 100.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 'b', 'job', 'demo', 'type', 'used'), toDateTime64('%s', 9), 200.0)`, ts),
		)
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	// `sum by(instance, type) (metric)` folds the per-group rows
	// down to one value per (instance, type); comparing that against
	// the bare selector on the same data with `on(instance, type)`
	// always holds for `==`.
	query := `sum by(instance, type) (demo_memory_usage_bytes) == on(instance, type) group_left(job) demo_memory_usage_bytes`
	matrix := runRangeModeQueryRange(t, srv.URL, query, start, end, step)
	if len(matrix) != 2 {
		t.Fatalf("expected 2 series (one per (instance, type) group), got %d: %+v",
			len(matrix), matrix)
	}
	wantPerInstance := map[string]string{"a": "100", "b": "200"}
	for _, ms := range matrix {
		inst := ms.Metric["instance"]
		want, ok := wantPerInstance[inst]
		if !ok {
			t.Errorf("unexpected series instance=%q: %+v", inst, ms.Metric)
			continue
		}
		if got := ms.Metric["job"]; got != "demo" {
			t.Errorf("instance=%s: expected job=demo (group_left copy), got job=%q (full metric: %+v)",
				inst, got, ms.Metric)
		}
		if len(ms.Values) != wantSamples {
			t.Errorf("instance=%s: expected %d samples, got %d: %+v",
				inst, wantSamples, len(ms.Values), ms.Values)
			continue
		}
		for i, v := range ms.Values {
			if got := v[1]; got != want {
				t.Errorf("instance=%s step %d: value=%q want=%q (full row: %+v)",
					inst, i, got, want, v)
			}
		}
	}
}

// TestQueryRange_RangeMode_TopK_ChDB pins `topk(K, v)` / `bottomk(K, v)`
// per-step semantics under `/api/v1/query_range`. Bucket 5 of the
// post-#350 compat residual audit (docs/compat-residual-audit-
// 25898791664.md) flagged 8 diffs traceable to the partition list on
// chplan.TopK omitting the per-step anchor. Pre-fix, the emitter
// rendered `LIMIT K BY <user-partition>` which collapses N anchors ×
// M series into a single K-row global window; Prom's semantics select
// K series per anchor.
//
// The fix threads `TimeUnix` (the per-step anchor re-aliased by
// wrapRangeLatestPerSeries) into TopK.By for range mode, so the
// emitter renders `LIMIT K BY (..., TimeUnix)` and the per-step matrix
// surfaces the correct K-per-anchor subset.
//
// Seed: 6 distinct series ranked by ascending Value (10, 20, 30, 40,
// 50, 60). Each series has one sample per step. The bare topk(3, v)
// should keep the top 3 (Value 60, 50, 40) at EVERY anchor — 3 series
// × N anchors = 3*N matrix rows. The bottomk(3, v) should keep (10,
// 20, 30) at every anchor. `topk by(group)` partitions by an extra
// label so K-per-group-per-step holds; the `without` variants exercise
// the MapWithoutKeys partition shape.
func TestQueryRange_RangeMode_TopK_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	// Seed: 6 distinct series. Series 1-3 are in group=a (values 10,
	// 20, 30); series 4-6 are in group=b (values 40, 50, 60). One
	// sample per series per step (constant value across the time
	// range so the per-step rank is deterministic).
	seedRows := make([]string, 0, wantSamples*6)
	for i := 0; i < wantSamples; i++ {
		ts := start.Add(time.Duration(i) * step).Format("2006-01-02 15:04:05.000000000")
		seedRows = append(seedRows,
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's1', 'group', 'a'), toDateTime64('%s', 9), 10.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's2', 'group', 'a'), toDateTime64('%s', 9), 20.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's3', 'group', 'a'), toDateTime64('%s', 9), 30.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's4', 'group', 'b'), toDateTime64('%s', 9), 40.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's5', 'group', 'b'), toDateTime64('%s', 9), 50.0)`, ts),
			fmt.Sprintf(`('demo_memory_usage_bytes', map('instance', 's6', 'group', 'b'), toDateTime64('%s', 9), 60.0)`, ts),
		)
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"
	srv, _ := newChDBServer(t, seed)

	// Pre-fix: cerberus emitted `LIMIT 3` (or LIMIT K BY <user> alone)
	// → the whole-query window collapsed to 3 rows TOTAL, leaving
	// fewer than `wantSamples` distinct anchors in the matrix pivot.
	// Post-fix: 3 series × N anchors = correct per-step shape.
	t.Run("topk_bare", func(t *testing.T) {
		matrix := runRangeModeQueryRange(t, srv.URL,
			"topk(3, demo_memory_usage_bytes)", start, end, step)
		// 3 series × N anchors → 3 series in the matrix (each with N
		// samples). The top-3 by Value are s4 (40), s5 (50), s6 (60).
		if len(matrix) != 3 {
			t.Fatalf("topk bare: expected 3 series, got %d: %+v",
				len(matrix), matrix)
		}
		wantInstances := map[string]string{"s4": "40", "s5": "50", "s6": "60"}
		for _, ms := range matrix {
			inst := ms.Metric["instance"]
			want, ok := wantInstances[inst]
			if !ok {
				t.Errorf("topk bare: unexpected series instance=%q: %+v", inst, ms.Metric)
				continue
			}
			if len(ms.Values) != wantSamples {
				t.Errorf("topk bare: instance=%s: expected %d samples per step, got %d (pre-fix would collapse to a global subset): %+v",
					inst, wantSamples, len(ms.Values), ms.Values)
				continue
			}
			for i, v := range ms.Values {
				if got := v[1]; got != want {
					t.Errorf("topk bare: instance=%s step %d: value=%q want=%q (full row: %+v)",
						inst, i, got, want, v)
				}
			}
		}
	})

	t.Run("bottomk_bare", func(t *testing.T) {
		matrix := runRangeModeQueryRange(t, srv.URL,
			"bottomk(3, demo_memory_usage_bytes)", start, end, step)
		// Bottom 3 by Value: s1 (10), s2 (20), s3 (30).
		if len(matrix) != 3 {
			t.Fatalf("bottomk bare: expected 3 series, got %d: %+v",
				len(matrix), matrix)
		}
		wantInstances := map[string]string{"s1": "10", "s2": "20", "s3": "30"}
		for _, ms := range matrix {
			inst := ms.Metric["instance"]
			want, ok := wantInstances[inst]
			if !ok {
				t.Errorf("bottomk bare: unexpected series instance=%q: %+v", inst, ms.Metric)
				continue
			}
			if len(ms.Values) != wantSamples {
				t.Errorf("bottomk bare: instance=%s: expected %d samples per step, got %d: %+v",
					inst, wantSamples, len(ms.Values), ms.Values)
				continue
			}
			for i, v := range ms.Values {
				if got := v[1]; got != want {
					t.Errorf("bottomk bare: instance=%s step %d: value=%q want=%q (full row: %+v)",
						inst, i, got, want, v)
				}
			}
		}
	})

	t.Run("topk_by_group", func(t *testing.T) {
		// `topk by(group) (2, ...)` keeps the top-2 per `group`. group=a
		// has values (10, 20, 30) → top-2 = (20, 30); group=b has
		// values (40, 50, 60) → top-2 = (50, 60). At every anchor we
		// expect exactly 4 series in the matrix.
		matrix := runRangeModeQueryRange(t, srv.URL,
			"topk by(group) (2, demo_memory_usage_bytes)", start, end, step)
		if len(matrix) != 4 {
			t.Fatalf("topk by(group): expected 4 series (top-2 per of 2 groups), got %d: %+v",
				len(matrix), matrix)
		}
		wantInstances := map[string]string{"s2": "20", "s3": "30", "s5": "50", "s6": "60"}
		for _, ms := range matrix {
			inst := ms.Metric["instance"]
			want, ok := wantInstances[inst]
			if !ok {
				t.Errorf("topk by(group): unexpected series instance=%q: %+v", inst, ms.Metric)
				continue
			}
			if len(ms.Values) != wantSamples {
				t.Errorf("topk by(group): instance=%s: expected %d samples per step, got %d: %+v",
					inst, wantSamples, len(ms.Values), ms.Values)
				continue
			}
			for i, v := range ms.Values {
				if got := v[1]; got != want {
					t.Errorf("topk by(group): instance=%s step %d: value=%q want=%q (full row: %+v)",
						inst, i, got, want, v)
				}
			}
		}
	})

	t.Run("bottomk_without_empty", func(t *testing.T) {
		// `bottomk(K, v) without()` partitions by the full Attributes
		// map (per-series). Each (series, step) tuple sits in its own
		// partition, so `LIMIT K BY (Attributes, TimeUnix)` keeps 1
		// row per partition — every (series, step) survives. The matrix
		// therefore has 6 series × N anchors.
		matrix := runRangeModeQueryRange(t, srv.URL,
			"bottomk without() (5, demo_memory_usage_bytes)", start, end, step)
		if len(matrix) != 6 {
			t.Fatalf("bottomk without(): expected 6 series (per-series partitions), got %d: %+v",
				len(matrix), matrix)
		}
		for _, ms := range matrix {
			if len(ms.Values) != wantSamples {
				t.Errorf("bottomk without(): instance=%s: expected %d per-step samples, got %d: %+v",
					ms.Metric["instance"], wantSamples, len(ms.Values), ms.Values)
			}
		}
	})

	t.Run("topk_without_instance", func(t *testing.T) {
		// `topk without(instance) (2, ...)` strips `instance` from the
		// partition shape — partition keys collapse to {group}.
		// Equivalent to grouping by everything except `instance`, which
		// here yields 2 groups (a, b). Top-2 per group → 4 series in
		// the matrix.
		matrix := runRangeModeQueryRange(t, srv.URL,
			"topk without(instance) (2, demo_memory_usage_bytes)", start, end, step)
		if len(matrix) != 4 {
			t.Fatalf("topk without(instance): expected 4 series, got %d: %+v",
				len(matrix), matrix)
		}
		wantInstances := map[string]string{"s2": "20", "s3": "30", "s5": "50", "s6": "60"}
		for _, ms := range matrix {
			inst := ms.Metric["instance"]
			want, ok := wantInstances[inst]
			if !ok {
				t.Errorf("topk without(instance): unexpected series instance=%q: %+v",
					inst, ms.Metric)
				continue
			}
			if len(ms.Values) != wantSamples {
				t.Errorf("topk without(instance): instance=%s: expected %d samples per step, got %d: %+v",
					inst, wantSamples, len(ms.Values), ms.Values)
				continue
			}
			for i, v := range ms.Values {
				if got := v[1]; got != want {
					t.Errorf("topk without(instance): instance=%s step %d: value=%q want=%q (full row: %+v)",
						inst, i, got, want, v)
				}
			}
		}
	})
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
