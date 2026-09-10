//go:build chdb

// chDB-backed wire-level pins for the series identity the exp-histogram
// FOLD family and last_over_time / first_over_time owe over an `or`
// subquery (cerberus issue #3253) — the singly-nested sibling of
// handler_chdb_mixed_or_subquery_duplicate_labelset_test.go's SELECT-family
// pins (#3232).
//
// The collision arrives the same way it does there: `or` matches on a
// signature that DROPS `__name__`, so two exp-histogram metrics carrying
// byte-identical attributes share one signature. Where the shadow is
// PARTIAL — one arm covers only part of the subquery grid — both series
// reach the subquery's Matrix under their own `__name__`, and what happens
// next depends on whether the outer function keeps that name:
//
//   - The seven FOLD names (rate, increase, delta, irate, idelta,
//     sum_over_time, avg_over_time) drop it, so the two folds land on one
//     label set and `Matrix.ContainsSameLabelset()` refuses the query.
//     Cerberus reduced on Attributes alone and answered ONE series.
//
// Both arms here are exponential histograms deliberately. The cross-TYPE
// (histogram/float) sibling of this collision is NOT covered here: the two
// reference surfaces cerberus grades against disagree about it —
// test/spec's parity oracle runs promqltest.NewTestEngine, which forces
// `EnableDelayedNameRemoval`, while compatibility/prometheus runs the real
// server, which defaults it off — and the answer flips between them. A
// pure-histogram collision raises under BOTH, which is why this file is
// the half that can be pinned today.
//   - last_over_time / first_over_time keep it, so the answer is TWO
//     series. Cerberus reduced on Attributes alone and then argMax'd a
//     single `__name__` out of the merged group, publishing one of the
//     pair and DELETING the other.
//
// Every assertion is at the wire, which is where the difference between
// "Grafana shows an error" and "Grafana shows a wrong number" lives.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// histOrFoldWindow is the eval window every test in this file uses.
func histOrFoldWindow() (start, end time.Time, step time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute), time.Minute
}

// histOrFoldSubqueryRange is the subquery bracket every query below uses:
// `[5m:1m]` at an eval time of start+5m grids anchors at minutes 1..5
// (left-open, epoch-aligned).
const histOrFoldSubqueryRange = "[5m:1m]"

// histOrFoldSeed writes TWO exponential-histogram metrics under the
// caller-supplied attribute maps, sampled so that the `or` shadow is
// PARTIAL and BOTH arms end up with at least two points on the subquery
// grid — which is what the counter folds need before they publish a
// series at all.
//
//	subquery anchor:      1     2     3     4     5
//	other_exp_hist:      0:30  1:30  1:30    -     -
//	latency_exp_hist:      -     -     -   3:30  4:30
//
// The shadowed arm therefore owns anchors 1-3 and the shadowing arm owns
// 4-5, so the Matrix holds two series of three and two points. A one-point
// arm would be dropped by rate/increase/delta before any collision could
// happen, which would make the abort untestable for five of the seven
// names.
//
// Passing the SAME map for both is the collision. Passing disjoint maps is
// the control: two signatures, no shadow, and two series upstream answers
// happily.
func histOrFoldSeed(t *testing.T, start time.Time, shadowedAttrs, shadowingAttrs string) string {
	t.Helper()
	at := func(d time.Duration) string {
		return start.Add(d).Format("2006-01-02 15:04:05.000000000")
	}
	return metaShapedMetricsDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_exponential_histogram
    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES
    ('other_exp_hist',   '', '', %[5]s, toDateTime64('%[1]s', 9),  4,  8.0, 0, 0, 0, [1, 1, 1, 1], 0, []),
    ('other_exp_hist',   '', '', %[5]s, toDateTime64('%[2]s', 9),  6, 12.0, 0, 0, 0, [2, 2, 1, 1], 0, []),
    ('latency_exp_hist', '', '', %[6]s, toDateTime64('%[3]s', 9), 10, 20.0, 0, 0, 0, [1, 2, 3, 4], 0, []),
    ('latency_exp_hist', '', '', %[6]s, toDateTime64('%[4]s', 9), 12, 24.0, 0, 0, 0, [1, 2, 3, 6], 0, []);`,
		at(30*time.Second), at(time.Minute+30*time.Second),
		at(3*time.Minute+30*time.Second), at(4*time.Minute+30*time.Second),
		shadowedAttrs, shadowingAttrs,
	)
}

// histOrFoldNames are the seven FOLD names, all of which drop `__name__`.
// Listing all seven is what proves the guard rides on the reduction the
// whole family shares rather than on one function's own projection: the
// three window selections [histogramWindowAllSamples],
// [histogramWindowEndpoints] and [histogramWindowLastTwo] are all
// represented.
var histOrFoldNames = []string{
	"rate", "increase", "delta", "irate", "idelta",
	"sum_over_time", "avg_over_time",
}

// histOrFoldPreservingNames are the two names whose output KEEPS
// `__name__`, and which therefore answer two series rather than aborting.
var histOrFoldPreservingNames = []string{"last_over_time", "first_over_time"}

// histOrFoldQuery brackets one name over the pure-histogram `or`.
func histOrFoldQuery(name string) string {
	return fmt.Sprintf("%s((latency_exp_hist or other_exp_hist)%s)", name, histOrFoldSubqueryRange)
}

// TestQuery_HistOrSubqueryFoldFamily_DuplicateLabelset_ChDB is the
// wrong-answer regression, instant shape: every one of the seven
// name-dropping FOLD names must refuse this query with upstream's own
// words, where each used to answer a single merged series.
func TestQuery_HistOrSubqueryFoldFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := histOrFoldWindow()
	srv, _ := newChDBServer(t, histOrFoldSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, name := range histOrFoldNames {
		query := histOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQueryRange_HistOrSubqueryFoldFamily_DuplicateLabelset_ChDB is the
// same seven over /api/v1/query_range, which reduces through
// chplan.RangeBucketFanout rather than the instant chplan.Aggregate — a
// different node with no Having slot, so the guard has to reach it as a
// Filter over the fan-out's own output. Running both modes is what proves
// the abort is not pinned to whichever one happens to be an Aggregate.
func TestQueryRange_HistOrSubqueryFoldFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, step := histOrFoldWindow()
	srv, _ := newChDBServer(t, histOrFoldSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, name := range histOrFoldNames {
		query := histOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf(
				"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
				srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
			))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQuery_HistOrSubqueryFoldFamily_DistinctLabelsets_ChDB is the
// discriminating control. The same seven queries over arms whose
// attributes DIFFER must still answer, two series apiece: two signatures
// means no shadow, so both series fold over their own points and nothing
// collides. A guard keyed on "this reduction can see more than one metric
// name" rather than on "two names landed in ONE group" would fail here.
func TestQuery_HistOrSubqueryFoldFamily_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := histOrFoldWindow()
	srv, _ := newChDBServer(t, histOrFoldSeed(t,
		start, "map('x', '1', 'pod', 'o1')", "map('x', '1', 'pod', 'l1')"))

	for _, name := range histOrFoldNames {
		query := histOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			vec := histOrFoldVector(t, srv.URL, query, end)
			if len(vec) != 2 {
				t.Fatalf("%s: got %d series, want 2 (pod=o1 and pod=l1 are distinct label sets, "+
					"so nothing collides and nothing may abort): %+v", query, len(vec), vec)
			}
			seen := map[string]bool{}
			for _, s := range vec {
				if gotName, ok := s.Metric["__name__"]; ok {
					t.Errorf("%s: __name__ %q survived a name-dropping function: %+v", query, gotName, s.Metric)
				}
				seen[s.Metric["pod"]] = true
			}
			for _, want := range []string{"o1", "l1"} {
				if !seen[want] {
					t.Errorf("%s: expected a series with pod=%q, got %v", query, want, seen)
				}
			}
		})
	}
}

// TestQuery_HistOrSubqueryLastFirst_TwoSeries_ChDB is the OTHER half of
// #3253's first part, and it is not an abort. last_over_time and
// first_over_time publish the selected sample's own `__name__`, so two
// colliding-attribute arms are two distinct OUTPUT series and upstream
// answers both. Cerberus reduced on Attributes alone and argMax'd one
// `__name__` out of the merged group, so it answered ONE series and the
// other was not merged into a wrong number — it was deleted.
//
// Asserting the NAMES, not just the count, is what makes this
// discriminating: a reduction that merged the pair and then happened to
// emit two rows for some unrelated reason would still fail.
func TestQuery_HistOrSubqueryLastFirst_TwoSeries_ChDB(t *testing.T) {
	start, end, _ := histOrFoldWindow()
	srv, _ := newChDBServer(t, histOrFoldSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, name := range histOrFoldPreservingNames {
		query := histOrFoldQuery(name)
		t.Run(query, func(t *testing.T) {
			vec := histOrFoldVector(t, srv.URL, query, end)
			if len(vec) != 2 {
				t.Fatalf("%s: got %d series, want 2 — a name-PRESERVING fold keys on (Attributes, __name__), "+
					"so the two arms stay two series: %+v", query, len(vec), vec)
			}
			seen := map[string]bool{}
			for _, s := range vec {
				seen[s.Metric["__name__"]] = true
				if s.Metric["x"] != "1" {
					t.Errorf("%s: series lost its attributes: %+v", query, s.Metric)
				}
			}
			for _, want := range []string{"latency_exp_hist", "other_exp_hist"} {
				if !seen[want] {
					t.Errorf("%s: expected a series named %q, got %v", query, want, seen)
				}
			}
		})
	}
}

// histOrFoldSeries is one entry of an instant-query vector result, read
// back by label set alone — the values are the fold's own arithmetic,
// which the promql-package chDB tests pin; what this file asks is which
// SERIES exist.
type histOrFoldSeries struct {
	Metric map[string]string `json:"metric"`
}

// histOrFoldVector issues one instant query and decodes its vector,
// failing on any non-200.
func histOrFoldVector(t *testing.T, baseURL, query string, at time.Time) []histOrFoldSeries {
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
	var vec []histOrFoldSeries
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("%s: decode vector: %v; body=%s", query, err, body)
	}
	return vec
}
