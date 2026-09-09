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
//   - the other thirteen names DROP it. Reference's two output series then
//     carry one identical label set and `Matrix.ContainsSameLabelset()`
//     refuses the query. Cerberus answered one merged series carrying a
//     value neither input has.
//
// So the fix is not one rule: the name-preserving pair gains
// `__name__` in its group key, and the name-dropping thirteen keep the
// attributes-only key and gain the collision guard that key is what makes
// visible. Both halves are pinned here, over both row shapes the
// composition admits, because the five continuations that share the fanout
// split across them.
//
// The queries all nest a set-op inner (`(a) or (b)`) inside the inner
// subquery bracket. That is not decoration: per
// internal/promql/histogram_native_subquery_call_subquery_chdb_test.go's
// own header, a bare-selector or `rate(...)` inner is intercepted by the
// singly-nested continuations before this composition is ever reached, so
// a set-op inner is what actually exercises the doubly-nested fanout.

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
func callSubqIdentityWindow() (start, end time.Time, step time.Duration) {
	start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.Add(8 * time.Minute), time.Minute
}

// callSubqMixedSeed writes one exponential-histogram metric and one gauge
// metric — the MixedRowShape input. The float is sampled every minute from
// the window's start; the histogram only from the fourth minute on, so the
// `or` shadow is PARTIAL: the earliest inner-subquery anchors see only the
// float and the rest see the histogram.
//
// Passing the SAME attribute map for both is the collision — two series
// that differ only by `__name__`. Passing disjoint maps is the control.
func callSubqMixedSeed(t *testing.T, start time.Time, histAttrs, floatAttrs string) string {
	t.Helper()
	return metaShapedMetricsDDL +
		"\nINSERT INTO otel_metrics_exponential_histogram\n" +
		"    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		expHistRowsEveryMinute(t, "latency_exp_hist", histAttrs, start, 4, 8) +
		"\nINSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES\n" +
		gaugeRowsEveryMinute(t, "latency_float", floatAttrs, start, 0, 8)
}

// callSubqHistSeed writes TWO exponential-histogram metrics — the
// HistogramRowShape input, which routes the same fifteen names through
// different continuations than the mixed one does. Same partial-shadow
// layout: the shadowed arm covers the whole window, the shadowing arm only
// its tail.
func callSubqHistSeed(t *testing.T, start time.Time, leadAttrs, trailAttrs string) string {
	t.Helper()
	return metaShapedMetricsDDL +
		"\nINSERT INTO otel_metrics_exponential_histogram\n" +
		"    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		expHistRowsEveryMinute(t, "latency_exp_hist", leadAttrs, start, 4, 8) +
		"\nINSERT INTO otel_metrics_exponential_histogram\n" +
		"    (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		expHistRowsEveryMinute(t, "other_exp_hist", trailAttrs, start, 0, 8)
}

// expHistRowsEveryMinute renders one exp-histogram row per minute in
// [fromMin, toMin], with a monotonically rising Count/Sum so a fold has
// something non-degenerate to fold.
func expHistRowsEveryMinute(t *testing.T, metric, attrs string, start time.Time, fromMin, toMin int) string {
	t.Helper()
	rows := make([]string, 0, toMin-fromMin+1)
	for i := fromMin; i <= toMin; i++ {
		rows = append(rows, fmt.Sprintf(
			"    ('%s', '', '', %s, toDateTime64('%s', 9), %d, %d.0, 0, 0, 0, [1, 2, 3, 4], 0, [])",
			metric, attrs, start.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05.000000000"),
			10*(i+1), 20*(i+1),
		))
	}
	return joinRows(rows)
}

// gaugeRowsEveryMinute is [expHistRowsEveryMinute] for the gauge table.
func gaugeRowsEveryMinute(t *testing.T, metric, attrs string, start time.Time, fromMin, toMin int) string {
	t.Helper()
	rows := make([]string, 0, toMin-fromMin+1)
	for i := fromMin; i <= toMin; i++ {
		rows = append(rows, fmt.Sprintf(
			"    ('%s', '', '', %s, toDateTime64('%s', 9), %d.0)",
			metric, attrs, start.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05.000000000"),
			i+1,
		))
	}
	return joinRows(rows)
}

// joinRows terminates a VALUES list.
func joinRows(rows []string) string {
	out := ""
	for i, r := range rows {
		out += r
		if i < len(rows)-1 {
			out += ",\n"
		}
	}
	return out + ";\n"
}

// callSubqNameDroppingNames are the thirteen names whose output carries no
// `__name__`, listed in full because they do NOT share one continuation:
// count_over_time / present_over_time / ts_of_* route through
// lowerSelectFnOverCallSubqueryInput, resets / changes through it or
// through lowerMixedResetsOrChangesOverCallSubqueryInput depending on the
// row shape, and the seven fold names through
// lowerExpHistogramFoldOverCallSubqueryInput or
// lowerMixedFoldOverCallSubqueryInput. Listing them is what proves the
// guard rides on the shared fanout rather than on one continuation.
var callSubqNameDroppingNames = []string{
	"count_over_time", "present_over_time",
	"ts_of_first_over_time", "ts_of_last_over_time",
	"resets", "changes",
	"rate", "increase", "delta", "irate", "idelta",
	"sum_over_time", "avg_over_time",
}

// callSubqNamePreservingNames are the two that keep `__name__`.
var callSubqNamePreservingNames = []string{"last_over_time", "first_over_time"}

// callSubqRowShape is one of the two `wideInner` row shapes the
// doubly-nested composition admits, seeded so its two arms COLLIDE — same
// attributes, different `__name__`. The five continuations that share the
// fanout split across these two shapes, so both are run for every name.
type callSubqRowShape struct {
	name     string
	seed     string
	lhs, rhs string
}

// callSubqCollidingShapes seeds both row shapes with colliding arms.
func callSubqCollidingShapes(t *testing.T, start time.Time) []callSubqRowShape {
	t.Helper()
	const same = "map('x', '1')"
	return []callSubqRowShape{
		{"mixed", callSubqMixedSeed(t, start, same, same), "latency_exp_hist", "latency_float"},
		{"histogram", callSubqHistSeed(t, start, same, same), "latency_exp_hist", "other_exp_hist"},
	}
}

// callSubqQuery builds the doubly-nested composition over the two named
// arms.
func callSubqQuery(fn, lhs, rhs string) string {
	return fmt.Sprintf("%s(((%s) or (%s))[3m:1m])[4m:1m]", fn, lhs, rhs)
}

// TestQuery_CallSubqueryNameDropping_DuplicateLabelset_ChDB is the
// wrong-answer regression for the thirteen name-dropping names, over both
// row shapes. Each used to answer one merged series; each must now refuse
// the query with upstream's own words.
func TestQuery_CallSubqueryNameDropping_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := callSubqIdentityWindow()
	shapes := callSubqCollidingShapes(t, start)
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			srv, _ := newChDBServer(t, shape.seed)
			for _, fn := range callSubqNameDroppingNames {
				query := callSubqQuery(fn, shape.lhs, shape.rhs)
				t.Run(fn, func(t *testing.T) {
					status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
						srv.URL, url.QueryEscape(query), end.Unix()))
					assertDuplicateLabelsetRejected(t, body, status, query)
				})
			}
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
	start, end, _ := callSubqIdentityWindow()
	shapes := callSubqCollidingShapes(t, start)
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			srv, _ := newChDBServer(t, shape.seed)
			for _, fn := range callSubqNamePreservingNames {
				query := callSubqQuery(fn, shape.lhs, shape.rhs)
				t.Run(fn, func(t *testing.T) {
					names := matrixSeriesNames(t, srv.URL, query, end)
					want := map[string]bool{shape.lhs: true, shape.rhs: true}
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
		})
	}
}

// TestQuery_CallSubqueryNameDropping_DistinctLabelsets_ChDB is the
// discriminating control: the same thirteen names over arms whose
// ATTRIBUTES differ must still answer, and must answer two series.
//
// A guard keyed on "this reduction can see more than one metric name"
// rather than on "two names landed in ONE group" would refuse this query
// too, and a fix that simply widened the key for everybody would answer it
// while leaving the genuine collision unreported.
func TestQuery_CallSubqueryNameDropping_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := callSubqIdentityWindow()
	srv, _ := newChDBServer(t, callSubqMixedSeed(t,
		start, "map('x', '1', 'pod', 'h1')", "map('x', '1', 'pod', 'f1')"))

	for _, fn := range callSubqNameDroppingNames {
		query := callSubqQuery(fn, "latency_exp_hist", "latency_float")
		t.Run(fn, func(t *testing.T) {
			series := matrixSeries(t, srv.URL, query, end)
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

// matrixSeries issues one instant query, requires a 200, and returns the
// label set of each matrix series. The doubly-nested composition is a
// SubqueryExpr at the plan root, so its result type is a matrix even from
// /api/v1/query.
func matrixSeries(t *testing.T, srvURL, query string, at time.Time) []map[string]string {
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

// matrixSeriesNames reduces [matrixSeries] to the set of `__name__` values
// it carries, which is the whole of what the name-preserving assertion is
// about.
func matrixSeriesNames(t *testing.T, srvURL, query string, at time.Time) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range matrixSeries(t, srvURL, query, at) {
		out[m["__name__"]] = true
	}
	return out
}
