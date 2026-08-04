//go:build chdb

// chDB-backed regression coverage for OOM #2: PromQL /api/v1/query_range
// buffering O(input) instead of O(output) before truncating in Go.
//
// matrixFromCursor (internal/api/prom/handler.go) drains the streaming
// cursor row-by-row into per-series buffers, then pivots to a Matrix. The
// memory it holds is therefore exactly the row count the cursor yields. The
// gateway OOMed because, for a wide window with dense raw samples, an
// un-collapsed scan returned one row per RAW SAMPLE — O(series × samples) —
// and matrixFromCursor buffered all of it before the matrix shape (and any
// truncation) bit. The Pool-AK range-mode rework fixed this by collapsing
// the per-step LWR on the SQL side: the scan now returns one row per
// (series, step anchor) — O(series × step) — so the drain is bounded by the
// OUTPUT matrix size, independent of how many raw samples each window holds.
//
// The blind spot that let OOM #2 through: every prior range-mode test used a
// FIXED tiny seed (one raw sample per step), so "drain everything" and
// "drain one-per-anchor" produced identical counts — the input axis was
// never varied, so the drain never had room to diverge from the output. This
// test varies the input axis (raw sample DENSITY per window) while holding
// the output bound (series × step) fixed, and asserts the drain tracks
// output, not input. It is wired through the shared boundsdrain harness so
// the Tempo /api/search trace-limit pushdown (OOM #1) rides as the proven
// reference row.

package prom_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// Bounds-drain seed scale for the PromQL range path. The point is to make
// the INPUT axis (raw samples) dwarf the OUTPUT axis (series × step) so a
// bounded drain (O(output)) and an unbounded one (O(input)) produce wildly
// different counts:
//
//   - drainSeriesCount distinct series (the cardinality axis),
//   - drainStepCount step anchors across the request window (the output axis),
//   - drainSamplesPerWindow raw samples seeded inside EACH per-step staleness
//     window (the input-density axis the prior fixed-seed tests never varied).
//
// Output bound: drainSeriesCount × drainStepCount — one row per
// (series, anchor) after the per-step LWR collapse.
// Full seed: drainSeriesCount × drainStepCount × drainSamplesPerWindow —
// what an un-collapsed scan would drain (the OOM count).
//
// With drainSamplesPerWindow = 6 the full seed is 6× the bound, comfortably
// past boundsDrainFudge (2×): a bounded drain passes, an O(input) drain fails.
const (
	drainSeriesCount      = 25
	drainStepCount        = 13
	drainSamplesPerWindow = 6
)

// drainStep is the query_range step. The staleness window is 5m, so a step
// of 1m keeps every step anchor's (anchor-5m, anchor] window overlapping the
// prior anchors' samples — but the per-step argMax collapse still yields
// exactly one row per (series, anchor), which is the property under test.
const drainStep = time.Minute

// boundsDrainPromServer builds a chDB-backed prom handler + httptest server
// and returns both, so the caller can install the streaming-drain hook on
// the handler before driving /api/v1/query_range. The plain newChDBServer
// helper does not expose the handler, and the drain hook is the only way an
// external test reads the streaming-path drain count.
func boundsDrainPromServer(t *testing.T, seed string) (*prom.Handler, *httptest.Server) {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

// seedManySeriesDenseWindows plants drainSeriesCount series, each carrying
// drainSamplesPerWindow distinct raw samples inside every per-step window
// across drainStepCount anchors. The raw row count is therefore
// drainSeriesCount × drainStepCount × drainSamplesPerWindow — the input axis
// — while the bare-selector range query collapses to one row per
// (series, anchor) on the SQL side — the output axis.
func seedManySeriesDenseWindows(start time.Time) (seed string, fullSeed int64) {
	rows := make([]string, 0, drainSeriesCount*drainStepCount*drainSamplesPerWindow)
	for s := 0; s < drainSeriesCount; s++ {
		inst := fmt.Sprintf("inst-%03d", s)
		for step := 0; step < drainStepCount; step++ {
			anchor := start.Add(time.Duration(step) * drainStep)
			// Spread drainSamplesPerWindow raw samples through the seconds
			// leading up to (and including) the anchor, all inside the 5m
			// staleness window so the per-step LWR sees every one of them and
			// must collapse them to a single argMax(Value, TimeUnix) row.
			for k := 0; k < drainSamplesPerWindow; k++ {
				ts := anchor.Add(-time.Duration(k) * time.Second).
					Format("2006-01-02 15:04:05.000000000")
				rows = append(rows, fmt.Sprintf(
					`('demo_memory_usage_bytes', map('instance', '%s'), toDateTime64('%s', 9), %d.0)`,
					inst, ts, s*1000+step*10+k,
				))
			}
		}
	}
	seed = gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(rows, ",\n  ") + ";"
	return seed, int64(len(rows))
}

// promRangeDrainCase builds the PromQL /api/v1/query_range bounds-drain row.
// It seeds many series with dense per-window samples, installs the drain
// hook, drives a bare-selector range query at a fixed step, and returns the
// streaming-drain count plus the full seed for the harness's two assertions.
func promRangeDrainCase() chclienttest.BoundsDrainCase {
	return chclienttest.BoundsDrainCase{
		Name:        "promql/query_range/bare_selector",
		OutputBound: int64(drainSeriesCount * drainStepCount),
		Run: func(t *testing.T) (drain, fullSeed int64) {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			end := start.Add(time.Duration(drainStepCount-1) * drainStep)

			seed, full := seedManySeriesDenseWindows(start)
			h, srv := boundsDrainPromServer(t, seed)

			var got int64
			h.SetOnRangeDrain(func(n int64) { got = n })

			matrix := runRangeModeQueryRange(t, srv.URL,
				"demo_memory_usage_bytes", start, end, drainStep)

			// Sanity: the output really is the (series × step) matrix the
			// bound is named for — every series present, every step anchor
			// populated. (If the SQL returned nothing the drain would be a
			// vacuous 0 and the bound would pass for the wrong reason.)
			if len(matrix) != drainSeriesCount {
				t.Fatalf("got %d series, want %d — the output matrix is not the (series × step) shape the bound names",
					len(matrix), drainSeriesCount)
			}
			for _, ser := range matrix {
				if len(ser.Values) != drainStepCount {
					t.Fatalf("series %v: %d samples, want %d (one per step anchor)",
						ser.Metric, len(ser.Values), drainStepCount)
				}
			}
			return got, full
		},
	}
}

// Bounds-drain seed scale for the PromQL INSTANT subquery path
// (sum_over_time(metric[range:step]) evaluated via /api/v1/query, not
// /api/v1/query_range). #1515 flagged this shape as one of the two
// recurring axes ("the instant query shape") missing from the harness.
//
// A subquery lowers to a NESTED chplan.RangeWindow: the inner node rasters
// the subquery's own [range:step] into one row per (series, inner anchor)
// via the same per-anchor LWR collapse promRangeDrainCase exercises; the
// outer node (step=0 — a single instant evaluation) aggregates the inner
// anchors' values with groupArray/arraySum INSIDE the same emitted SQL
// statement and returns exactly one row per series. The whole chain is one
// ClickHouse round trip, so Engine.Query's res.Inspected is bounded to
// O(series) regardless of how many raw samples or inner anchors feed it —
// UNLESS a regression drops the outer GROUP BY, which is exactly the
// #1515 gap this case guards against.
//
//   - drainInstantSeriesCount distinct series (the cardinality axis),
//   - drainInstantInnerStepCount inner subquery anchors (the raster-density
//     axis a regression could fail to collapse),
//   - drainInstantSamplesPerInnerWindow raw samples seeded inside EACH
//     inner anchor's staleness window (the input-density axis).
//
// Output bound: drainInstantSeriesCount — one row per series after the
// outer instant (step=0) aggregation.
// Full seed: drainInstantSeriesCount × drainInstantInnerStepCount ×
// drainInstantSamplesPerInnerWindow — what an unbounded drain would pull.
const (
	drainInstantSeriesCount           = 15
	drainInstantInnerStepCount        = 13
	drainInstantSamplesPerInnerWindow = 6
)

// drainInstantInnerStep is the subquery's own step (the "1m" in
// "[13m:1m]"); drainInstantInnerStepCount anchors spaced by this step span
// exactly the subquery's declared range, so the bracket literal
// promInstantSubqueryDrainCase builds ("(drainInstantInnerStepCount-1) *
// drainInstantInnerStep") covers every seeded anchor.
const drainInstantInnerStep = time.Minute

// seedManySeriesDenseInnerWindows plants drainInstantSeriesCount series,
// each carrying drainInstantSamplesPerInnerWindow raw samples inside every
// one of the subquery's drainInstantInnerStepCount inner-anchor windows,
// anchored so the newest sample lands exactly at `instant` and the oldest
// inner anchor lands at instant - (drainInstantInnerStepCount-1) * step.
func seedManySeriesDenseInnerWindows(instant time.Time) (seed string, fullSeed int64) {
	rows := make([]string, 0, drainInstantSeriesCount*drainInstantInnerStepCount*drainInstantSamplesPerInnerWindow)
	for s := 0; s < drainInstantSeriesCount; s++ {
		inst := fmt.Sprintf("inst-instant-%03d", s)
		for step := 0; step < drainInstantInnerStepCount; step++ {
			anchor := instant.Add(-time.Duration(drainInstantInnerStepCount-1-step) * drainInstantInnerStep)
			// Spread drainInstantSamplesPerInnerWindow raw samples through the
			// seconds leading up to (and including) the anchor, mirroring
			// seedManySeriesDenseWindows above — dense enough that the inner
			// per-anchor LWR collapse (not "there happened to be one sample")
			// is what the anti-vacuous full-seed margin exercises.
			for k := 0; k < drainInstantSamplesPerInnerWindow; k++ {
				ts := anchor.Add(-time.Duration(k) * time.Second).
					Format("2006-01-02 15:04:05.000000000")
				rows = append(rows, fmt.Sprintf(
					`('demo_memory_usage_bytes', map('instance', '%s'), toDateTime64('%s', 9), %d.0)`,
					inst, ts, s*1000+step*10+k,
				))
			}
		}
	}
	seed = gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(rows, ",\n  ") + ";"
	return seed, int64(len(rows))
}

// promInstantSubqueryDrainCase builds the PromQL /api/v1/query instant-
// subquery bounds-drain row. It seeds many series with dense per-inner-
// anchor samples, installs the instant-drain hook, drives
// sum_over_time(metric[range:step]) at a fixed instant, and returns the
// eager-path drain count plus the full seed for the harness's two
// assertions.
func promInstantSubqueryDrainCase() chclienttest.BoundsDrainCase {
	return chclienttest.BoundsDrainCase{
		Name:        "promql/query/instant_subquery",
		OutputBound: int64(drainInstantSeriesCount),
		Run: func(t *testing.T) (drain, fullSeed int64) {
			instant := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

			seed, full := seedManySeriesDenseInnerWindows(instant)
			h, srv := boundsDrainPromServer(t, seed)

			var got int64
			h.SetOnInstantDrain(func(n int64) { got = n })

			subqueryRange := time.Duration(drainInstantInnerStepCount-1) * drainInstantInnerStep
			q := fmt.Sprintf("sum_over_time(demo_memory_usage_bytes[%s:%s])",
				formatPromDuration(subqueryRange), formatPromDuration(drainInstantInnerStep))
			reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(q), instant.Unix())

			resp, err := http.Get(reqURL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
			}
			var out struct {
				Data struct {
					ResultType string              `json:"resultType"`
					Result     []prom.VectorSample `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Data.ResultType != "vector" {
				t.Fatalf("resultType = %q, want vector (instant sum_over_time collapses to one point per series)", out.Data.ResultType)
			}

			// Anti-vacuous shape check: every seeded series must appear, else a
			// selector/window bug could silently zero the drain and the bound
			// would pass for the wrong reason.
			if len(out.Data.Result) != drainInstantSeriesCount {
				t.Fatalf("got %d series, want %d — the output vector is not the one-point-per-series shape the bound names",
					len(out.Data.Result), drainInstantSeriesCount)
			}

			return got, full
		},
	}
}

// formatPromDuration renders a time.Duration in the compact unit PromQL's
// duration literal grammar accepts (e.g. "1m"), matching how subquery
// bracket literals are spelled throughout test/spec/promql's subquery_*
// fixtures.
func formatPromDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// TestBoundsDrain_ResultBufferingHandlers is the shared bounds-drain gate:
// each row seeds at scale, drives a result-buffering handler, and the harness
// asserts the drain is O(output) (≤ OutputBound × fudge) AND a real reduction
// below the full seed. The PromQL range row is the high-value regression for
// OOM #2; the PromQL instant-subquery row guards the same collapse at the
// nested-RangeWindow shape #1515 flagged as untested; the Tempo row (in
// package tempo_test) is the proven reference for OOM #1.
// The behavioural falsifiability of the range row is proven by
// TestBoundsDrain_ResultBufferingHandlers itself: it runs matrixFromCursor end
// to end against a real chDB drain, so neutering the SQL-side per-step argMax
// collapse (see internal/chsql/range_lwr.go) turns the measured drain from
// drainSeriesCount × drainStepCount into the full seed (× drainSamplesPerWindow),
// failing the bound assertion. The PR records the measured before/after
// (325 → 8250). The predicate that does the rejecting is unit-tested directly in
// internal/chclienttest/boundsdrain_test.go — no separate, literal-only test
// here, which would assert nothing about the handler.
func TestBoundsDrain_ResultBufferingHandlers(t *testing.T) {
	chclienttest.RunBoundsDrain(t, []chclienttest.BoundsDrainCase{
		promRangeDrainCase(),
		promInstantSubqueryDrainCase(),
	})
}
