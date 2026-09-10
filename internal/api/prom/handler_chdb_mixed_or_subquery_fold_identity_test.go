//go:build chdb

// chDB-backed wire-level pins for the mixed float/histogram `or` subquery
// FOLD family's cross-branch collision (cerberus issue #3253's second
// part, cerberus issue #3271's authority decision, and cerberus issue
// #3262's own reverted commit — see combineMixedFoldBranches's doc in
// internal/promql/histogram_native_mixed_or_aggregate.go for the
// mechanism).
//
// A mixed float/histogram `or` subquery fold splits the relation on its
// discriminator, folds each half separately and recombines. A hist/float
// collision on that recombination is CROSS-BRANCH: the histogram arm is
// the only name in the histogram branch and the float arm is the only
// name in the float branch, so each branch's OWN distinct-`__name__`
// count is 1 and neither branch-local guard can fire — the collision is
// visible only at the recombination, where one match key carries a row
// from each side. Before combineMixedFoldBranches learned to reject that
// key, these queries answered 200 with a single histogram-valued series
// and the float arm's own fold missing from the answer entirely; upstream
// (with delayed name removal at its real, off default — cerberus issue
// #3271) raises "vector cannot contain metrics with the same labelset"
// instead, and this file pins cerberus doing the same.
//
// This intentionally does NOT cover the pure-histogram (same-family)
// collision or the name-preserving last_over_time/first_over_time case —
// those are cerberus issue #3253's first part and belong with its own
// lowering fix.
package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// mixedOrFoldWindow is the eval window every test in this file uses.
func mixedOrFoldWindow() (start, end time.Time, step time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute), time.Minute
}

// mixedOrFoldSubqueryRange is the subquery bracket every query below uses:
// `[5m:1m]` at an eval time of start+5m grids anchors at minutes 1..5
// (left-open, epoch-aligned).
const mixedOrFoldSubqueryRange = "[5m:1m]"

// mixedOrFoldNames are the seven FOLD names, all of which drop `__name__`.
// Listing all seven is what proves the guard rides on the reduction the
// whole family shares rather than on one function's own projection.
var mixedOrFoldNames = []string{
	"rate", "increase", "delta", "irate", "idelta",
	"sum_over_time", "avg_over_time",
}

// mixedOrFoldSeed writes a histogram metric and a gauge metric under the
// caller-supplied attribute maps, sampled so the `or` shadow is PARTIAL and
// BOTH arms end up with at least two points on the subquery grid — which is
// what the counter folds need before they publish a series at all.
//
//	subquery anchor:    1     2     3     4     5
//	latency_float:     0:30  1:30    -     -     -
//	latency_exp_hist:    -     -     -   3:30  4:30
//
// Passing the SAME map for both is the collision. Passing disjoint maps is
// the control: two signatures, no shadow, and two series upstream answers
// happily.
func mixedOrFoldSeed(t *testing.T, start time.Time, floatAttrs, histAttrs string) string {
	t.Helper()
	at := func(d time.Duration) string {
		return start.Add(d).Format("2006-01-02 15:04:05.000000000")
	}
	return metaShapedMetricsDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('latency_float', '', '', %[5]s, toDateTime64('%[1]s', 9), 1.0),
    ('latency_float', '', '', %[5]s, toDateTime64('%[2]s', 9), 2.0);
INSERT INTO otel_metrics_exponential_histogram
    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES
    ('latency_exp_hist', '', '', %[6]s, toDateTime64('%[3]s', 9), 10, 20.0, 0, 0, 0, [1, 2, 3, 4], 0, []),
    ('latency_exp_hist', '', '', %[6]s, toDateTime64('%[4]s', 9), 12, 24.0, 0, 0, 0, [1, 2, 3, 6], 0, []);`,
		at(30*time.Second), at(time.Minute+30*time.Second),
		at(3*time.Minute+30*time.Second), at(4*time.Minute+30*time.Second),
		floatAttrs, histAttrs,
	)
}

// mixedOrFoldQuery brackets one name over the mixed float/histogram `or`.
// The histogram is the LHS, as in the issue's own repro; upstream's answer
// does not depend on which arm leads (measured against the reference
// engine both ways — cerberus issue #3271).
func mixedOrFoldQuery(name string) string {
	return fmt.Sprintf("%s((latency_exp_hist or latency_float)%s)", name, mixedOrFoldSubqueryRange)
}

// TestQuery_MixedOrSubqueryFoldFamily_DuplicateLabelset_ChDB is the
// wrong-answer regression, instant shape: every one of the seven
// name-dropping FOLD names must refuse this query with upstream's own
// words, where each used to answer 200 with a single histogram-valued
// series.
func TestQuery_MixedOrSubqueryFoldFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := mixedOrFoldWindow()
	srv, _ := newChDBServer(t, mixedOrFoldSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, name := range mixedOrFoldNames {
		query := mixedOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQueryRange_MixedOrSubqueryFoldFamily_DuplicateLabelset_ChDB is the
// same seven over /api/v1/query_range. The recombination is the same node
// in both modes, but the two branches reduce through different ones, so
// running both is what proves the rejection rides on the union rather than
// on either branch's own reduction.
func TestQueryRange_MixedOrSubqueryFoldFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, step := mixedOrFoldWindow()
	srv, _ := newChDBServer(t, mixedOrFoldSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, name := range mixedOrFoldNames {
		query := mixedOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf(
				"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
				srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
			))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQuery_MixedOrSubqueryFoldFamily_DistinctLabelsets_ChDB is the
// discriminating control: the recombination's match key is (Attributes,
// timestamp), so a guard that fired on "both branches produced a row"
// rather than "both produced a row on ONE key" would reject here too.
// Disjoint attributes must still answer two series, one per arm.
func TestQuery_MixedOrSubqueryFoldFamily_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := mixedOrFoldWindow()
	srv, _ := newChDBServer(t, mixedOrFoldSeed(t,
		start, "map('x', '1', 'pod', 'f1')", "map('x', '1', 'pod', 'h1')"))

	for _, name := range mixedOrFoldNames {
		query := mixedOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			vec := mixedOrFoldVector(t, srv.URL, query, end)
			if len(vec) != 2 {
				t.Fatalf("%s: got %d series, want 2 (pod=f1 and pod=h1 are distinct label sets, "+
					"so nothing collides and nothing may abort): %+v", query, len(vec), vec)
			}
			seen := map[string]bool{}
			for _, s := range vec {
				seen[s.Metric["pod"]] = true
			}
			for _, want := range []string{"f1", "h1"} {
				if !seen[want] {
					t.Errorf("%s: expected a series with pod=%q, got %v", query, want, seen)
				}
			}
		})
	}
}

// mixedOrFoldSeries is one entry of an instant-query vector result, read
// back by label set alone — the values are the fold's own arithmetic,
// which the promql-package chDB tests pin; what this file asks is which
// SERIES exist.
type mixedOrFoldSeries struct {
	Metric map[string]string `json:"metric"`
}

// mixedOrFoldVector issues one instant query and decodes its vector,
// failing on any non-200.
func mixedOrFoldVector(t *testing.T, baseURL, query string, at time.Time) []mixedOrFoldSeries {
	t.Helper()
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		baseURL, url.QueryEscape(query), at.Unix()))
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: unmarshal: %v; body=%s", query, err, body)
	}
	rawResult, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("%s: remarshal: %v", query, err)
	}
	var vec []mixedOrFoldSeries
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("%s: decode vector: %v; body=%s", query, err, body)
	}
	return vec
}
