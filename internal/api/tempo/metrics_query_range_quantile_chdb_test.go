//go:build chdb

// chDB-backed end-to-end pin for `| quantile_over_time(duration, ...)` on
// /api/metrics/query_range: parse → lower → emit → embedded ClickHouse →
// bucket fold → JSON response.
//
// The unit tests in metrics_histogram_quantile_test.go pin the fold in
// isolation, against a hand-built row stream. This one pins it against the
// stream the EMITTER actually produces, which is where the defect lived: the
// matrix SQL plants a synthetic (bucket 0, count 0) row per (group, anchor) so
// an anchor with no spans still yields a row, and the fold read that synthetic
// row as an observed boundary. log2(0) is -Inf, the interpolation evaluated to
// NaN, and a NaN cannot be JSON-encoded — so the response came back 200 with an
// empty body. Nothing between the SQL and the client could tell that apart from
// "this query matched nothing", which is why it reached a dashboard.
//
// A hand-built stream cannot catch a change in what the emitter plants; only a
// roundtrip through real SQL can. That is the layer this file adds.
package tempo_test

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMetricsQueryRangeQuantile_MultiPhiOverSparseWindow_ChDB drives the exact
// query shape the showcase dashboard's failing panel used —
// `{ } | quantile_over_time(duration, .5, .9, .99)` — over a window whose grid
// anchors are mostly empty, so the emitter's zero-fill rows are guaranteed to
// be in the stream.
func TestMetricsQueryRangeQuantile_MultiPhiOverSparseWindow_ChDB(t *testing.T) {
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
	// Four spans, all landing on the 10:02 anchor (anchors round up), split
	// 3-to-1 across two duration buckets. The split matters: phi=0.5 targets
	// the 2nd of 4 samples, which sits STRICTLY INSIDE the 3-sample bucket, so
	// the estimate is interpolated against a lower boundary rather than landing
	// on a bucket edge. Interpolation is the only path that reads the
	// predecessor boundary, and therefore the only path a spurious zero-fill
	// boundary can corrupt. The window's other three anchors hold no spans, so
	// they arrive carrying only the emitter's zero-fill row.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'a', 1024, toDateTime64('2026-05-12 10:01:10.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000002', '1000000000000002', '', 'b', 1024, toDateTime64('2026-05-12 10:01:20.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000004', '1000000000000004', '', 'd', 1024, toDateTime64('2026-05-12 10:01:25.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000003', '1000000000000003', '', 'c', 100000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'svc'));`)

	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{ } | quantile_over_time(duration, .5, .9, .99)", map[string]string{
		"start": fixtureStartUnix, // 2026-05-12T10:00:00Z
		"end":   fixtureEndUnix,   // +3m
		"step":  "60s",
	})
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)

	// The headline assertion. A NaN in the payload used to surface here as
	// exactly this: 200, zero bytes.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if body == "" {
		t.Fatal("200 with an empty body — the response failed to encode, which is " +
			"indistinguishable from a query that legitimately matched nothing")
	}

	var out tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}

	// One series per phi.
	byPhi := map[string]tempo.MetricsSeries{}
	for _, s := range out.Series {
		for _, l := range s.Labels {
			if l.Key == "p" {
				byPhi[l.Value] = s
			}
		}
	}
	for _, phi := range []string{"0.5", "0.9", "0.99"} {
		s, ok := byPhi[phi]
		if !ok {
			t.Errorf("missing p=%s series: %s", phi, body)
			continue
		}
		// Every sample has to be a finite number: one NaN anywhere in the
		// document is enough for the encoder to refuse all of it.
		for i, sm := range s.Samples {
			if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
				t.Errorf("p=%s sample[%d] = %g at ts=%d", phi, i, sm.Value, sm.TimestampMs)
			}
		}
	}

	// The populated anchor must carry a real estimate rather than collapsing
	// to the zero-fill's 0 — otherwise "no NaN" would be satisfied by
	// answering nothing everywhere.
	const populatedAnchorMs = 1778580120000 // 10:02 — anchors round up
	for phi, s := range byPhi {
		var got float64
		var found bool
		for _, sm := range s.Samples {
			if sm.TimestampMs == populatedAnchorMs {
				got, found = sm.Value, true
			}
		}
		if !found {
			t.Errorf("p=%s: no sample at the populated anchor: %+v", phi, s.Samples)
			continue
		}
		if got <= 0 {
			t.Errorf("p=%s: populated anchor read %g, want a positive duration estimate", phi, got)
		}
	}
}
