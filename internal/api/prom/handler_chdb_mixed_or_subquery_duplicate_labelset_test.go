//go:build chdb

// chDB-backed wire-level pins for the duplicate-labelset abort the
// name-dropping SELECT family owes over a MIXED float/histogram `or`
// subquery (#3232) — the mixed-`or` sibling of the multi-name-selector
// shapes handler_chdb_duplicate_labelset_test.go already pins.
//
// The collision arrives by a different route here. There is no regex
// `__name__` matcher and no multi-metric selector: the two series come
// from two DIFFERENT metrics, in two different tables, and it is `or`
// that puts them in one Matrix. Upstream's `or` matches on a signature
// that DROPS `__name__`, so a histogram series and a float series
// carrying byte-identical attributes share one signature — the histogram
// shadows the float at the anchors it covers and the float survives at
// the rest. Both series therefore reach the subquery's Matrix, the outer
// function drops `__name__` from each, and
// `Matrix.ContainsSameLabelset()` refuses the query.
//
// Cerberus reduced that relation with `GROUP BY Attributes` alone, which
// merged the two series before the fold ever ran and answered ONE sample
// carrying a value neither series has. The guard rides on the reduction
// (internal/promql's subqueryNameCollisionAgg / subqueryNameCollisionFilter)
// for the same reason the multi-name guard rides on the inner window: by
// the time a row exists the two series are already one.
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

// mixedOrDupWindow is the eval window every test in this file uses. The
// instant tests ask at `end`; the range tests sweep [start, end] by step.
func mixedOrDupWindow() (start, end time.Time, step time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute), time.Minute
}

// mixedOrDupSeed writes one exponential-histogram metric and one gauge
// metric under the caller-supplied attribute maps, sampled so that the
// `or` shadow is PARTIAL over the window: the float carries a sample from
// before the histogram's first one, so at the earliest subquery anchors
// only the float exists, and from then on the histogram shadows it.
//
// Passing the SAME map for both is the collision — two series that differ
// only by `__name__`. Passing disjoint maps is the control: two signatures,
// no shadow, and two output series that upstream answers happily.
func mixedOrDupSeed(t *testing.T, start time.Time, histAttrs, floatAttrs string) string {
	t.Helper()
	at := func(d time.Duration) string {
		return start.Add(d).Format("2006-01-02 15:04:05.000000000")
	}
	return metaShapedMetricsDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_exponential_histogram
    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES
    ('latency_exp_hist', '', '', %[5]s, toDateTime64('%[1]s', 9), 10, 20.0, 0, 0, 0, [1, 2, 3, 4], 0, []),
    ('latency_exp_hist', '', '', %[5]s, toDateTime64('%[2]s', 9), 12, 24.0, 0, 0, 0, [1, 2, 3, 4], 0, []);
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('latency_float', '', '', %[6]s, toDateTime64('%[3]s', 9), 1.0),
    ('latency_float', '', '', %[6]s, toDateTime64('%[4]s', 9), 2.0);`,
		at(3*time.Minute+30*time.Second), at(4*time.Minute+30*time.Second),
		at(2*time.Minute+30*time.Second), at(4*time.Minute+45*time.Second),
		histAttrs, floatAttrs,
	)
}

// mixedOrDupQueries are the six names whose output DROPS `__name__`, each
// applied to the same mixed `or` subquery. All six reduce through the same
// two continuations — lowerSelectFnOverExpHistogramSubqueryInput for the
// four type-blind ones, lowerMixedOrSubqueryResetsOrChangesInput for
// resets/changes over a Mixed relation — so listing them here is what
// proves the guard is on the reduction rather than on one function's own
// projection.
//
// `on(x)` narrows the shadow key to the label both arms share, which is
// what routes the query to the per-anchor-correct reduction rather than
// through the bare-`or` recognizer. The signature it produces is the same
// one the default key produces on this seed — both arms carry `x` and
// nothing else — so the answer upstream gives is identical either way.
var mixedOrDupQueries = []string{
	`count_over_time((latency_exp_hist or on(x) latency_float)[3m:1m])`,
	`present_over_time((latency_exp_hist or on(x) latency_float)[3m:1m])`,
	`ts_of_first_over_time((latency_exp_hist or on(x) latency_float)[3m:1m])`,
	`ts_of_last_over_time((latency_exp_hist or on(x) latency_float)[3m:1m])`,
	`resets((latency_exp_hist or on(x) latency_float)[3m:1m])`,
	`changes((latency_exp_hist or on(x) latency_float)[3m:1m])`,
}

// TestQuery_MixedOrSubquerySelectFamily_DuplicateLabelset_ChDB is the
// wrong-answer regression, instant shape. Every one of the six
// name-dropping names must refuse this query with upstream's own words,
// where each used to answer a single merged sample.
func TestQuery_MixedOrSubquerySelectFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := mixedOrDupWindow()
	srv, _ := newChDBServer(t, mixedOrDupSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, query := range mixedOrDupQueries {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQueryRange_MixedOrSubquerySelectFamily_DuplicateLabelset_ChDB is
// the same six over /api/v1/query_range, which reduces through a
// different node entirely — chplan.RangeBucketFanout, one `[3m]` window
// per step anchor, with no Having slot for a guard to ride in. Running
// the range shape is what proves the abort reaches that mode too rather
// than only the instant reduction that happens to be an Aggregate.
func TestQueryRange_MixedOrSubquerySelectFamily_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, step := mixedOrDupWindow()
	srv, _ := newChDBServer(t, mixedOrDupSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, query := range mixedOrDupQueries {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf(
				"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
				srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
			))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQuery_MixedOrSubquerySelectFamily_DistinctLabelsets_ChDB is the
// other half of the boundary, and the half that keeps the guard a match
// for upstream rather than merely stricter than it. The same six queries
// over arms whose attribute sets DIFFER must still answer, two nameless
// series apiece: two signatures means no shadow at all, so both series
// fold over their own full window and their outputs never collide.
//
// A guard keyed on "this reduction can see more than one metric name"
// rather than on "two names landed in ONE group" would fail here, which
// is what makes this the discriminating control rather than a
// restatement.
func TestQuery_MixedOrSubquerySelectFamily_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := mixedOrDupWindow()
	srv, _ := newChDBServer(t, mixedOrDupSeed(t,
		start, "map('x', '1', 'pod', 'h1')", "map('x', '1', 'pod', 'f1')"))

	for _, query := range mixedOrDupQueries {
		t.Run(query, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			if status != http.StatusOK {
				t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
			}
			var parsed queryResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v; body=%s", err, body)
			}
			rawResult, _ := json.Marshal(parsed.Data.Result)
			var vec []struct {
				Metric map[string]string `json:"metric"`
			}
			if err := json.Unmarshal(rawResult, &vec); err != nil {
				t.Fatalf("decode vector: %v; body=%s", err, body)
			}
			if len(vec) != 2 {
				t.Fatalf("%s: got %d series, want 2 (pod=h1 and pod=f1 are distinct label sets, "+
					"so nothing collides and nothing may abort): %+v", query, len(vec), vec)
			}
			seen := map[string]bool{}
			for _, s := range vec {
				if name, ok := s.Metric["__name__"]; ok {
					t.Errorf("%s: __name__ %q survived a name-dropping function: %+v", query, name, s.Metric)
				}
				seen[s.Metric["pod"]] = true
			}
			for _, want := range []string{"h1", "f1"} {
				if !seen[want] {
					t.Errorf("%s: expected a series with pod=%q, got %v", query, want, seen)
				}
			}
		})
	}
}

// TestQuery_MixedOrSubquerySelectFamily_DuplicateSeriesTags_ChDB is this
// guard's own differential against
// chopt.FeatureTSThrowDuplicateSeriesIf (cerberus issue #3038), the
// fourth call site of the shared abort builder
// (internal/promql's duplicateLabelsetAbortExpr) —
// handler_chdb_throw_duplicate_series_if_test.go covers the other three.
//
// With the feature on, the SAME collision must land on the SAME 422
// errorType=execution wire shape while the message names the actual
// colliding tag rather than carrying the static text. Without this the
// feature's branch of the builder would be reachable only in theory from
// this site: the collision TEST differs here (a distinct-name count, not
// a row count, because the relation holds one row per subquery anchor),
// so it is the one call site where the two branches are not trivially the
// same expression.
func TestQuery_MixedOrSubquerySelectFamily_DuplicateSeriesTags_ChDB(t *testing.T) {
	start, end, _ := mixedOrDupWindow()
	srv := newChDBServerWithThrowDuplicateSeriesIf(t,
		mixedOrDupSeed(t, start, "map('host', 'a')", "map('host', 'a')"), false)

	const query = `count_over_time((latency_exp_hist or on(host) latency_float)[3m:1m])`
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(query), end.Unix()))
	assertDuplicateSeriesTagsRejected(t, body, status, query)
}
