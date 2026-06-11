//go:build chdb

// chDB-backed end-to-end pin for `| histogram_over_time(duration)` on
// /api/metrics/query_range: parse → lower → emit → embedded ClickHouse
// → response shaping. Pins the three reference-Tempo contracts the
// crawl found broken or at risk:
//
//  1. Bucket UNIT + VALUE: the `__bucket` label is Log2Bucketize(ns)
//     rebased to SECONDS — the duration rounded UP to the next power
//     of two nanoseconds, divided by 1e9 (pkg/traceql/ast_metrics.go
//     bucketizeDuration). Pre-fix cerberus emitted the raw log2
//     exponent divided by 1e9 (~3e-08 for a 1.024µs span) instead of
//     1.024e-06.
//  2. Label FORM: the bucket label string uses Go's shortest float
//     rendering ("1.024e-06"), not ClickHouse's ("0.000001024"), so
//     series keys align with what consumers derive from Tempo's
//     doubleValue wire shape.
//  3. ZERO-FILL: every observed (group, __bucket) series reaches the
//     wire dense — one sample per grid anchor, 0 where no span lands
//     — because upstream histogram series ride
//     NewCountOverTimeAggregator and SeriesSet.ToProto skips only NaN.
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMetricsQueryRangeHistogram_DurationBucketsSeconds_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    Duration Int64,
    Timestamp DateTime64(9),
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`)
	// Two spans inside the [10:00, 10:03] query window:
	//   - 1024 ns  (2^10, exact power of two) at 10:01:30 → bucket
	//     1024/1e9 = 1.024e-06 s, anchor 10:02.
	//   - 1e8 ns   (100ms)                    at 10:00:30 → bucket
	//     2^ceil(log2(1e8))/1e9 = 134217728/1e9 = 0.134217728 s,
	//     anchor 10:01.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'fast', 1024, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000002', '1000000000000002', '', 'slow', 100000000, toDateTime64('2026-05-12 10:00:30.000000000', 9), map(), map('service.name', 'svc'));`)

	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{} | histogram_over_time(duration)", map[string]string{
		"start": fixtureStartUnix, // 2026-05-12T10:00:00Z
		"end":   fixtureEndUnix,   // +3m
		"step":  "60s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var out tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(out.Series) != 2 {
		t.Fatalf("expected 2 series (one per bucket), got %d: %s", len(out.Series), body)
	}

	bySeries := map[string]tempo.MetricsSeries{}
	for _, s := range out.Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "__bucket" {
			t.Fatalf("expected single __bucket label, got %+v", s.Labels)
		}
		bySeries[s.Labels[0].Value] = s
	}

	// Contract 1 + 2: bucket values in seconds, Go shortest-form
	// rendering. The pre-fix shape surfaced "1e-08"-magnitude buckets
	// (log2 exponent / 1e9); the CH string form would have been
	// "0.000001024".
	fast, ok := bySeries["1.024e-06"]
	if !ok {
		t.Fatalf("missing __bucket=1.024e-06 series (got %v): %s", labelValues(bySeries), body)
	}
	slow, ok := bySeries["0.134217728"]
	if !ok {
		t.Fatalf("missing __bucket=0.134217728 series (got %v): %s", labelValues(bySeries), body)
	}

	// Contract 3: dense series — 4 grid anchors (10:00..10:03 at 60s),
	// zero-filled outside the span's anchor.
	wantAnchorsMs := []int64{
		1778580000000, // 10:00
		1778580060000, // 10:01
		1778580120000, // 10:02
		1778580180000, // 10:03
	}
	assertSamples := func(name string, s tempo.MetricsSeries, wantValues []float64) {
		t.Helper()
		if len(s.Samples) != len(wantAnchorsMs) {
			t.Fatalf("%s: expected %d zero-filled samples, got %d: %+v", name, len(wantAnchorsMs), len(s.Samples), s.Samples)
		}
		for i, sm := range s.Samples {
			if sm.TimestampMs != wantAnchorsMs[i] {
				t.Errorf("%s: sample[%d] ts = %d, want %d", name, i, sm.TimestampMs, wantAnchorsMs[i])
			}
			if sm.Value != wantValues[i] {
				t.Errorf("%s: sample[%d] value = %g, want %g", name, i, sm.Value, wantValues[i])
			}
		}
	}
	assertSamples("fast bucket", fast, []float64{0, 0, 1, 0})
	assertSamples("slow bucket", slow, []float64{0, 1, 0, 0})
}

// labelValues lists the map keys for failure messages.
func labelValues(m map[string]tempo.MetricsSeries) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
