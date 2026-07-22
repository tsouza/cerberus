package tempo_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMetricsQueryRangeHistogram_Ungrouped pins the
// `| histogram_over_time(duration)` HTTP wire shape: one series per
// `__bucket` edge, per-anchor counts, empty exemplar envelope (upstream
// Tempo registers exemplarNaN for histograms — placeholders only).
// Grafana's Traces Drilldown app (preinstalled since Grafana 12.x)
// drives this exact query on its duration-histogram panel; before the
// wiring landed the handler 400'd it as "not a metrics-pipeline
// expression".
func TestMetricsQueryRangeHistogram_Ungrouped(t *testing.T) {
	t.Parallel()

	ts := func(min int) time.Time {
		return time.Date(2026, 5, 12, 10, min, 0, 0, time.UTC)
	}
	// The matrix SQL projects map('__bucket', toString(<bucket edge>))
	// for the ungrouped histogram. Two buckets, two anchors each.
	q := &stubQuerier{samples: []chclient.Sample{
		{Labels: map[string]string{"__bucket": "0.25"}, Timestamp: ts(1), Value: 3},
		{Labels: map[string]string{"__bucket": "0.25"}, Timestamp: ts(0), Value: 1},
		{Labels: map[string]string{"__bucket": "0.5"}, Timestamp: ts(0), Value: 2},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | histogram_over_time(duration)", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "60s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series (one per __bucket), got %d: %+v", len(body.Series), body)
	}
	for _, s := range body.Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "__bucket" {
			t.Errorf("expected single __bucket label per histogram series, got %+v", s.Labels)
		}
		if s.Exemplars == nil || len(s.Exemplars) != 0 {
			t.Errorf("expected empty exemplar envelope, got %+v", s.Exemplars)
		}
	}
	// Series are keyed canonically; the 0.25 bucket carries both anchors
	// sorted ascending.
	var b025 *tempo.MetricsSeries
	for i := range body.Series {
		if body.Series[i].Labels[0].Value == "0.25" {
			b025 = &body.Series[i]
		}
	}
	if b025 == nil {
		t.Fatalf("no series for __bucket=0.25: %+v", body.Series)
		return
	}
	if len(b025.Samples) != 2 || b025.Samples[0].Value != 1 || b025.Samples[1].Value != 3 {
		t.Errorf("bucket 0.25 samples wrong (want ascending [1 3]): %+v", b025.Samples)
	}

	// The histogram matrix emitter must have produced the arrayJoin
	// fan-out with the __bucket grouping column.
	assertSQLContains(t, q.lastSQL, "arrayJoin")
	assertSQLContains(t, q.lastSQL, "anchor_ts")
	assertSQLContains(t, q.lastSQL, "__bucket")
}

// TestMetricsQueryRangeHistogram_GroupBy pins the grouped shape: the
// series label list is (group display name…, __bucket) and the group
// label uses the Tempo-canonical scope-prefixed wire name.
func TestMetricsQueryRangeHistogram_GroupBy(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		{
			Labels:    map[string]string{"resource.service.name": "checkout", "__bucket": "0.25"},
			Timestamp: ts,
			Value:     4,
		},
		{
			Labels:    map[string]string{"resource.service.name": "cart", "__bucket": "0.25"},
			Timestamp: ts,
			Value:     7,
		},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL,
		"{} | histogram_over_time(duration) by (resource.service.name)",
		map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}
	for _, s := range body.Series {
		if len(s.Labels) != 2 {
			t.Fatalf("expected (group, __bucket) label pair, got %+v", s.Labels)
		}
		// Label order follows histogramLabelNames: group-by first
		// (Tempo-canonical scope-prefixed name), then __bucket.
		if s.Labels[0].Key != "resource.service.name" || s.Labels[1].Key != "__bucket" {
			t.Errorf("label order/name mismatch: %+v", s.Labels)
		}
	}
}

// TestMetricsQueryRangeHistogram_BucketLabelNormalisation pins the
// wire form of the `__bucket` label value to Go's shortest round-trip
// float rendering (strconv 'g'/-1, what fmt.Sprint(float64) produces —
// the same form a consumer derives from reference Tempo's doubleValue
// projection). ClickHouse's `toString(Float64)` disagrees with Go on
// small magnitudes — CH renders 1.024e-6 as "0.000001024" — so without
// the handler-side rewrite, sub-100µs duration buckets would carry a
// CH-version-dependent label that never aligns with reference Tempo's
// (the tempo compatibility differ keys series by the stringified
// label set).
func TestMetricsQueryRangeHistogram_BucketLabelNormalisation(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 10, 1, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		// CH toString rendering of the 1.024µs bucket (2^10 ns / 1e9).
		{Labels: map[string]string{"__bucket": "0.000001024"}, Timestamp: ts, Value: 3},
		// Values Go renders identically must pass through unchanged.
		{Labels: map[string]string{"__bucket": "0.25"}, Timestamp: ts, Value: 5},
	}}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | histogram_over_time(duration)", map[string]string{
		"start": fixtureStartUnix,
		"end":   fixtureEndUnix,
		"step":  "60s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var body tempo.MetricsQueryRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(body.Series), body)
	}
	got := map[string]bool{}
	for _, s := range body.Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "__bucket" {
			t.Fatalf("expected single __bucket label, got %+v", s.Labels)
		}
		got[s.Labels[0].Value] = true
	}
	for _, want := range []string{"1.024e-06", "0.25"} {
		if !got[want] {
			t.Errorf("missing __bucket=%q on the wire (got %v) — bucket labels must use Go's shortest float form", want, got)
		}
	}
}
