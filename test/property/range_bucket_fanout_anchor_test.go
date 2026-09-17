//go:build chdb

// Regression coverage for RangeBucketFanout/RangeLWR's query_range anchor
// grid (internal/chsql/range_bucket_fanout.go, internal/chsql/range_lwr.go).
//
// Both emitters walk their anchor grid BACKWARD from a single "newest
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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
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
