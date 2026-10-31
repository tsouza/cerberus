//go:build chdb

// chDB-backed pin for a `*_over_time(<attribute>)` range query whose
// window holds spans that do not carry the attribute. The aggregate skips
// those spans, but an anchor (or a by() group) where EVERY matched span
// lacks the attribute used to come back as a row with a NULL Value. The
// scan then either failed on it (clickhouse-go v2.50 and later refuse a
// NULL into a plain float64, so the request answered 502) or bound it as
// a fabricated 0 sample. Reference Tempo reports no sample for such a
// step and omits a series that has none.
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

func TestMetricsQueryRange_OverTimeSkipsAttributeLessSteps_ChDB(t *testing.T) {
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
	// Window 10:00–10:03, step 60s; anchors round up, so a span at 10:0N:xx
	// lands on the 10:0(N+1) anchor.
	//   - 10:01 anchor: two `api` spans carrying payload_bytes (2048 + 512).
	//   - 10:02 anchor: one `api` span with no payload_bytes and one whose
	//     value is not a number; nothing numeric to aggregate.
	//   - 10:03 anchor: no spans at all.
	//   - `worker` spans exist (10:01 and 10:02 anchors) but never carry
	//     payload_bytes, so the by(resource.service.name) query has nothing
	//     to report for that group.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'a', 1000, toDateTime64('2026-05-12 10:00:10.000000000', 9), map('payload_bytes', '2048'), map('service.name', 'api')),
    ('a0000000000000000000000000000002', '1000000000000002', '', 'b', 1000, toDateTime64('2026-05-12 10:00:20.000000000', 9), map('payload_bytes', '512'), map('service.name', 'api')),
    ('a0000000000000000000000000000003', '1000000000000003', '', 'c', 1000, toDateTime64('2026-05-12 10:01:10.000000000', 9), map(), map('service.name', 'api')),
    ('a0000000000000000000000000000004', '1000000000000004', '', 'd', 1000, toDateTime64('2026-05-12 10:01:20.000000000', 9), map('payload_bytes', 'n/a'), map('service.name', 'api')),
    ('a0000000000000000000000000000005', '1000000000000005', '', 'e', 1000, toDateTime64('2026-05-12 10:00:30.000000000', 9), map(), map('service.name', 'worker')),
    ('a0000000000000000000000000000006', '1000000000000006', '', 'f', 1000, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'worker'));`)

	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const (
		anchor1Ms = 1778580060000 // 10:01
		anchor2Ms = 1778580120000 // 10:02
		anchor3Ms = 1778580180000 // 10:03
	)

	queryRange := func(t *testing.T, q string) tempo.MetricsQueryRangeResponse {
		t.Helper()
		resp, err := http.Get(metricsQueryRangeURL(srv.URL, q, map[string]string{
			"start": fixtureStartUnix,
			"end":   fixtureEndUnix,
			"step":  "60s",
		}))
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", q, resp.StatusCode, body)
		}
		var out tempo.MetricsQueryRangeResponse
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v\nbody: %s", q, err, body)
		}
		return out
	}
	samplesAt := func(s tempo.MetricsSeries) map[int64]float64 {
		m := map[int64]float64{}
		for _, sm := range s.Samples {
			m[sm.TimestampMs] = sm.Value
		}
		return m
	}

	for _, q := range []string{
		"{ } | sum_over_time(.payload_bytes)",
		"{ } | max_over_time(span.payload_bytes)",
		"{ } | avg_over_time(.payload_bytes)",
	} {
		t.Run(q, func(t *testing.T) {
			out := queryRange(t, q)
			if len(out.Series) != 1 {
				t.Fatalf("series = %+v, want exactly one", out.Series)
			}
			got := samplesAt(out.Series[0])
			if _, ok := got[anchor1Ms]; !ok {
				t.Fatalf("no sample at the populated 10:01 anchor: %+v", out.Series[0].Samples)
			}
			// The attribute-less anchor answers exactly like the empty one:
			// whatever the range-fill policy does for a step with no spans,
			// a step with no numeric observation gets the same.
			v2, ok2 := got[anchor2Ms]
			v3, ok3 := got[anchor3Ms]
			if ok2 != ok3 || v2 != v3 {
				t.Errorf("attribute-less 10:02 anchor = (%g, present=%v), empty 10:03 anchor = (%g, present=%v); want identical",
					v2, ok2, v3, ok3)
			}
		})
	}

	t.Run("by group without numeric values", func(t *testing.T) {
		out := queryRange(t, "{ } | sum_over_time(.payload_bytes) by (resource.service.name)")
		var services []string
		for _, s := range out.Series {
			for _, l := range s.Labels {
				if l.Key == "resource.service.name" {
					services = append(services, l.Value)
				}
			}
		}
		if len(services) != 1 || services[0] != "api" {
			t.Errorf("series services = %v, want only [api]: the worker group carries no payload_bytes, so the reference omits it",
				services)
		}
	})
}
