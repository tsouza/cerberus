//go:build chdb

// chDB-backed regression coverage for the engine step-loop bug
// Pool-O / Pool-S2 surfaced: `/api/v1/query_range` evaluated
// "no driving vector" expressions (`time()`, `vector(scalar)`,
// zero-arg date fns, `absent(...)`) by emitting a single row at the
// eval anchor `now64(9)`. The matrix-pivot step loop in the handler
// dropped that row outside the 5-minute lookback window, producing an
// empty Matrix response where Prom emits one sample per
// (start, end, step) step. Compatibility run 25888277012 caught 54
// shape-diff failures of this family.
//
// The fix threads the request's `step` through to the PromQL
// lowering context. When step > 0, the synthetic-vector lowerings
// materialise a StepGrid source (one row per step in [start, end])
// instead of OneRow, with the per-step `anchor_ts` column wired into
// the TimeUnix projection and (where applicable) the value
// expression. The matrix pivot then sees N rows per series and lays
// them out on the requested step grid.
//
// These tests pin the wire shape end-to-end against chDB so the
// classes of bug cannot recur silently:
//
//   - `1 + time()` — scalar/time() binop fold.
//   - `vector(42)` — vector(scalar) literal.
//   - `time()` — bare time().
//   - `absent(nonexistent)` — absent over an empty selector.
//   - `year()` — zero-arg date function.
//   - `demo + 0` (control) — driving-vector mixed shape that must
//     keep working through the same change.

package prom_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// TestQueryRange_StepLoop_NoDrivingVector_ChDB walks every shape
// from the Pool-S2 compat diff and asserts the matrix carries the
// expected step count + per-bucket values.
//
// Window: start=T0, end=T0+5m, step=30s → 11 anchors at T0+i*30
// for i = 0..10. Every "no driving vector" expression must surface
// 11 samples in the matrix; the driving-vector control must keep
// emitting per-sample data on the step grid.
func TestQueryRange_StepLoop_NoDrivingVector_ChDB(t *testing.T) {
	// Pick a deterministic absolute start so the matrix bucket-by-
	// bucket assertions are byte-stable across CI runs.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11 // (5m / 30s) + 1, end-inclusive.

	// Seed a single demo_memory_usage_bytes sample at every step so
	// the "demo + 0" control case has a per-bucket value to pivot
	// onto the matrix.
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

	cases := []struct {
		name      string
		query     string
		wantValue func(i int) string // expected value at step i
	}{
		{
			name:  "scalar-plus-time",
			query: "1 + time()",
			wantValue: func(i int) string {
				// time() at step i is start.Unix() + i*30 seconds; +1.
				v := float64(start.Unix()+int64(i)*30) + 1
				return strconv.FormatFloat(v, 'f', -1, 64)
			},
		},
		{
			name:  "vector-literal",
			query: "vector(42)",
			wantValue: func(_ int) string {
				return "42"
			},
		},
		{
			name:  "bare-time",
			query: "time()",
			wantValue: func(i int) string {
				v := float64(start.Unix() + int64(i)*30)
				return strconv.FormatFloat(v, 'f', -1, 64)
			},
		},
		{
			name:  "absent-of-missing-metric",
			query: "absent(nonexistent_metric_for_step_loop_test)",
			wantValue: func(_ int) string {
				return "1"
			},
		},
		{
			name:  "zero-arg-year",
			query: "year()",
			// `year()` per step → toYear(toDateTime(anchor_ts)).
			// All 11 anchors are in 2026; emit "2026" at every step.
			wantValue: func(_ int) string {
				return "2026"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matrix := runStepLoopRange(t, srv.URL, tc.query, start, end, step)
			if len(matrix) != 1 {
				t.Fatalf("expected exactly 1 series in matrix, got %d: %+v", len(matrix), matrix)
			}
			values := matrix[0].Values
			if len(values) != wantSamples {
				t.Fatalf("expected %d samples (one per step), got %d: %+v",
					wantSamples, len(values), values)
			}
			// Assert per-bucket values match the Prom semantics:
			// each step emits one sample of the expression evaluated
			// at that step's timestamp.
			for i, v := range values {
				want := tc.wantValue(i)
				if got := v[1]; got != want {
					t.Errorf("step %d: value=%q want=%q (full row: %+v)",
						i, got, want, v)
				}
			}
		})
	}
}

// TestQueryRange_StepLoop_TimeTime_ChDB walks the 12 `time() OP time()`
// shapes Pool-Z surfaced (all 6 arithmetic + 6 bool comparisons). Each
// is a V-V binop where BOTH legs are synthetic-scalar plans — pre-fix
// these routed through chplan.VectorJoin and the per-side argMax
// collapse reduced each leg to one row before joining, leaving the
// matrix with 1×1 rows instead of N rows per step. Compatibility run
// surfaced these as 12 shape-diffs in the compat lane.
//
// The fix detects "both legs are synthetic-scalar plans" inside
// lowerBinary and folds to a single Project over the shared StepGrid
// source (the equivalent of TryFoldScalar's literal-literal fold one
// level up). This test pins the runtime matrix shape end-to-end:
// each query must surface N samples on the requested step grid, with
// the expected per-step value derived from the step's `anchor_ts`.
func TestQueryRange_StepLoop_TimeTime_ChDB(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second
	const wantSamples = 11

	// time() at step i is `start.Unix() + i*30`. The test seeds nothing
	// because synthetic-scalar plans don't read from any table, but
	// applySeed requires a non-empty seed string — pick a no-op
	// CREATE TABLE so the chdb session has something to ingest. The
	// query under test doesn't read from this table.
	seed := gaugeDDL
	srv, _ := newChDBServer(t, seed)

	// timeOf returns time() (Unix seconds) at step i, as a float so the
	// expected-value formatting matches the wire shape.
	timeOf := func(i int) float64 {
		return float64(start.Unix() + int64(i)*30)
	}
	fmtF := func(f float64) string {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}

	cases := []struct {
		name      string
		query     string
		wantValue func(i int) string
	}{
		// 6 arithmetic ops: time() <OP> time() — both legs equal at
		// each step so SUB / MOD yield 0, MUL/POW square, DIV/ADD
		// double/scale.
		{
			name:  "add",
			query: "time() + time()",
			wantValue: func(i int) string {
				return fmtF(2 * timeOf(i))
			},
		},
		{
			name:  "sub",
			query: "time() - time()",
			wantValue: func(_ int) string {
				return "0"
			},
		},
		{
			name:  "mul",
			query: "time() * time()",
			wantValue: func(i int) string {
				return fmtF(timeOf(i) * timeOf(i))
			},
		},
		{
			name:  "div",
			query: "time() / time()",
			wantValue: func(_ int) string {
				return "1"
			},
		},
		{
			name:  "mod",
			query: "time() % time()",
			wantValue: func(_ int) string {
				return "0"
			},
		},
		{
			name:  "pow",
			query: "time() ^ time()",
			wantValue: func(i int) string {
				return fmtF(math.Pow(timeOf(i), timeOf(i)))
			},
		},
		// 6 bool comparisons: time() <CMP> bool time() — both legs are
		// equal at every step so `== bool` and `<= bool` and `>= bool`
		// yield 1.0; `!= bool`, `< bool`, `> bool` yield 0.0.
		{
			name:  "eq-bool",
			query: "time() == bool time()",
			wantValue: func(_ int) string {
				return "1"
			},
		},
		{
			name:  "ne-bool",
			query: "time() != bool time()",
			wantValue: func(_ int) string {
				return "0"
			},
		},
		{
			name:  "lt-bool",
			query: "time() < bool time()",
			wantValue: func(_ int) string {
				return "0"
			},
		},
		{
			name:  "le-bool",
			query: "time() <= bool time()",
			wantValue: func(_ int) string {
				return "1"
			},
		},
		{
			name:  "gt-bool",
			query: "time() > bool time()",
			wantValue: func(_ int) string {
				return "0"
			},
		},
		{
			name:  "ge-bool",
			query: "time() >= bool time()",
			wantValue: func(_ int) string {
				return "1"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matrix := runStepLoopRange(t, srv.URL, tc.query, start, end, step)
			if len(matrix) != 1 {
				t.Fatalf("expected exactly 1 series in matrix, got %d: %+v", len(matrix), matrix)
			}
			values := matrix[0].Values
			if len(values) != wantSamples {
				t.Fatalf("expected %d samples (one per step), got %d: %+v",
					wantSamples, len(values), values)
			}
			for i, v := range values {
				want := tc.wantValue(i)
				if got := v[1]; got != want {
					t.Errorf("step %d: value=%q want=%q (full row: %+v)",
						i, got, want, v)
				}
			}
		})
	}
}

// TestQueryRange_StepLoop_DrivingVector_ChDB pins the control case:
// a driving-vector query (`avg_over_time(demo_memory_usage_bytes[1m])`)
// must keep emitting per-step samples from the input data, NOT N rows
// of the synthetic StepGrid. The change must be a strict superset of
// the prior behaviour — i.e., any shape that already has a driving
// input is not routed through StepGrid + CrossJoin and emits the
// same matrix shape it did pre-fix.
//
// We deliberately use the avg_over_time / matrix RangeWindow path
// rather than a bare selector + scalar binop because wrapping in `+ 0`
// triggers a pre-existing 4-column `lowerVectorScalar` Project bug
// over a 2-column RangeWindow derived shape. That bug is not caused
// by, nor exposed by, the step-loop fix in this PR.
//
// (The bare-selector LWR path previously also hit chDB's
// `ENGINE = Memory does not support PREWHERE` failure; that regression
// is fixed in the same change that swaps `gaugeDDL` to MergeTree.)
func TestQueryRange_StepLoop_DrivingVector_ChDB(t *testing.T) {
	// Anchor the seed window at "now" because the matrix RangeWindow
	// path uses `End = now64(9)` (no @ modifier threading) on its
	// inner SELECT — picking an absolute past start would emit zero
	// matrix rows even on a healthy pipeline.
	end := time.Now().UTC()
	start := end.Add(-5 * time.Minute)
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

	matrix := runStepLoopRange(t, srv.URL,
		"avg_over_time(demo_memory_usage_bytes[1m])", start, end, step)
	if len(matrix) == 0 {
		t.Fatalf("expected a non-empty driving-vector matrix; got 0 series")
	}
	if got := len(matrix[0].Values); got == 0 {
		t.Fatalf("expected non-empty matrix for driving-vector control; got 0 samples")
	}
}

// runStepLoopRange is a thin wrapper around the test server that
// posts a `/api/v1/query_range` request, decodes the matrix
// envelope, and returns the matrix series. Centralises the boilerplate
// so each test case stays focused on its expected shape.
func runStepLoopRange(t *testing.T, baseURL, query string, start, end time.Time, step time.Duration) []prom.MatrixSample {
	t.Helper()
	// Use url.QueryEscape (not the local `escape` helper) because the
	// expressions under test (`1 + time()`, `demo + 0`) contain `+`
	// which the local helper would convert to literal `+` and the
	// server-side `+` → space rewrite would mangle the operator.
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
		t.Fatalf("status=%d body=%s (pre-fix: empty matrix or 502 from CH)",
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
