//go:build chdb

// chDB-backed wire-level pins for cerberus issue #3252's
// samples-per-series-per-window pre-rejection: the operator gets a 422
// naming the axis, not a 502 relaying ClickHouse's own
// MEMORY_LIMIT_EXCEEDED, and not a wrong answer.
//
// # Why these seeds are wide rather than dense
//
// The axis the bound counts is `S x W x (S + W)` — S samples in the
// window, W the widest stored bucket array in it. The issue's own repro
// reaches the ceiling through S (a 20 Hz series, 6000 samples in a
// 5-minute window); seeding six thousand rows per case would make this
// file slow for no extra evidence, because the guard reads ONE product
// and cannot tell which factor carried it there. These cases reach the
// same product through W instead, at six rows.
//
// That is not a weaker test — it is a stricter one. The two cases below
// differ ONLY in bucket width, on the identical sample count, timestamps
// and query, so what they discriminate is the guard's cost expression
// rather than "a big query fails". A guard that counted samples alone
// would answer 200 for both.
package prom_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// expHistBoundWindow is the eval window every case here uses: six
// one-minute samples, all inside a single 5-minute lookback.
func expHistBoundWindow() (start, end time.Time) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute)
}

// expHistBoundSeed writes six exponential-histogram samples one minute
// apart whose positive bucket array is `width` elements wide. Count rises
// with time so the counter folds have something real to fold.
//
// The array is built with `range`/`arrayMap` rather than written out as a
// literal, so a width of 1200 costs one expression instead of 1200 tokens
// per row.
func expHistBoundSeed(t *testing.T, start time.Time, width int) string {
	t.Helper()
	rows := make([]string, 0, 6)
	for i := range 6 {
		ts := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05.000000000")
		rows = append(rows, fmt.Sprintf(
			"    ('dense_exp_hist', '', '', map('x', '1'), toDateTime64('%s', 9), %d, %d.0, 0, 0, 0, "+
				"arrayMap(n -> toUInt64(1), range(%d)), 0, [])",
			ts, (i+1)*10, (i+1)*10, width,
		))
	}
	return metaShapedMetricsDDL +
		"\nINSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, " +
		"PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		strings.Join(rows, ",\n") + ";"
}

// expHistBoundNames are the window folds a bound on the window's own
// groupArrays has to cover. All seven of the FOLD family plus the fused
// quantile form the issue measured — that last one matters because it
// reduces through a DIFFERENT lowering
// (internal/promql's expHistogramWindowStage family) than the bare folds.
var expHistBoundNames = []string{
	"rate(dense_exp_hist[5m])",
	"increase(dense_exp_hist[5m])",
	"delta(dense_exp_hist[5m])",
	"irate(dense_exp_hist[5m])",
	"idelta(dense_exp_hist[5m])",
	"sum_over_time(dense_exp_hist[5m])",
	"avg_over_time(dense_exp_hist[5m])",
	"histogram_quantile(0.95, sum(rate(dense_exp_hist[5m])))",
}

// TestQuery_ExpHistogramWindowSampleBound_RejectedAt422_ChDB is the
// rejection. At width 1200 the six-sample window costs
// 6 x 1200 x 1206 = 8,683,200 units against the 5,000,000 a 1 GiB cap
// grants, so every window fold must refuse rather than build the fold
// ClickHouse would then die inside.
func TestQuery_ExpHistogramWindowSampleBound_RejectedAt422_ChDB(t *testing.T) {
	_, end := expHistBoundWindow()
	start, _ := expHistBoundWindow()
	srv, _ := newChDBServer(t, expHistBoundSeed(t, start, 1200))

	for _, query := range expHistBoundNames {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("%s: status: got %d, want %d; body=%s",
					query, status, http.StatusUnprocessableEntity, body)
			}
			if !strings.Contains(body, chplan.ExpHistogramWindowSampleBudgetMessage) {
				t.Errorf("%s: body does not name the bound: %s", query, body)
			}
			if strings.Contains(body, "DB::Exception") {
				t.Errorf("%s: body leaks the ClickHouse exception: %s", query, body)
			}
		})
	}
}

// TestQueryRange_ExpHistogramWindowSampleBound_RejectedAt422_ChDB is the
// same rejection over /api/v1/query_range, which reduces through
// chplan.RangeBucketFanout rather than the instant chplan.Aggregate. The
// range shape is the one #3252 measured, and it is a different node, so
// running both is what proves the guard is not pinned to whichever
// reduction happens to be an Aggregate.
func TestQueryRange_ExpHistogramWindowSampleBound_RejectedAt422_ChDB(t *testing.T) {
	start, end := expHistBoundWindow()
	srv, _ := newChDBServer(t, expHistBoundSeed(t, start, 1200))

	for _, query := range expHistBoundNames {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf(
				"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60",
				srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(),
			))
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("%s: status: got %d, want %d; body=%s",
					query, status, http.StatusUnprocessableEntity, body)
			}
			if !strings.Contains(body, chplan.ExpHistogramWindowSampleBudgetMessage) {
				t.Errorf("%s: body does not name the bound: %s", query, body)
			}
		})
	}
}

// TestQuery_ExpHistogramWindowSampleBound_OrdinaryWidthAnswers_ChDB is the
// discriminating control, and the reason the seeds are parameterised on
// width alone: SAME six samples, SAME timestamps, SAME queries, width 100
// instead of 1200. The cost is 6 x 100 x 106 = 63,600 units, two orders
// under the ceiling, so every one of these must answer 200.
//
// Without it the rejection test above would pass just as well against a
// guard that refused every exp-histogram window query outright — which is
// the failure mode a resource bound is most likely to ship with, and the
// one an operator notices last.
func TestQuery_ExpHistogramWindowSampleBound_OrdinaryWidthAnswers_ChDB(t *testing.T) {
	start, end := expHistBoundWindow()
	srv, _ := newChDBServer(t, expHistBoundSeed(t, start, 100))

	for _, query := range expHistBoundNames {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			if status != http.StatusOK {
				t.Fatalf("%s: status: got %d, want 200 — 63,600 cost units is two orders under the "+
					"ceiling and this query must answer; body=%s", query, status, body)
			}
			if strings.Contains(body, chplan.ExpHistogramWindowSampleBudgetMessage) {
				t.Errorf("%s: answered 200 but still names the bound: %s", query, body)
			}
		})
	}
}
