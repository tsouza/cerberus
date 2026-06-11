//go:build chdb

// chDB-backed consumer-grade check for Grafana Traces Drilldown's
// "Structure" tab: the EXACT TraceQL its buildQuery emits (rate
// metric; traces-drilldown
// src/components/Explore/TracesByService/Tabs/Structure/StructureScene.tsx)
// goes through the full pipeline — parse → lower → wrap → optimize →
// emit → ClickHouse → response shaping — and the /api/search response
// must carry everything the tab's mergeTraces consumer reads:
//
//   - spanSets[].spans[].attributes with nestedSetLeft /
//     nestedSetRight / nestedSetParent intValues (utils.ts
//     nestedSetLeft() THROWS when missing) carrying reference Tempo's
//     exact nested-set numbering for this tree,
//   - `service.name` stringValue + span `name` (tree-node.ts nodeName
//     builds `${svcName}:${s.name}`),
//   - `status` stringValue in reference Tempo's lowercase wire casing.
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

// tracesDDL is the OTel-CH traces shape covering every column the
// Drilldown structure query touches: the structural-join envelope
// columns + the nested-set numbering walk inputs. MergeTree because
// the emitter may promote Filter(Scan) predicates to PREWHERE.
const tracesDDL = `CREATE TABLE otel_traces (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    Duration Int64,
    Timestamp DateTime64(9),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    SpanAttributes Map(String, String),
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// The seeded trace tree (timestamps strictly increasing in tree
// order, so the deterministic sibling order matches insertion order):
//
//	r1 GET /home  (Server,  frontend)            left=1 right=8 parent=-1
//	├─ c1 auth     (Internal, auth)               left=2 right=3 parent=1
//	└─ c2 checkout (Server,  checkout, Error)     left=4 right=7 parent=1
//	   └─ g1 db    (Client,  db)                  left=5 right=6 parent=4
//
// `{nestedSetParent<0}` matches r1; `&>> {kind=server}` matches the
// server descendant c2 plus (union form) r1; the `||` arm re-adds r1.
// Result: {r1, c2}.
const tracesSeed = `INSERT INTO otel_traces VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'GET /home', 'Server', 1000, toDateTime64('2026-05-01 10:00:00.000000001', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('a0000000000000000000000000000001', '1000000000000002', '1000000000000001', 'auth', 'Internal', 500, toDateTime64('2026-05-01 10:00:00.000000002', 9), 'Unset', '', '', '', map(), map('service.name', 'auth')),
    ('a0000000000000000000000000000001', '1000000000000003', '1000000000000001', 'checkout', 'Server', 700, toDateTime64('2026-05-01 10:00:00.000000003', 9), 'Error', '', '', '', map(), map('service.name', 'checkout')),
    ('a0000000000000000000000000000001', '1000000000000004', '1000000000000003', 'db', 'Client', 300, toDateTime64('2026-05-01 10:00:00.000000004', 9), 'Unset', '', '', '', map(), map('service.name', 'db'));`

func newTempoChDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, tracesDDL)
	c.Seed(t, tracesSeed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSearch_DrilldownStructureTab_ChDB(t *testing.T) {
	srv := newTempoChDBServer(t)

	// Verbatim StructureScene.tsx buildQuery output for the rate
	// metric with the Drilldown's root-span primary signal as the
	// filter expression.
	drilldownQuery := `({nestedSetParent<0} &>> { kind = server }) || ({nestedSetParent<0}) | select(status, resource.service.name, name, nestedSetParent, nestedSetLeft, nestedSetRight)`

	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(drilldownQuery) + "&limit=200&spss=20")
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
	tr := sr.Traces[0]
	if tr.RootServiceName != "frontend" || tr.RootTraceName != "GET /home" {
		t.Errorf("root metadata = (%q, %q), want (frontend, GET /home)", tr.RootServiceName, tr.RootTraceName)
	}
	if len(tr.SpanSets) != 1 {
		t.Fatalf("want exactly 1 spanset (mergeTraces throws otherwise), got %d", len(tr.SpanSets))
	}
	spans := tr.SpanSets[0].Spans
	if len(spans) != 2 {
		t.Fatalf("want 2 matched spans (root + server descendant), got %d (%s)", len(spans), body)
	}

	attr := func(sp tempo.SpanSetSpan, key string) (tempo.AnyValue, bool) {
		for _, kv := range sp.Attributes {
			if kv.Key == key {
				return kv.Value, true
			}
		}
		return tempo.AnyValue{}, false
	}
	wantInt := func(sp tempo.SpanSetSpan, key, want string) {
		t.Helper()
		av, ok := attr(sp, key)
		if !ok || av.IntValue == nil || *av.IntValue != want {
			t.Errorf("span %s attr %s = %+v, want intValue %q", sp.SpanID, key, av, want)
		}
	}
	wantStr := func(sp tempo.SpanSetSpan, key, want string) {
		t.Helper()
		av, ok := attr(sp, key)
		if !ok || av.StringValue == nil || *av.StringValue != want {
			t.Errorf("span %s attr %s = %+v, want stringValue %q", sp.SpanID, key, av, want)
		}
	}

	byID := map[string]tempo.SpanSetSpan{}
	for _, sp := range spans {
		byID[sp.SpanID] = sp
	}
	r1, ok := byID["1000000000000001"]
	if !ok {
		t.Fatalf("root span missing from spanset: %s", body)
	}
	c2, ok := byID["1000000000000003"]
	if !ok {
		t.Fatalf("server descendant missing from spanset: %s", body)
	}

	// Reference Tempo's numbering for this tree (DFS, counter from 1;
	// parent = parent's left bound; root parent = -1).
	wantInt(r1, "nestedSetParent", "-1")
	wantInt(r1, "nestedSetLeft", "1")
	wantInt(r1, "nestedSetRight", "8")
	wantStr(r1, "service.name", "frontend")
	wantStr(r1, "status", "unset")
	if r1.Name != "GET /home" {
		t.Errorf("root span name = %q, want GET /home (selected name populates tempopb.Span.Name)", r1.Name)
	}

	wantInt(c2, "nestedSetParent", "1")
	wantInt(c2, "nestedSetLeft", "4")
	wantInt(c2, "nestedSetRight", "7")
	wantStr(c2, "service.name", "checkout")
	wantStr(c2, "status", "error")
	if c2.Name != "checkout" {
		t.Errorf("descendant span name = %q, want checkout", c2.Name)
	}
}
