//go:build chdb

// chDB-backed wire-level pins for cerberus issue #3468's RangeBucketFanout
// collapse-output group-count bound: the operator gets a 422 naming the
// axis, not a 502 relaying ClickHouse's own MEMORY_LIMIT_EXCEEDED (the
// #2522 failure mode every case in this switch guards against — see
// handler.go's own RangeBucketFanoutGroupBudgetMessage case comment), and
// not a wrong answer.
//
// # Why this axis needs many SERIES x ANCHORS, not width
//
// exp_histogram_window_sample_bound.go's own S x W x (S + W) guard already
// covers a single group's own cost (handler_chdb_exp_histogram_window_
// sample_bound_test.go pins it via bucket WIDTH). This bound covers a
// SEPARATE axis: how many (series, anchor) groups the collapse hands to
// that per-group guard in one statement — a query whose EVERY group
// individually passes the per-group guard can still exceed real
// ClickHouse's memory once the group count itself grows (cerberus issue
// #3468's own live reproduction: the k3d/compose self-observability
// dashboard's P95-by-language panel). So these seeds vary series count and
// anchor count, holding each group's own width/sample-count trivially
// small — the opposite parameterisation from the sibling file.
package prom_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
)

// rangeBucketFanoutGroupBoundWindow is the eval window every case here
// uses: anchorCount one-minute samples per series, all reachable through a
// query_range grid at step=60s covering the same span.
func rangeBucketFanoutGroupBoundWindow(anchorCount int) (start, end time.Time) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(time.Duration(anchorCount) * time.Minute)
}

// rangeBucketFanoutGroupBoundSeed writes seriesCount distinct exp-histogram
// series, each with anchorCount one-minute-apart samples (a trivially
// narrow, single-bucket payload — this bound is about GROUP COUNT, not
// per-group cost, see the file doc comment) starting at start. Each series
// carries its own `series` attribute value so `sum by (series) (...)`
// resolves one output group per series while the window fold beneath it
// still reduces through seriesCount x anchorCount (series, anchor) groups.
func rangeBucketFanoutGroupBoundSeed(t *testing.T, start time.Time, seriesCount, anchorCount int) string {
	t.Helper()
	rows := make([]string, 0, seriesCount*anchorCount)
	for s := range seriesCount {
		for i := range anchorCount {
			ts := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05.000000000")
			rows = append(rows, fmt.Sprintf(
				"    ('many_groups_exp_hist', '', '', map('series', 's%d'), toDateTime64('%s', 9), %d, %d.0, 0, 0, 0, "+
					"[toUInt64(1)], 0, [])",
				s, ts, i+1, i+1,
			))
		}
	}
	return metaShapedMetricsDDL +
		"\nINSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, " +
		"PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		strings.Join(rows, ",\n") + ";"
}

const rangeBucketFanoutGroupBoundQuery = "histogram_quantile(0.95, sum by (series) (rate(many_groups_exp_hist[5m])))"

// TestQueryRange_RangeBucketFanoutGroupBound_RejectedAt422_ChDB is the
// rejection: 30 series x 30 anchors = 900 (series, anchor) groups, over
// maxRangeBucketFanoutGroupRows' own default of 800.
func TestQueryRange_RangeBucketFanoutGroupBound_RejectedAt422_ChDB(t *testing.T) {
	const seriesCount, anchorCount = 30, 30
	start, end := rangeBucketFanoutGroupBoundWindow(anchorCount)
	srv, _ := newChDBServer(t, rangeBucketFanoutGroupBoundSeed(t, start, seriesCount, anchorCount))

	status, body := getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60",
		srv.URL, url.QueryEscape(rangeBucketFanoutGroupBoundQuery), start.Unix(), end.Unix(),
	))
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want %d; body=%s", status, http.StatusUnprocessableEntity, body)
	}
	if !strings.Contains(body, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("body does not name the bound: %s", body)
	}
	if strings.Contains(body, "DB::Exception") {
		t.Errorf("body leaks the ClickHouse exception: %s", body)
	}
}

// TestQueryRange_RangeBucketFanoutGroupBound_OrdinaryGroupCountAnswers_ChDB
// is the discriminating control, and the reason the seed is parameterised
// on series/anchor count alone: SAME per-group payload shape, SAME query,
// 10 series x 30 anchors = 300 groups — comfortably under the 800
// default. Without it the rejection test above would pass just as well
// against a guard that refused every RangeBucketFanout-over-exp-histogram
// query outright.
func TestQueryRange_RangeBucketFanoutGroupBound_OrdinaryGroupCountAnswers_ChDB(t *testing.T) {
	const seriesCount, anchorCount = 10, 30
	start, end := rangeBucketFanoutGroupBoundWindow(anchorCount)
	srv, _ := newChDBServer(t, rangeBucketFanoutGroupBoundSeed(t, start, seriesCount, anchorCount))

	status, body := getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60",
		srv.URL, url.QueryEscape(rangeBucketFanoutGroupBoundQuery), start.Unix(), end.Unix(),
	))
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — %d groups is comfortably under the 800 default ceiling "+
			"and this query must answer; body=%s", status, seriesCount*anchorCount, body)
	}
	if strings.Contains(body, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("answered 200 but still names the bound: %s", body)
	}
}
