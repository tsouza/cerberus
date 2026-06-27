//go:build chdb

// chDB-backed regression pins for `| select(...)` on the PLAIN-FILTER
// /api/search arm — the showcase "select / by / coalesce" panel query
// `{ status = error } | select(span.http.method, resource.service.name)`.
//
// The select() wrap projection references the carrier maps the selected
// attributes live in (SpanAttributes / ResourceAttributes) from INSIDE
// the canonical Attributes map() expression. On the plain Filter(Scan)
// arm the optimizer's ProjectionPushdown narrows Scan.Columns to the
// ColumnRefs it can see — and its expression walker did not descend
// into chplan.FieldAccess sources, so SpanAttributes was pruned from
// the inner scan and ClickHouse failed the outer scope resolution with
// error 47 (`Unknown expression or function identifier
// 'SpanAttributes'`), surfacing as an HTTP 502 in compose-smoke. The
// structural-join and mixed-`||` arms dodge the rule (pushdown only
// matches Project(Scan) / Project(Filter(Scan))), which is why the
// drilldown-structure chDB test alone did not catch this.
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// plainFilterSeed: two single-span traces — one Error span carrying an
// http.method span attribute (the `{ status = error }` match), one
// Unset span that must stay filtered out.
const plainFilterSeed = `INSERT INTO otel_traces VALUES
    ('b0000000000000000000000000000001', '2000000000000001', '', 'POST /checkout', 'Server', 1500, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Error', '', '', '', map('http.method', 'POST'), map('service.name', 'checkout')),
    ('b0000000000000000000000000000002', '2000000000000002', '', 'GET /home', 'Server', 800, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', '', '', '', map('http.method', 'GET'), map('service.name', 'frontend'));`

func newPlainFilterChDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, plainFilterSeed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runSelectSearch issues /api/search for query and decodes the single
// expected matching trace, failing fast on non-200 (the regression
// surfaced as a 502 wrapping ClickHouse error 47).
func runSelectSearch(t *testing.T, srv *httptest.Server, query string) tempo.TraceSummary {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(query) + "&start=1777593600&end=1777680000&limit=20&spss=20")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("want 1 trace, got %d (%s)", len(sr.Traces), body)
	}
	return sr.Traces[0]
}

// wantSelectedStr asserts one selected attribute surfaced as an OTLP
// stringValue on the only span of the only spanset.
func wantSelectedStr(t *testing.T, tr tempo.TraceSummary, key, want string) {
	t.Helper()
	if len(tr.SpanSets) != 1 || len(tr.SpanSets[0].Spans) != 1 {
		t.Fatalf("want 1 spanset with 1 span, got %+v", tr.SpanSets)
	}
	for _, kv := range tr.SpanSets[0].Spans[0].Attributes {
		if kv.Key == key {
			if kv.Value.StringValue == nil || *kv.Value.StringValue != want {
				t.Errorf("attr %q = %+v, want stringValue %q", key, kv.Value, want)
			}
			return
		}
	}
	t.Errorf("attr %q missing from span attributes: %+v", key, tr.SpanSets[0].Spans[0].Attributes)
}

// TestSearch_SelectAttrs_PlainFilter_ChDB runs the exact showcase
// panel query through the full pipeline (parse → lower → wrap →
// optimize → emit → ClickHouse → response shaping): the Filter(Scan)
// input arm must expose every carrier column the select() shaping
// reads.
func TestSearch_SelectAttrs_PlainFilter_ChDB(t *testing.T) {
	srv := newPlainFilterChDBServer(t)

	tr := runSelectSearch(t, srv, `{ status = error } | select(span.http.method, resource.service.name)`)
	if tr.TraceID == "" {
		t.Fatalf("trace ID missing: %+v", tr)
	}
	wantSelectedStr(t, tr, "http.method", "POST")
	wantSelectedStr(t, tr, "service.name", "checkout")
}

// TestSearch_SelectAttrs_BareScan_ChDB covers the Project(Scan) arm —
// no filter at all — which goes through ProjectionPushdown's other
// match shape (applyProjectScan) and needs the same carrier-column
// plumbing. The seed keeps it to a single matching span by querying
// on the span attribute itself.
func TestSearch_SelectAttrs_BareScan_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, `INSERT INTO otel_traces VALUES
    ('b0000000000000000000000000000003', '2000000000000003', '', 'GET /solo', 'Server', 900, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', map('http.method', 'GET'), map('service.name', 'solo'));`)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tr := runSelectSearch(t, srv, `{} | select(span.http.method, resource.service.name)`)
	wantSelectedStr(t, tr, "http.method", "GET")
	wantSelectedStr(t, tr, "service.name", "solo")
}
