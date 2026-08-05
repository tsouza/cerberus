//go:build chdb

// chDB-backed pins for the SYNTHETIC-NAME half of `__name__` matching on the
// metadata surfaces (/api/v1/series, /api/v1/label/<name>/values).
//
// A classic histogram is stored as ONE row under a bare base MetricName, and
// PromQL exposes it only as the synthetic companion series `<base>_bucket` /
// `<base>_count` / `<base>_sum`. An EXACT `__name__` matcher is resolved
// against that synthetic alphabet by the selector lowering (suffix strip →
// histogram row). A REGEX `__name__` matcher has no suffix to strip, so
// evaluating it against the stored MetricName column asks about the BASE
// alphabet — a question no client ever asks — and the endpoint answers
// "success" with a silently incomplete result.
//
// The asymmetry is what makes it a silent wrong answer rather than an error,
// so each shape below is pinned against its counterpart: regex-vs-exact and
// histogram-vs-non-histogram over one seed.

package prom_test

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"
)

// syntheticNameSeed seeds one classic-histogram family (stored under its bare
// base name, with the bucket arrays a `_bucket` selector fans out), one
// EXPONENTIAL-histogram family, and one plain sum-table counter. The counter
// is the control: it proves the regex machinery itself works on this seed, so
// an empty histogram result cannot be dismissed as "the regex matched
// nothing".
//
// The exp-histogram row carries the same `route` attribute as the classic one
// so the negated-regex shape (which selects on `route` rather than on a name
// substring) sweeps both physical layouts in one assertion.
func syntheticNameSeed(t *testing.T, ts string) string {
	t.Helper()
	return metaShapedMetricsDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_sum (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('synth_requests_total', '', '', map('route', '/a'), toDateTime64('%[1]s', 9), 7.0);
INSERT INTO otel_metrics_histogram (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES
    ('synth_latency_seconds', '', '', map('route', '/a'), toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]);
INSERT INTO otel_metrics_exponential_histogram (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES
    ('synth_expo_seconds', '', '', map('route', '/a'), toDateTime64('%[1]s', 9), 5, 2.5, 0, 0, 0, [2, 3], 0, []);`,
		ts)
}

// syntheticNameWindow returns the seed timestamp literal plus the [start,end]
// pair every request below carries, so the assertions never depend on the
// default lookback.
func syntheticNameWindow() (ts string, start, end int64) {
	seedTime := time.Now().UTC().Add(-time.Minute)
	return seedTime.Format("2006-01-02 15:04:05.000"),
		seedTime.Add(-5 * time.Minute).Unix(),
		seedTime.Add(5 * time.Minute).Unix()
}

// metadataGet issues a windowed metadata request with a single match[]
// selector and returns the response body.
func metadataGet(t *testing.T, base, path, selector string, start, end int64) string {
	t.Helper()
	u := fmt.Sprintf("%s%s?match%%5B%%5D=%s&start=%d&end=%d",
		base, path, url.QueryEscape(selector), start, end)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", u, resp.StatusCode, body)
	}
	return body
}

// seriesNames returns the sorted `__name__` values of a /api/v1/series
// response, one entry per returned series (duplicates kept, so a bucket
// fan-out's per-boundary series are visible).
func seriesNames(t *testing.T, body string) []string {
	t.Helper()
	out := []string{}
	for _, lset := range decodeSeriesData(t, body) {
		out = append(out, lset["__name__"])
	}
	sort.Strings(out)
	return out
}

// TestSeries_RegexNameMatcher_HistogramSynthetic_ChDB is the core pin: a regex
// `__name__` matcher must select the histogram family's synthetic series, and
// must select exactly the same set the EXACT-matcher path already serves.
// Before the synthetic-name resolution the regex arm returned an empty data
// array here while the counter control below returned its series.
func TestSeries_RegexNameMatcher_HistogramSynthetic_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := seriesNames(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~".*synth_latency_seconds.*"}`, start, end))
	want := []string{
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_count",
		"synth_latency_seconds_sum",
	}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ over a histogram family: got %v, want %v", got, want)
	}
}

// TestSeries_RegexVsExactNameMatcher_ChDB pins the asymmetry itself: the exact
// `<base>_sum` matcher and a regex that selects only `<base>_sum` must return
// the identical series. A regression that re-breaks the regex arm fails here
// even if the expected-set test above is edited.
func TestSeries_RegexVsExactNameMatcher_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	exact := metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__="synth_latency_seconds_sum"}`, start, end)
	regex := metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~"synth_latency_seconds_su."}`, start, end)

	exactSets, regexSets := decodeSeriesData(t, exact), decodeSeriesData(t, regex)
	if len(exactSets) != 1 || len(regexSets) != len(exactSets) {
		t.Fatalf("exact/regex series count mismatch: exact=%v regex=%v", exactSets, regexSets)
	}
	for k, v := range exactSets[0] {
		if regexSets[0][k] != v {
			t.Errorf("label %q: exact=%q regex=%q", k, v, regexSets[0][k])
		}
	}
}

// TestSeries_RegexNameMatcher_NonHistogram_ChDB is the control: the same regex
// shape against a sum-table counter. It holds both before and after the
// synthetic-name resolution, which is exactly why it belongs here — it proves
// the histogram emptiness is a name-resolution defect and not a broken regex.
func TestSeries_RegexNameMatcher_NonHistogram_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := seriesNames(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~".*synth_requests_total.*"}`, start, end))
	want := []string{"synth_requests_total"}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ over a counter: got %v, want %v", got, want)
	}
}

// TestSeries_RegexNameMatcher_AttributePredicatePreserved_ChDB pins that the
// non-name matchers survive the rewrite into equality-pinned variants: a
// selector whose attribute predicate excludes the seeded row must stay empty
// rather than being widened by the fan-out.
func TestSeries_RegexNameMatcher_AttributePredicatePreserved_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	matching := seriesNames(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~".*synth_latency_seconds.*",route="/a"}`, start, end))
	if len(matching) == 0 {
		t.Fatalf("route=/a selector returned no series")
	}
	excluded := decodeSeriesData(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~".*synth_latency_seconds.*",route="/absent"}`, start, end))
	if len(excluded) != 0 {
		t.Fatalf("route=/absent selector must return no series, got %v", excluded)
	}
}

// TestLabelValues_MetricName_RegexNameMatcher_ChDB pins the
// /api/v1/label/__name__/values surface: a regex selecting a histogram family
// must list every derived name, matching what the unmatched catalog advertises
// for the same family. The bare base name must NOT appear — reference
// Prometheus never exposes it, and advertising a name the query surface cannot
// serve is the inverse defect.
func TestLabelValues_MetricName_RegexNameMatcher_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := decodeStringData(t, metadataGet(t, srv.URL, "/api/v1/label/__name__/values",
		`{__name__=~".*synth_latency.*"}`, start, end))
	want := []string{
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_count",
		"synth_latency_seconds_sum",
	}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ values: got %v, want %v", got, want)
	}
}

// TestLabelValues_Attribute_RegexNameMatcher_ChDB pins the sibling
// /api/v1/label/<attribute>/values surface — the shape Grafana's label
// dropdown drives. The histogram row's attribute is unreachable while the
// regex resolves against the stored base name only.
func TestLabelValues_Attribute_RegexNameMatcher_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := decodeStringData(t, metadataGet(t, srv.URL, "/api/v1/label/route/values",
		`{__name__=~".*synth_latency_seconds.*"}`, start, end))
	want := []string{"/a"}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ route values: got %v, want %v", got, want)
	}
}

// TestLabels_RegexNameMatcher_Histogram_ChDB pins /api/v1/labels — the labels
// chip Grafana renders for a metric — over the same regex shape.
func TestLabels_RegexNameMatcher_Histogram_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := decodeStringData(t, metadataGet(t, srv.URL, "/api/v1/labels",
		`{__name__=~".*synth_latency_seconds.*"}`, start, end))
	want := []string{"__name__", "le", "route"}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ label names: got %v, want %v", got, want)
	}
}

// TestSeries_RegexNameMatcher_ExpHistogramSynthetic_ChDB is the
// exp-histogram twin of the core pin above: an exponential histogram is
// likewise stored as ONE row under a bare base name, so a regex `__name__`
// matcher must resolve it to its synthetic companion series.
//
// The expected set is `_count` / `_sum` and nothing else. There is no
// `_bucket`: an exp-histogram row carries no ExplicitBounds ladder to fan out
// (Scale / PositiveBucketCounts instead), so no per-boundary series exists.
// The BARE name must not appear either — it is not Prom-wire visible, and the
// unknown-name scan (TablesForUnknownName) covers only the gauge and sum
// tables, so nothing should surface it.
func TestSeries_RegexNameMatcher_ExpHistogramSynthetic_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := seriesNames(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__=~".*synth_expo_seconds.*"}`, start, end))
	want := []string{
		"synth_expo_seconds_count",
		"synth_expo_seconds_sum",
	}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ over an exp-histogram family: got %v, want %v", got, want)
	}
}

// TestLabelValues_MetricName_RegexNameMatcher_ExpHistogram_ChDB pins the same
// resolution on the /api/v1/label/__name__/values surface — the list Grafana's
// metric picker renders. A name the series surface serves must be advertised
// here, and the bare base name must not be.
func TestLabelValues_MetricName_RegexNameMatcher_ExpHistogram_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := decodeStringData(t, metadataGet(t, srv.URL, "/api/v1/label/__name__/values",
		`{__name__=~".*synth_expo.*"}`, start, end))
	want := []string{
		"synth_expo_seconds_count",
		"synth_expo_seconds_sum",
	}
	if !equalStringSlice(got, want) {
		t.Fatalf("regex __name__ values over an exp-histogram: got %v, want %v", got, want)
	}
}

// TestSeries_NegatedRegexNameMatcher_ChDB pins the negated shape: `!~` is the
// same non-equality class, so the histogram family must surface whenever the
// pattern does not exclude it. The selector excludes only the counter, so
// BOTH histogram layouts answer — the classic family with its bucket fan-out,
// the exponential one with its two companions.
func TestSeries_NegatedRegexNameMatcher_ChDB(t *testing.T) {
	ts, start, end := syntheticNameWindow()
	srv, _ := newChDBServer(t, syntheticNameSeed(t, ts))

	got := seriesNames(t, metadataGet(t, srv.URL, "/api/v1/series",
		`{__name__!~".*requests.*",route="/a"}`, start, end))
	want := []string{
		"synth_expo_seconds_count",
		"synth_expo_seconds_sum",
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_bucket",
		"synth_latency_seconds_count",
		"synth_latency_seconds_sum",
	}
	if !equalStringSlice(got, want) {
		t.Fatalf("negated regex __name__: got %v, want %v", got, want)
	}
}
