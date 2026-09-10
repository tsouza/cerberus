//go:build chdb

// chDB-backed wire-level pins for series identity under the DOUBLY-nested
// subquery composition `<fn>(<inner-sub>)[<outer-range>:<step>]`
// (cerberus issue #3240) — the doubly-nested sibling of the singly-nested
// family handler_chdb_mixed_or_subquery_duplicate_labelset_test.go pins.
//
// Every continuation of that composition reduced on the stored attributes
// column ALONE (internal/promql's buildOuterRangeSubqueryFanout), so two
// series that differ only in `__name__` — exactly what an `or` produces,
// since upstream matches its arms on a signature that DROPS `__name__` —
// landed in one group. What that cost depends on whether the outer
// function keeps the name:
//
//   - last_over_time / first_over_time KEEP it. Reference folds per series
//     and keys its output on Metric.Hash(), so it answers TWO series.
//     Cerberus published one, under whichever name its argMax/argMin pick
//     happened to select, and dropped the other series outright.
//   - the name-dropping names DROP it. Reference's two output series then
//     carry one identical label set and `Matrix.ContainsSameLabelset()`
//     refuses the query. Cerberus answered one merged series carrying a
//     value neither input has.
//
// So the fix is not one rule: the name-preserving pair gains `__name__` in
// its group key, and the name-dropping names keep the attributes-only key
// and gain the collision guard that key is what makes visible. Both halves
// are pinned here.
//
// # Why every query is a hist/float `or`
//
// Two constraints meet. The inner subquery's own expression has to be a
// set-op: per internal/promql/histogram_native_subquery_call_subquery_chdb_test.go's
// header, a bare-selector or `rate(...)` inner is intercepted by the
// singly-nested continuations before this composition is reached. And the
// set-op has to be `or` over a MIXED pair, because — measured with a
// lowering probe over all fifteen names — a HistogramRowShape input never
// reaches this composition either: an `and`/`or` of two exp-histogram arms
// is likewise intercepted, which is why the dispatcher's own
// `shape == HistogramRowShape` arms are unreached today
// (tsouza/cerberus#3253). `and` is out for a third reason: it publishes
// its left arm's `__name__` alone, so there is no collision to see.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// callSubqIdentityWindow is the eval window every test in this file uses.
func callSubqIdentityWindow() (start, end time.Time) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(8 * time.Minute)
}

// callSubqMixedSeed writes one exponential-histogram metric and one gauge
// metric — the MixedRowShape input. The float is sampled every minute from
// the window's start; the histogram only from the fourth minute on, so the
// `or` shadow is PARTIAL: the earliest inner-subquery anchors see only the
// float and the rest see the histogram. Both names therefore reach one
// outer-anchor window, which is the collision.
//
// Passing the SAME attribute map for both is that collision. Passing
// disjoint maps is the control.
func callSubqMixedSeed(t *testing.T, start time.Time, histAttrs, floatAttrs string) string {
	t.Helper()
	return metaShapedMetricsDDL +
		"\nINSERT INTO otel_metrics_exponential_histogram\n" +
		"    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		callSubqExpHistRows(t, "latency_exp_hist", histAttrs, start, 4, 8) +
		"\nINSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES\n" +
		callSubqGaugeRows(t, "latency_float", floatAttrs, start, 0, 8)
}

// callSubqExpHistRows renders one exp-histogram row per minute in
// [fromMin, toMin], with a rising Count/Sum so a fold has something
// non-degenerate to fold.
func callSubqExpHistRows(t *testing.T, metric, attrs string, start time.Time, fromMin, toMin int) string {
	t.Helper()
	rows := make([]string, 0, toMin-fromMin+1)
	for i := fromMin; i <= toMin; i++ {
		rows = append(rows, fmt.Sprintf(
			"    ('%s', '', '', %s, toDateTime64('%s', 9), %d, %d.0, 0, 0, 0, [1, 2, 3, 4], 0, [])",
			metric, attrs, callSubqStamp(start, i), 10*(i+1), 20*(i+1),
		))
	}
	return callSubqValues(rows)
}

// callSubqGaugeRows is [callSubqExpHistRows] for the gauge table.
func callSubqGaugeRows(t *testing.T, metric, attrs string, start time.Time, fromMin, toMin int) string {
	t.Helper()
	rows := make([]string, 0, toMin-fromMin+1)
	for i := fromMin; i <= toMin; i++ {
		rows = append(rows, fmt.Sprintf(
			"    ('%s', '', '', %s, toDateTime64('%s', 9), %d.0)",
			metric, attrs, callSubqStamp(start, i), i+1,
		))
	}
	return callSubqValues(rows)
}

// callSubqStamp renders the minute-th sample instant.
func callSubqStamp(start time.Time, minute int) string {
	return start.Add(time.Duration(minute) * time.Minute).Format("2006-01-02 15:04:05.000000000")
}

// callSubqValues terminates a VALUES list.
func callSubqValues(rows []string) string {
	out := ""
	for i, r := range rows {
		out += r
		if i < len(rows)-1 {
			out += ",\n"
		}
	}
	return out + ";\n"
}

// callSubqNameDroppingNames are the six names whose output carries no
// `__name__` AND whose collision this composition's group key can see.
// They do not share one continuation — count_over_time / present_over_time
// / ts_of_* route through lowerSelectFnOverCallSubqueryInput and resets /
// changes through lowerMixedResetsOrChangesOverCallSubqueryInput — so
// running all six is what proves the guard rides on the shared fanout
// rather than on one continuation.
//
// The seven FOLD names (rate / increase / delta / irate / idelta /
// sum_over_time / avg_over_time) are deliberately absent, and not because
// they are awkward: they reach the same fanout, but their collision is
// CROSS-branch. lowerMixedFoldOverCallSubqueryInput splits the relation by
// type first, so each side of splitMixedRelByDiscriminator sees exactly
// one `__name__` and a guard on either branch's own reduction cannot see
// the pair. Measured: `rate` answers 200 with a single histogram-valued
// series where reference raises. Closing that needs a duplicate check that
// survives the split, which is a different mechanism from this issue's
// group key — tsouza/cerberus#3253.
var callSubqNameDroppingNames = []string{
	"count_over_time", "present_over_time",
	"ts_of_first_over_time", "ts_of_last_over_time",
	"resets", "changes",
}

// callSubqNamePreservingNames are the two that keep `__name__`.
var callSubqNamePreservingNames = []string{"last_over_time", "first_over_time"}

// callSubqQuery builds the doubly-nested composition over the two arms.
func callSubqQuery(fn string) string {
	return fmt.Sprintf("%s(((latency_exp_hist) or (latency_float))[3m:1m])[4m:1m]", fn)
}

// TestQuery_CallSubqueryNameDropping_DuplicateLabelset_ChDB is the
// wrong-answer regression for the name-dropping half. Each of the six used
// to answer one merged series; each must now refuse the query with
// upstream's own words.
func TestQuery_CallSubqueryNameDropping_DuplicateLabelset_ChDB(t *testing.T) {
	start, end := callSubqIdentityWindow()
	srv, _ := newChDBServer(t, callSubqMixedSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, fn := range callSubqNameDroppingNames {
		query := callSubqQuery(fn)
		t.Run(fn, func(t *testing.T) {
			status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(query), end.Unix()))
			assertDuplicateLabelsetRejected(t, body, status, query)
		})
	}
}

// TestQuery_CallSubqueryNamePreserving_KeepsBothSeries_ChDB is the other
// half of the fix, and the half a guard alone would get WRONG.
//
// last_over_time and first_over_time publish the selected sample's own
// `__name__`, so reference answers TWO series here rather than raising.
// Under the attributes-only fanout key cerberus answered ONE, carrying
// whichever name the directional pick selected — a series silently
// deleted, not an error. Refusing the query instead of widening the key
// would be just as wrong, in the opposite direction.
func TestQuery_CallSubqueryNamePreserving_KeepsBothSeries_ChDB(t *testing.T) {
	start, end := callSubqIdentityWindow()
	srv, _ := newChDBServer(t, callSubqMixedSeed(t, start, "map('x', '1')", "map('x', '1')"))

	for _, fn := range callSubqNamePreservingNames {
		query := callSubqQuery(fn)
		t.Run(fn, func(t *testing.T) {
			names := callSubqSeriesNames(t, srv.URL, query, end)
			want := map[string]bool{"latency_exp_hist": true, "latency_float": true}
			if len(names) != len(want) {
				t.Fatalf("%s: got %d series %v, want one per __name__ %v — an "+
					"attributes-only reduction key merges them and publishes whichever "+
					"name the argMax/argMin pick selected", query, len(names), names, want)
			}
			for n := range want {
				if !names[n] {
					t.Errorf("%s: no series carrying __name__=%q; got %v", query, n, names)
				}
			}
		})
	}
}

// TestQuery_CallSubqueryNameDropping_DistinctLabelsets_ChDB is the
// discriminating control: the same six names over arms whose ATTRIBUTES
// differ must still answer, and must answer two series.
//
// A guard keyed on "this reduction can see more than one metric name"
// rather than on "two names landed in ONE group" would refuse this query
// too, and a fix that simply widened the key for everybody would answer it
// while leaving the genuine collision unreported.
func TestQuery_CallSubqueryNameDropping_DistinctLabelsets_ChDB(t *testing.T) {
	start, end := callSubqIdentityWindow()
	srv, _ := newChDBServer(t, callSubqMixedSeed(t,
		start, "map('x', '1', 'pod', 'h1')", "map('x', '1', 'pod', 'f1')"))

	for _, fn := range callSubqNameDroppingNames {
		query := callSubqQuery(fn)
		t.Run(fn, func(t *testing.T) {
			series := callSubqSeries(t, srv.URL, query, end)
			if len(series) != 2 {
				t.Fatalf("%s: got %d series, want 2 (pod=h1 and pod=f1 are distinct label "+
					"sets, so nothing collides and nothing may abort): %+v", query, len(series), series)
			}
			for _, m := range series {
				if name, ok := m["__name__"]; ok {
					t.Errorf("%s: __name__ %q survived a name-dropping function: %+v", query, name, m)
				}
			}
		})
	}
}

// callSubqSeries issues one instant query, requires a 200, and returns the
// label set of each matrix series. The doubly-nested composition is a
// SubqueryExpr at the plan root, so its result type is a matrix even from
// /api/v1/query.
func callSubqSeries(t *testing.T, srvURL, query string, at time.Time) []map[string]string {
	t.Helper()
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srvURL, url.QueryEscape(query), at.Unix()))
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: unmarshal: %v; body=%s", query, err, body)
	}
	rawResult, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", query, err)
	}
	var matrix []struct {
		Metric map[string]string `json:"metric"`
	}
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("%s: decode matrix: %v; body=%s", query, err, body)
	}
	out := make([]map[string]string, 0, len(matrix))
	for _, m := range matrix {
		out = append(out, m.Metric)
	}
	return out
}

// callSubqSeriesNames reduces [callSubqSeries] to the set of `__name__`
// values it carries, which is the whole of what the name-preserving
// assertion is about.
func callSubqSeriesNames(t *testing.T, srvURL, query string, at time.Time) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range callSubqSeries(t, srvURL, query, at) {
		out[m["__name__"]] = true
	}
	return out
}
