//go:build chdb

// chDB-backed end-to-end pin for the VERBATIM query Grafana Traces
// Drilldown's breakdown tab issues when the user clicks the "kind"
// groupBy:
//
//	{nestedSetParent<0 && true && kind != nil} | rate() by(kind)
//
// The Drilldown app appends `&& <groupBy> != nil` to EVERY breakdown
// query, so a backend that rejects nil comparisons on intrinsics
// breaks every intrinsic breakdown (kind, status, name, ...). Three
// contracts pinned here:
//
//  1. ACCEPTANCE: the request returns 200, not the pre-fix 422
//     ("traceql: nil comparison on intrinsic kind is unsupported").
//  2. SEMANTICS: `kind != nil` matches EVERY span — reference Tempo
//     stores intrinsics as required parquet columns and its OpExists
//     only tests the nil static (pkg/traceql ast_execute.go), so even
//     a SPAN_KIND_UNSPECIFIED root span lands in the breakdown (as
//     its own group), and is NOT filtered by an enum-zero check.
//  3. WIRE FORM: `by(kind)` label values are reference Tempo's
//     lowercase Static.EncodeToString form ("server", "unspecified"),
//     not the TitleCase OTel-CH column payload ("Server").
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

func TestMetricsQueryRange_DrilldownBreakdownKind_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`)
	// Four spans inside the [10:00, 10:03] query window:
	//   - two Server ROOT spans (10:00:30 → anchor 10:01;
	//     10:01:30 → anchor 10:02),
	//   - one Unspecified-kind ROOT span (10:01:30 → anchor 10:02) —
	//     `kind != nil` must keep it (constant-TRUE semantics),
	//   - one Client CHILD span — `nestedSetParent<0` must drop it,
	//     so no "client" series may surface.
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'root-a', 'Server', 1000000, toDateTime64('2026-05-12 10:00:30.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000002', '1000000000000002', '', 'root-b', 'Server', 1000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000003', '1000000000000003', '', 'root-c', 'Unspecified', 1000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'svc')),
    ('a0000000000000000000000000000001', '1000000000000004', '1000000000000001', 'child', 'Client', 1000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), map(), map('service.name', 'svc'));`)

	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	u := metricsQueryRangeURL(srv.URL, "{nestedSetParent<0 && true && kind != nil} | rate() by(kind)", map[string]string{
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
		t.Fatalf("status=%d body=%s — the Drilldown breakdown query must not be rejected", resp.StatusCode, body)
	}

	var out tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(out.Series) != 2 {
		t.Fatalf("expected 2 series (server + unspecified; client child excluded by nestedSetParent<0), got %d: %s", len(out.Series), body)
	}

	bySeries := map[string]tempo.MetricsSeries{}
	for _, s := range out.Series {
		if len(s.Labels) != 1 || s.Labels[0].Key != "kind" {
			t.Fatalf("expected single `kind` label, got %+v", s.Labels)
		}
		bySeries[s.Labels[0].Value] = s
	}

	server, ok := bySeries["server"]
	if !ok {
		t.Fatalf("missing kind=server series (lowercase reference wire form); got %v: %s", labelValues(bySeries), body)
	}
	unspecified, ok := bySeries["unspecified"]
	if !ok {
		t.Fatalf("missing kind=unspecified series — `kind != nil` must match SPAN_KIND_UNSPECIFIED spans too (got %v): %s", labelValues(bySeries), body)
	}
	if _, ok := bySeries["client"]; ok {
		t.Fatalf("kind=client series must not surface — the child span fails nestedSetParent<0: %s", body)
	}

	// rate() = spans per second per 60s anchor; dense zero-filled grid
	// across the 4 anchors (10:00..10:03).
	wantAnchorsMs := []int64{
		1778580000000, // 10:00
		1778580060000, // 10:01
		1778580120000, // 10:02
		1778580180000, // 10:03
	}
	perSpanRate := 1.0 / 60.0
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
	assertSamples("kind=server", server, []float64{0, perSpanRate, perSpanRate, 0})
	assertSamples("kind=unspecified", unspecified, []float64{0, 0, perSpanRate, 0})
}
