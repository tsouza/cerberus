//go:build chdb

package prom_test

// PARITY (Layer 6a — chDB roundtrip; result identity across the two
// histogram base-name enumeration shapes).
//
// The classic-histogram base-name enumeration behind every matched metadata
// call and the `__name__` catalog takes the grouped
// `GROUP BY MetricName HAVING max(TimeUnix) BETWEEN start AND end` arm, plus a
// WHERE-bounded recheck of the names whose newest sample lies after end,
// whenever the window ends at or after the wall clock — the shape Grafana
// sends for a relative range, whose end it rounds up past "now". A window
// ending in the past keeps the WHERE-bounded DISTINCT. These tests pin that
// both shapes answer the same request identically on the same seed:
//
//   - histogram families with a sample inside the window list (as their
//     companions) and a family whose newest sample predates the window does
//     not;
//   - a bare pinned ordinary metric is not mistaken for a histogram base, and
//     a bare pinned histogram base still reaches its companion rows;
//   - a family whose samples lie only after the window's end, or before its
//     start and after its end with none inside, does not list, while a family
//     with a sample inside the window and a newer one after its end does.
//
// The histogram table carries proj_series (materialized) so the grouped arm is
// also proven correct when served from the projection.

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

// histogramBaseWindowSeed seeds three histogram families and one gauge around
// a 15-day window: one family and one gauge series inside it near "now", one
// family days inside it, and one family plus one gauge series older than its
// start.
func histogramBaseWindowSeed(now time.Time) string {
	lit := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02 15:04:05.000000000")
	}
	const (
		recentAge = 2 * time.Minute
		withinAge = 10 * 24 * time.Hour
		staleAge  = 20 * 24 * time.Hour
	)
	return metaShapedMetricsDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('kube_pod_info', map('namespace', 'ns-live'),  toDateTime64('%[1]s', 9), 1.0),
    ('kube_pod_info', map('namespace', 'ns-stale'), toDateTime64('%[3]s', 9), 1.0);
INSERT INTO otel_metrics_histogram (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES
    ('h_recent_seconds', '', '', map('namespace', 'ns-hist-recent'), toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_within_seconds', '', '', map('namespace', 'ns-hist-within'), toDateTime64('%[2]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_stale_seconds',  '', '', map('namespace', 'ns-hist-stale'),  toDateTime64('%[3]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]);
`, lit(recentAge), lit(withinAge), lit(staleAge)) + addSeriesProjection("otel_metrics_histogram")
}

// histogramBaseWindows returns the shared window start plus the two ends the
// tests compare: one rounded up past the wall clock (grouped enumeration) and
// one just in the past but after every in-window sample (WHERE-bounded
// DISTINCT).
func histogramBaseWindows(now time.Time) (start, endAfterNow, endInPast int64) {
	const (
		window       = 15 * 24 * time.Hour
		roundUpSlack = time.Minute
		pastSlack    = 30 * time.Second
	)
	return now.Add(-window).Unix(), now.Add(roundUpSlack).Unix(), now.Add(-pastSlack).Unix()
}

func metadataWindowURL(base, path, selector string, start, end int64) string {
	u := fmt.Sprintf("%s%s?start=%d&end=%d", base, path, start, end)
	if selector != "" {
		u += "&match%5B%5D=" + url.QueryEscape(selector)
	}
	return u
}

func assertSameStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s:\n got:  %v\n want: %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s:\n got:  %v\n want: %v", what, got, want)
		}
	}
}

// TestHistogramBaseNames_WindowEndingNowParity pins the `__name__` catalog:
// the window ending after now (grouped arm) and the window ending in the past
// (DISTINCT arm) return the identical name set, and that set holds exactly the
// in-window histogram families' companions plus the gauge.
func TestHistogramBaseNames_WindowEndingNowParity(t *testing.T) {
	now := time.Now().UTC()
	srv, _ := newChDBServer(t, histogramBaseWindowSeed(now))
	start, endAfterNow, endInPast := histogramBaseWindows(now)

	grouped := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/__name__/values", "", start, endAfterNow)))
	distinct := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/__name__/values", "", start, endInPast)))

	want := []string{
		"h_recent_seconds_bucket", "h_recent_seconds_count", "h_recent_seconds_sum",
		"h_within_seconds_bucket", "h_within_seconds_count", "h_within_seconds_sum",
		"kube_pod_info",
	}
	assertSameStrings(t, "__name__ values, window ending after now", grouped, want)
	assertSameStrings(t, "__name__ values, window ending in the past", distinct, want)
}

// TestHistogramBaseNames_MatchedLabelValuesParity pins the Grafana
// `label_values(<metric>, namespace)` request: for an ordinary metric and for a
// bare classic-histogram base, the window ending after now and the window
// ending in the past answer identically, out-of-window values stay excluded,
// and the histogram base still reaches its companion rows.
func TestHistogramBaseNames_MatchedLabelValuesParity(t *testing.T) {
	now := time.Now().UTC()
	srv, _ := newChDBServer(t, histogramBaseWindowSeed(now))
	start, endAfterNow, endInPast := histogramBaseWindows(now)

	cases := []struct {
		selector string
		want     []string
	}{
		{selector: "kube_pod_info", want: []string{"ns-live"}},
		{selector: "h_within_seconds", want: []string{"ns-hist-within"}},
		{selector: "h_stale_seconds", want: []string{}},
	}
	for _, tc := range cases {
		grouped := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/namespace/values", tc.selector, start, endAfterNow)))
		distinct := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/namespace/values", tc.selector, start, endInPast)))
		assertSameStrings(t, tc.selector+" namespace values, window ending after now", grouped, tc.want)
		assertSameStrings(t, tc.selector+" namespace values, window ending in the past", distinct, tc.want)
	}
}

// histogramBaseFutureDatedSeed extends histogramBaseWindowSeed with three
// families carrying samples after the window's end, as a producer with a
// clock ahead of the server writes them: one whose only sample lies after
// the end, one with samples before the start and after the end but none
// inside, and one with a sample inside the window as well as one after it.
func histogramBaseFutureDatedSeed(now time.Time) string {
	lit := func(d time.Duration) string {
		return now.Add(d).Format("2006-01-02 15:04:05.000000000")
	}
	const (
		futureOffset = 2 * time.Hour
		insideOffset = -time.Hour
		beforeOffset = -20 * 24 * time.Hour
	)
	return histogramBaseWindowSeed(now) + fmt.Sprintf(`
INSERT INTO otel_metrics_histogram (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds) VALUES
    ('h_future_seconds',   '', '', map('namespace', 'ns-hist-future'),   toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_straddle_seconds', '', '', map('namespace', 'ns-hist-straddle'), toDateTime64('%[3]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_straddle_seconds', '', '', map('namespace', 'ns-hist-straddle'), toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_mixed_seconds',    '', '', map('namespace', 'ns-hist-mixed'),    toDateTime64('%[2]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]),
    ('h_mixed_seconds',    '', '', map('namespace', 'ns-hist-mixed'),    toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 0.5]);
`, lit(futureOffset), lit(insideOffset), lit(beforeOffset))
}

// TestHistogramBaseNames_FutureDatedSamplesExcluded pins the `__name__`
// catalog against samples written after the window's end. A family whose only
// samples lie after the end, or before the start and after the end with none
// inside, has no sample in the window and is not listed; a family with a
// sample inside the window is listed even though its newest sample lies after
// the end. The window ending after now and the window ending in the past
// answer identically.
func TestHistogramBaseNames_FutureDatedSamplesExcluded(t *testing.T) {
	now := time.Now().UTC()
	srv, _ := newChDBServer(t, histogramBaseFutureDatedSeed(now))
	start, endAfterNow, endInPast := histogramBaseWindows(now)

	grouped := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/__name__/values", "", start, endAfterNow)))
	distinct := stringData(t, getMetaData(t, metadataWindowURL(srv.URL, "/api/v1/label/__name__/values", "", start, endInPast)))

	want := []string{
		"h_mixed_seconds_bucket", "h_mixed_seconds_count", "h_mixed_seconds_sum",
		"h_recent_seconds_bucket", "h_recent_seconds_count", "h_recent_seconds_sum",
		"h_within_seconds_bucket", "h_within_seconds_count", "h_within_seconds_sum",
		"kube_pod_info",
	}
	assertSameStrings(t, "__name__ values with future-dated samples, window ending after now", grouped, want)
	assertSameStrings(t, "__name__ values with future-dated samples, window ending in the past", distinct, want)
}
