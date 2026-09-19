//go:build chdb

// Regression coverage for the query_range anchor grid of every backward-walked
// fan-out emitter: RangeBucketFanout/RangeLWR (internal/chsql/
// range_bucket_fanout.go, internal/chsql/range_lwr.go) and the RangeWindow
// family (internal/chsql/range_window.go and its per-shape siblings).
//
// All of them walk their anchor grid BACKWARD from a single "newest
// anchor" base to reuse the same dist-behind-anchor membership math a
// staleness lookback needs (see startAnchoredGridEnd's own doc). That walk
// answers the same anchor timestamps Prometheus's Start-anchored
// `Start, Start+Step, …` query_range grid does ([chplan.StepGrid]'s
// contract) only when `(End-Start)` is an exact multiple of `Step`. Every
// generator in test/property/gen always constructs an exact-multiple span
// (gen/promql_range.go's `endSec := startSec + (rangeGridPoints-1)*step`),
// so this file is the one place a non-exact-multiple span is exercised —
// gen/promql_range.go's own expression pool also excludes
// histogram_quantile/changes/resets entirely, so RangeBucketFanout's
// anchor grid otherwise has NO property/oracle coverage in this repo.
package property_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// TestPromQL_RangeBucketFanoutAnchorsMatchStartAnchoredGrid seeds a
// monotonically-growing exp-histogram (no genuine counter reset ever, so
// resets() must read 0 and changes() must read exactly
// samplesInWindow-1 at EVERY anchor) and runs changes()/resets() —
// range-vector functions [lowerExpHistogramResetsRange] lowers onto
// RangeBucketFanout — over a query_range grid whose (end-start) is NOT an
// exact multiple of step, alongside an exact-multiple control over the
// identical data.
//
// Before the anchor fix, the exact-multiple case answered correctly but
// the non-exact-multiple case reported every row at anchors shifted by
// (end-start) mod step (backward from the raw end instead of forward from
// start), which either changed the reported timestamp of a real answer or
// dropped an anchor's row entirely.
func TestPromQL_RangeBucketFanoutAnchorsMatchStartAnchoredGrid(t *testing.T) {
	const stepSeconds = 15
	const steps = 30
	const anchorMs = int64(1_700_000_000_000)
	const metric = "request_latency_exp_hist"

	s := property.SeriesData{MetricName: metric, Labels: map[string]string{"instance": "a"}}
	base := []uint64{1, 2, 3, 4}
	for step := 0; step < steps; step++ {
		n := step + 1
		counts := make([]uint64, len(base))
		for i, c := range base {
			counts[i] = c * uint64(n)
		}
		s.Points = append(s.Points, property.Point{
			TimestampMs: anchorMs + int64(step)*stepSeconds*1000,
			Histogram: &property.NativeHistogram{
				Count:                10 * uint64(n),
				Sum:                  35.0 * float64(n),
				PositiveBucketCounts: counts,
			},
		})
	}
	ddl := gen.ExpHistogramDDL([]property.SeriesData{s})

	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl)

	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// The first anchor with a full [5m] (20-sample) window, so every
	// anchor below reads a stable 19 changes / 0 resets regardless of
	// exactly which timestamps the grid lands on.
	dataStartSec := anchorMs / 1000
	gridStart := dataStartSec + 5*60
	const step = 60

	assertGrid := func(t *testing.T, start, end int64) {
		for _, fn := range []string{"changes", "resets"} {
			fn := fn
			t.Run(fn, func(t *testing.T) {
				out := runCerberusRange(context.Background(), srv.URL,
					fmt.Sprintf("%s(%s[5m])", fn, metric), start, end, step)
				if out.Err != nil {
					t.Fatalf("range query error: %v", out.Err)
				}
				want := 19.0
				if fn == "resets" {
					want = 0
				}
				for _, anchor := range gridAnchors(start, end, step) {
					anchorMs := anchor * 1000
					found := false
					for _, row := range out.Rows {
						if row.TimestampMs != anchorMs {
							continue
						}
						found = true
						if row.Value != want {
							t.Fatalf("%s(%s[5m]) at anchor=%d = %v, want %v", fn, metric, anchor, row.Value, want)
						}
					}
					if !found {
						t.Fatalf("%s(%s[5m]): missing a row at requested anchor=%d (grid=%d..%d/%ds)\nrows=%+v",
							fn, metric, anchor, start, end, step, out.Rows)
					}
				}
			})
		}
	}

	// Control: span=120 is an exact multiple of step=60 — the backward and
	// forward anchor walks always agreed here, exact fix or not.
	t.Run("exact_multiple_span", func(t *testing.T) {
		assertGrid(t, gridStart, gridStart+120)
	})
	// The regression: span=135 is NOT a multiple of step=60, so
	// Prometheus's own Start-anchored grid (gridStart, +60, +120) and a
	// naive End-anchored backward walk (gridStart+135, +75, +15) disagree
	// about which three timestamps are anchors.
	t.Run("non_exact_multiple_span", func(t *testing.T) {
		assertGrid(t, gridStart, gridStart+135)
	})
}

// rangeWindowAnchorStepSeconds is the query_range step every row of
// [TestRangeWindowAnchorsMatchStartAnchoredGrid] evaluates at.
const rangeWindowAnchorStepSeconds = int64(60)

// rangeWindowAnchorSampleSeconds is the seeded sample spacing: four samples
// per step, so every `[2m]` window holds eight samples and no anchor is
// ever empty (an empty anchor produces no row, which would let a shifted
// grid pass as "missing data").
const rangeWindowAnchorSampleSeconds = int64(15)

// rangeWindowAnchorSampleCount is the seeded sample count per series — a
// 25-minute ramp, long enough that the widest drawn grid (see
// rangeWindowAnchorMaxWholeSteps) plus its `[2m]` window and a `1m` offset
// sits well inside the data.
const rangeWindowAnchorSampleCount = 100

// rangeWindowAnchorMaxWholeSteps bounds how many whole steps a drawn grid
// span holds before its non-multiple remainder is added.
const rangeWindowAnchorMaxWholeSteps = 6

// rangeWindowAnchorMinWholeSteps is the fewest whole steps a drawn span
// holds: two, so the grid always has at least three anchors and a shifted
// walk cannot coincide with the request grid by having a single anchor.
const rangeWindowAnchorMinWholeSteps = 2

// drawNonMultipleSpan draws a grid span `k*step + r` with `1 <= r < step`,
// so `(end-start) mod step != 0` for every draw — the shape every
// generator under test/property/gen never constructs (they build exact
// multiples only). The seed is logged so a failing draw replays.
func drawNonMultipleSpan(t *testing.T, stepSec int64) int64 {
	t.Helper()
	seed := time.Now().UnixNano()
	t.Logf("non-multiple span draw seed=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	whole := int64(rng.Intn(rangeWindowAnchorMaxWholeSteps-rangeWindowAnchorMinWholeSteps+1)) + rangeWindowAnchorMinWholeSteps
	remainder := int64(rng.Intn(int(stepSec-1))) + 1
	return whole*stepSec + remainder
}

// assertOutputTimestampsAreStartAnchoredGrid asserts that the set of
// timestamps `rows` reports is EXACTLY `{start + i*step : start + i*step <=
// end}` — no anchor missing (a shifted grid drops the request's own anchors)
// and no timestamp the request never named (a shifted grid reports at
// `end - i*step` instead).
func assertOutputTimestampsAreStartAnchoredGrid(t *testing.T, rows []property.OutcomeRow, start, end, stepSec int64) {
	t.Helper()
	want := map[int64]bool{}
	for _, a := range gridAnchors(start, end, stepSec) {
		want[a*1000] = false
	}
	for _, row := range rows {
		seen, ok := want[row.TimestampMs]
		if !ok {
			t.Fatalf("reported timestamp %d is not a request grid anchor (grid=%d..%d/%ds)\nrows=%+v",
				row.TimestampMs, start, end, stepSec, rows)
		}
		if seen {
			t.Fatalf("anchor %d reported twice (grid=%d..%d/%ds)\nrows=%+v", row.TimestampMs, start, end, stepSec, rows)
		}
		want[row.TimestampMs] = true
	}
	for ts, seen := range want {
		if !seen {
			t.Fatalf("missing a row at requested anchor=%d (grid=%d..%d/%ds)\nrows=%+v", ts, start, end, stepSec, rows)
		}
	}
}

// TestRangeWindowAnchorsMatchStartAnchoredGrid is the RangeWindow sibling of
// [TestPromQL_RangeBucketFanoutAnchorsMatchStartAnchoredGrid]: every
// PromQL range function the fan-out emitter (internal/chsql/range_window.go
// and its per-shape siblings) lowers onto a RangeWindow walks its anchor
// grid backward from one "newest anchor" base, exactly as
// RangeBucketFanout/RangeLWR do, so the same non-multiple-span shift
// applies to every one of them. Each row runs one such function over a
// dense counter on a query_range grid whose span is drawn to NOT be a
// multiple of step, and asserts the reported timestamps are exactly the
// request's own `start + i*step` anchors.
func TestRangeWindowAnchorsMatchStartAnchoredGrid(t *testing.T) {
	const anchorMs = int64(1_700_000_000_000)
	const metric = "anchor_grid_requests_total"

	s := property.SeriesData{MetricName: metric, Labels: map[string]string{"job": "api"}}
	for i := 0; i < rangeWindowAnchorSampleCount; i++ {
		s.Points = append(s.Points, property.Point{
			TimestampMs: anchorMs + int64(i)*rangeWindowAnchorSampleSeconds*1000,
			Value:       float64(10 * i),
		})
	}
	cli := chclienttest.NewChDB(t)
	cli.Seed(t, gen.CounterSumDDL([]property.SeriesData{s}))
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dataStartSec := anchorMs / 1000
	// Leave a full [2m] window plus the 1m offset row's shift before the
	// first anchor, so every anchor of every row reads a full window.
	gridStart := dataStartSec + 5*60
	step := rangeWindowAnchorStepSeconds

	rows := []struct {
		name  string
		query string
	}{
		{"rate", fmt.Sprintf("rate(%s[2m])", metric)},
		{"increase", fmt.Sprintf("increase(%s[2m])", metric)},
		{"delta", fmt.Sprintf("delta(%s[2m])", metric)},
		{"sum_over_time", fmt.Sprintf("sum_over_time(%s[2m])", metric)},
		{"count_over_time", fmt.Sprintf("count_over_time(%s[2m])", metric)},
		{"sum_rate", fmt.Sprintf("sum(rate(%s[2m]))", metric)},
		{"max_over_time_subquery", fmt.Sprintf("max_over_time(%s[2m:30s])", metric)},
		{"rate_offset", fmt.Sprintf("rate(%s[2m] offset 1m)", metric)},
	}
	for _, tc := range rows {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			span := drawNonMultipleSpan(t, step)
			out := runCerberusRange(context.Background(), srv.URL, tc.query, gridStart, gridStart+span, step)
			if out.Err != nil {
				t.Fatalf("%s: range query error: %v", tc.query, out.Err)
			}
			assertOutputTimestampsAreStartAnchoredGrid(t, out.Rows, gridStart, gridStart+span, step)
		})
	}
}

// anchorGridLogsDDL is the otel_logs projection the LogQL range-aggregation
// lowering reads (the same column set internal/api/loki's chDB tests seed).
const anchorGridLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    SeverityText LowCardinality(String) DEFAULT '',
    SeverityNumber UInt8 DEFAULT 0,
    ResourceAttributes Map(String, String),
    ServiceName String DEFAULT '',
    LogAttributes Map(String, String),
    ScopeName String DEFAULT '',
    ScopeVersion String DEFAULT '',
    EventName LowCardinality(String) DEFAULT '',
    TraceId String DEFAULT '',
    SpanId String DEFAULT '',
    TraceFlags UInt8 DEFAULT 0
) ENGINE = Memory;`

// TestLogQL_RangeWindowAnchorsMatchStartAnchoredGrid is the LogQL row of
// [TestRangeWindowAnchorsMatchStartAnchoredGrid]: `count_over_time` over a
// log stream lowers onto the SAME RangeWindow node and emitter PromQL's
// range functions do (internal/logql/range_aggregation.go sets the grid
// from the request's start/end/step exactly as promql's
// applyStepGridFanout does), so it must answer on the same Start-anchored
// grid.
func TestLogQL_RangeWindowAnchorsMatchStartAnchoredGrid(t *testing.T) {
	const tsFmt = "2006-01-02 15:04:05.000000000"
	dataStart := time.Unix(1_700_000_000, 0).UTC()
	lines := make([]string, 0, rangeWindowAnchorSampleCount)
	for i := 0; i < rangeWindowAnchorSampleCount; i++ {
		ts := dataStart.Add(time.Duration(i) * time.Duration(rangeWindowAnchorSampleSeconds) * time.Second).Format(tsFmt)
		lines = append(lines, fmt.Sprintf("    (toDateTime64('%s', 9), 'line', map('service_name', 'api'))", ts))
	}
	ddl := anchorGridLogsDDL + "\nINSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES\n" +
		strings.Join(lines, ",\n") + ";"

	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl)
	h := loki.New(cli, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gridStart := dataStart.Unix() + 5*60
	step := rangeWindowAnchorStepSeconds
	span := drawNonMultipleSpan(t, step)
	end := gridStart + span

	reqURL := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d&step=%ds",
		srv.URL, url.QueryEscape(`count_over_time({service_name="api"}[2m])`), gridStart, end, step)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var parsed struct {
		Data struct {
			ResultType string              `json:"resultType"`
			Result     []loki.MatrixSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if parsed.Data.ResultType != "matrix" || len(parsed.Data.Result) != 1 {
		t.Fatalf("resultType=%q series=%d, want one matrix series: %s", parsed.Data.ResultType, len(parsed.Data.Result), body)
	}
	var rows []property.OutcomeRow
	for _, sample := range parsed.Data.Result[0].Values {
		ts, val, perr := wire.ParseSample(sample)
		if perr != nil {
			t.Fatalf("parse sample: %v", perr)
		}
		rows = append(rows, property.OutcomeRow{TimestampMs: ts, Value: val})
	}
	assertOutputTimestampsAreStartAnchoredGrid(t, rows, gridStart, end, step)
}
