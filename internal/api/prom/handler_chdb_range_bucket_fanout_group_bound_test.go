//go:build chdb

// chDB-backed wire-level pins for cerberus issue #3468's RangeBucketFanout
// collapse-output fold-cost bound: the operator gets a 422 naming the
// axis, not a 502 relaying ClickHouse's own MEMORY_LIMIT_EXCEEDED (the
// #2522 failure mode every case in this switch guards against — see
// handler.go's own RangeBucketFanoutGroupBudgetMessage case comment), and
// not a wrong answer.
//
// # What this axis is, and what issue #3514 changed about it
//
// exp_histogram_window_sample_bound.go's own S x W x (S + W) guard already
// covers a single group's own cost (handler_chdb_exp_histogram_window_
// sample_bound_test.go pins it via bucket WIDTH). This bound covers a
// SEPARATE axis: what ALL the (series, anchor) groups the collapse hands
// to that per-group guard cost TOGETHER in one statement — a query whose
// every group individually passes the per-group guard can still exceed
// real ClickHouse's memory once there are enough of them (cerberus issue
// #3468's own live reproduction: the k3d/compose self-observability
// dashboard's P95-by-language panel).
//
// #3468 first spent that budget as a flat GROUP COUNT, and #3514 is what
// that cost: ordinary compat-corpus queries at ~1,100 groups were rejected
// while peaking under 8% of the 1 GiB cap, because a group's real cost
// moves by more than an order of magnitude with the bucket-ladder width a
// group count cannot see. The budget is now the payload those groups
// actually accumulated (maxRangeBucketFanoutFoldCostUnits), so the three
// cases below are parameterised on ladder WIDTH and group count
// independently: the same group count at two widths must land on opposite
// sides of the bound, and a group count well past #3468's own 800-group
// ceiling must still answer at a narrow width.
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
// series, each with anchorCount one-minute-apart samples starting at start
// and a bucketWidth-wide positive ladder. Each series carries its own
// `series` attribute value so `sum by (series) (...)` resolves one output
// group per series while the window fold beneath it still reduces through
// seriesCount x anchorCount (series, anchor) groups.
//
// bucketWidth is the axis #3514 added: it moves what one group COSTS while
// leaving how many groups there are untouched, which is exactly the
// separation the replaced group-count ceiling could not express. The ladder
// is written as a `range()` expression rather than a literal array so a
// 300-wide seed stays a readable statement.
func rangeBucketFanoutGroupBoundSeed(t *testing.T, start time.Time, seriesCount, anchorCount, bucketWidth int) string {
	t.Helper()
	rows := make([]string, 0, seriesCount*anchorCount)
	for s := range seriesCount {
		for i := range anchorCount {
			ts := start.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05.000000000")
			rows = append(rows, fmt.Sprintf(
				"    ('many_groups_exp_hist', '', '', map('series', 's%d'), toDateTime64('%s', 9), %d, %d.0, 0, 0, 0, "+
					"arrayMap(b -> toUInt64(%d), range(%d)), 0, [])",
				s, ts, (i+1)*bucketWidth, i+1, i+1, bucketWidth,
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

// rangeBucketFanoutGroupBoundRun seeds, queries and returns the wire
// status + body for one (seriesCount, anchorCount, bucketWidth) point.
func rangeBucketFanoutGroupBoundRun(t *testing.T, seriesCount, anchorCount, bucketWidth int) (int, string) {
	t.Helper()
	start, end := rangeBucketFanoutGroupBoundWindow(anchorCount)
	srv, _ := newChDBServer(t, rangeBucketFanoutGroupBoundSeed(t, start, seriesCount, anchorCount, bucketWidth))
	return getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60",
		srv.URL, url.QueryEscape(rangeBucketFanoutGroupBoundQuery), start.Unix(), end.Unix(),
	))
}

// rangeBucketFanoutGroupBoundSeries / …Anchors are the group count the
// rejection case and its same-count control share, so the ONLY thing that
// differs between them is the ladder width. …WideAnchors is the larger
// count the narrow-payload case uses to reach past issue #3468's own
// 800-group ceiling.
const (
	rangeBucketFanoutGroupBoundSeries      = 30
	rangeBucketFanoutGroupBoundAnchors     = 20
	rangeBucketFanoutGroupBoundWideAnchors = 40
)

// rangeBucketFanoutGroupBoundWideLadder / …NarrowLadder are the two stored
// positive-bucket widths. The wide one is drawn from
// maxRangeBucketFanoutFoldCostUnits' own calibration range (real
// exponential histograms at fine scales store ladders of this order); the
// narrow one is the single-bucket payload the replaced group-count bound
// charged exactly the same for.
const (
	rangeBucketFanoutGroupBoundWideLadder   = 300
	rangeBucketFanoutGroupBoundNarrowLadder = 1
)

// TestQueryRange_RangeBucketFanoutFoldCostBound_RejectedAt422_ChDB is the
// rejection: 30 series x 20 anchors of 300-wide ladders, whose summed fold
// cost is several times maxRangeBucketFanoutFoldCostUnits. Every group
// individually clears exp_histogram_window_sample_bound.go's own per-group
// ceiling, so this axis is the only one that can refuse it.
func TestQueryRange_RangeBucketFanoutFoldCostBound_RejectedAt422_ChDB(t *testing.T) {
	status, body := rangeBucketFanoutGroupBoundRun(t,
		rangeBucketFanoutGroupBoundSeries, rangeBucketFanoutGroupBoundAnchors, rangeBucketFanoutGroupBoundWideLadder)
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

// TestQueryRange_RangeBucketFanoutFoldCostBound_SameGroupCountNarrowPayloadAnswers_ChDB
// is the discriminating control for the bound's currency: the IDENTICAL
// series x anchor count as the rejection above, the same query, the same
// grid — only the stored ladder is one bucket wide instead of 300. It must
// answer. A guard that still counted groups would refuse this one too, and
// the rejection above would then prove nothing about width.
func TestQueryRange_RangeBucketFanoutFoldCostBound_SameGroupCountNarrowPayloadAnswers_ChDB(t *testing.T) {
	status, body := rangeBucketFanoutGroupBoundRun(t,
		rangeBucketFanoutGroupBoundSeries, rangeBucketFanoutGroupBoundAnchors, rangeBucketFanoutGroupBoundNarrowLadder)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — the same group count at a one-bucket ladder costs the fold "+
			"orders of magnitude less and must answer; body=%s", status, body)
	}
	if strings.Contains(body, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("answered 200 but still names the bound: %s", body)
	}
}

// TestQueryRange_RangeBucketFanoutFoldCostBound_ManyCheapGroupsAnswer_ChDB
// is issue #3514's own regression pin: 30 series x 40 anchors is about
// 1,200 (series, anchor) groups — past the 800-group ceiling #3468 shipped
// and squarely in the range the 26 regressed compat-prometheus cases sat
// in — at a payload the fold folds for almost nothing. It must answer.
func TestQueryRange_RangeBucketFanoutFoldCostBound_ManyCheapGroupsAnswer_ChDB(t *testing.T) {
	status, body := rangeBucketFanoutGroupBoundRun(t,
		rangeBucketFanoutGroupBoundSeries, rangeBucketFanoutGroupBoundWideAnchors, rangeBucketFanoutGroupBoundNarrowLadder)
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want 200 — ~%d cheap groups is what issue #3514 was filed for; body=%s",
			status, rangeBucketFanoutGroupBoundSeries*rangeBucketFanoutGroupBoundWideAnchors, body)
	}
	if strings.Contains(body, chsql.RangeBucketFanoutGroupBudgetMessage) {
		t.Errorf("answered 200 but still names the bound: %s", body)
	}
}
