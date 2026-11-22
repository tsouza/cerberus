package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The window every case of TestHistogramQuantileStatementClass lowers over:
// an instant at hqClassEnd, or the hour before it at hqClassStep.
var (
	hqClassEnd   = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	hqClassStart = hqClassEnd.Add(-time.Hour)
)

// hqClassStep is the range cases' evaluation step.
const hqClassStep = time.Minute

// The empty-ladder guards hqClassicWriters.emptyLadderNaN renders, as the
// classic kernel's SQL spells them.
const (
	hqClassBranchGuard = "if(length(`BucketCounts`) = 0, nan, if(`_cerb_hqc_observations` = 0, nan, "
	hqClassFactorGuard = " * if(`_cerb_hqc_observations` = 0, nan, 1)"
)

// hqClassOutputBoundary is the singleton ARRAY JOIN over the quantile's answer
// row that no classic statement may carry.
const hqClassOutputBoundary = "_cerb_histogram_output"

// TestHistogramQuantileStatementClass lowers the classic quantile shapes of
// #4518 and #4529 as instant and range queries, the range ones under both the
// fan-out and the native rate lowerer (the one a server with the ts_grid
// functions boots with), and pins each statement's class:
//
//   - the kernel over per-bucket counts (a bare bucket selector, and a
//     selector that names no bucket metric) guards the empty ladder with
//     the branch chain;
//   - the kernel over a cumulative ladder (the per-series quantile of a
//     rate, whose memory the factor bounds, and the layout merge of an
//     aggregation by `le`) guards it with the factor;
//   - no classic statement hands its answer up through a singleton ARRAY
//     JOIN, which bounded nothing the factor had not already bounded and
//     cost every statement a pipeline stage.
func TestHistogramQuantileStatementClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query      string
		cumulative bool
	}{
		{query: `histogram_quantile(0.5, http_server_request_duration_bucket{method="GET"})`},
		{query: `histogram_quantile(0.5, http_server_request_duration_bucket)`},
		{query: `histogram_quantile(0.9, sum by (le) (http_server_request_duration_bucket))`, cumulative: true},
		{query: `histogram_quantile(0.9, http_requests_total)`},
		{query: `histogram_quantile(0.9, rate(http_server_request_duration_bucket{method="GET"}[5m]))`, cumulative: true},
		{query: `histogram_quantile(0.9, rate(http_server_request_duration_bucket[5m]))`, cumulative: true},
		{query: `histogram_quantile(0.9, rate(http_requests_total[5m]))`, cumulative: true},
		{query: `histogram_quantile(0.9, sum by (le) (rate(http_requests_total[5m])))`, cumulative: true},
	} {
		for _, mode := range []hqClassMode{hqClassInstant, hqClassRangeFanout, hqClassRangeNative} {
			t.Run(string(mode)+" "+tc.query, func(t *testing.T) {
				t.Parallel()
				plan := hqClassLower(t, tc.query, mode)
				quantiles := 0
				chplan.Walk(plan, func(n chplan.Node) bool {
					if hq, ok := n.(*chplan.HistogramQuantile); ok {
						quantiles++
						if hq.BucketCountsCumulative != tc.cumulative {
							t.Errorf("the quantile reads BucketCountsCumulative=%v; want %v", hq.BucketCountsCumulative, tc.cumulative)
						}
					}
					return true
				})
				if quantiles != 1 {
					t.Fatalf("the plan carries %d classic quantiles; want 1", quantiles)
				}
				sql, _, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatalf("Emit: %v", err)
				}
				if strings.Contains(sql, hqClassOutputBoundary) {
					t.Errorf("the statement hands the answer up through a singleton ARRAY JOIN (%s)\nSQL: %s", hqClassOutputBoundary, sql)
				}
				want, unwanted := hqClassBranchGuard, hqClassFactorGuard
				if tc.cumulative {
					want, unwanted = hqClassFactorGuard, hqClassBranchGuard
				}
				if !strings.Contains(sql, want) || strings.Contains(sql, unwanted) {
					t.Errorf("the kernel's empty-ladder guard: want %q and not %q\nSQL: %s", want, unwanted, sql)
				}
			})
		}
	}
}

// hqClassMode names how TestHistogramQuantileStatementClass lowers a query.
type hqClassMode string

const (
	hqClassInstant     hqClassMode = "instant"
	hqClassRangeFanout hqClassMode = "range"
	hqClassRangeNative hqClassMode = "range native-rate"
)

// hqClassLower parses and lowers query at hqClassEnd, or over the hour before
// it for a range mode.
func hqClassLower(t *testing.T, query string, mode hqClassMode) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	var plan chplan.Node
	switch mode {
	case hqClassInstant:
		plan, err = LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), hqClassEnd, hqClassEnd)
	case hqClassRangeFanout:
		plan, err = LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), hqClassStart, hqClassEnd, hqClassStep)
	case hqClassRangeNative:
		plan, err = LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), hqClassStart, hqClassEnd, hqClassStep,
			LowerOpts{Lowerers: RangeLowerers{Rate: NativeRateLowerer{Fallback: FanoutRateLowerer{}}}})
	}
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	return plan
}
