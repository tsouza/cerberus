//go:build chdb

// chDB-backed proof of cerberus issue #3065 item 0 (the issue's highest
// priority item, filed ahead of compare()/tag-discovery specifically
// because it blocks the /api/search baseline endpoint rather than an
// advanced operator): canonicalSampleProjections / sampleProjectionsWithSelected
// (handler.go) unconditionally build every /api/search response's
// Attributes column via
//
//	mapConcat(ResourceAttributes, map('__cerberus_traceID', ..., ...))
//
// — a chplan.FuncCall{Fn: chplan.FnMapMerge, ...} whose first argument is a
// bare *chplan.ColumnRef. cerberus issue #3063's PR (#3071) generalised
// internal/chsql/attr_strategy_fullmap.go's bounded-depth JSON
// reconstruction into exprFunc's dispatch for EVERY chplan tree using one
// of the jsonFullMapFns identifiers (FnMapMerge included), not merely
// LogQL's own call sites — so unlike item 1
// (metrics_compare_json_chdb_test.go, which needed a real fix:
// metricsLang was missing EmitAttrStrategies entirely), /api/search routes
// through traceqlLang, which already implements EmitAttrStrategies
// (cerberus issue #3062) — so this file is the empirical proof that item 0
// needs NO further code change, only this test, once #3071's generic
// chsql-layer substitution is in place.
//
// attr_strategy_json_chdb_test.go's own package doc explicitly named this
// gap and deliberately kept ResourceAttributes Map-typed in every table it
// seeds "even the JSON variant". Resolved in this change: this file flips
// ResourceAttributes (not SpanAttributes) to JSON and re-runs /api/search
// end-to-end.
package tempo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// searchBaselineTracesDDL renders an otel_traces-shaped table with an
// independent ResourceAttributes type — the column canonicalSampleProjections'
// mapConcat/FnMapMerge always reads in full for every /api/search response,
// regardless of the query's own filter shape.
func searchBaselineTracesDDL(table, resourceAttrType string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
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
    ResourceAttributes %s
) ENGINE = MergeTree() ORDER BY (Timestamp);`, table, resourceAttrType)
}

// searchBaselineSeed seeds one single-span trace whose ResourceAttributes
// carries two generic keys — service.name (also read via the well-known
// synthesis elsewhere in the package, here just an ordinary generic key)
// and a second, arbitrary key — so the reconstructed map must surface BOTH
// alongside the synthetic __cerberus_traceID / __cerberus_parentSpanID /
// __cerberus_spanID entries mapConcat/FnMapMerge always adds.
func searchBaselineSeed(table, resourceAttrLit string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('f0000000000000000000000000000001', '6000000000000001', '', 'checkout', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', map(), %s);`,
		table, resourceAttrLit)
}

// newSearchBaselineServer mirrors attr_strategy_json_chdb_test.go's
// newAttrStrategyChDBServer, kept as a distinct helper in this file since
// it flips ResourceAttributes rather than SpanAttributes.
func newSearchBaselineServer(t *testing.T, c *chclienttest.Client, table, ddl, seed string, attrStrategies chsql.AttrStrategies) *httptest.Server {
	t.Helper()
	c.Seed(t, ddl)
	c.Seed(t, seed)
	s := schema.DefaultOTelTraces()
	s.SpansTable = table
	h := tempo.New(c, s, "v-test", nil)
	h.SetAttrStrategies(attrStrategies)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSearch_JSONResourceAttributes_BaselineResponse_ChDB runs a
// match-everything /api/search (`q={}`, no filter beyond the time window)
// against a Map-typed
// control table and a JSON-typed (ResourceAttributes) table seeded
// identically, and asserts both surface the SAME TraceID/SpanID —
// populated via the SAME mergedAttrs mapConcat(ResourceAttributes, ...)
// FnMapMerge this file targets, through its `__cerberus_traceID` /
// `__cerberus_spanID` reserved-key extraction (toTraceSummaries /
// observeSpan; observeSpan DROPS a row outright when
// `__cerberus_spanID` fails to extract, so a wrong/absent reconstruction
// would silently shrink the response rather than merely mis-render one
// attribute value). A query-time TYPE_MISMATCH / ILLEGAL_TYPE_OF_ARGUMENT
// against the JSON-typed column — item 0's pre-#3071 failure mode — would
// instead surface as a 500 for every query regardless of filter shape,
// which searchBaselineIDs' status check catches directly.
//
// A second case adds `| select(resource.deploy.env)` to prove the
// generic ResourceAttributes content actually reaches the wire too (via
// selectedAttrKVPairs' `__cerberus_sel_str_*` reserved-key convention,
// itself folded into the SAME mergedAttrs map) — not merely the three
// fixed reserved keys every response carries regardless of content.
func TestSearch_JSONResourceAttributes_BaselineResponse_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "search_baseline_resattr_map", "search_baseline_resattr_json"
	strategies := chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}

	mapSrv := newSearchBaselineServer(t, c, mapTable,
		searchBaselineTracesDDL(mapTable, "Map(String, String)"),
		searchBaselineSeed(mapTable, "map('service.name', 'checkout-svc', 'deploy.env', 'prod')"), nil)
	jsonSrv := newSearchBaselineServer(t, c, jsonTable,
		searchBaselineTracesDDL(jsonTable, "JSON"),
		searchBaselineSeed(jsonTable, `'{"service.name":"checkout-svc","deploy.env":"prod"}'`), strategies)

	const wantTraceID = "f0000000000000000000000000000001"
	const wantSpanID = "6000000000000001"

	t.Run("bare_query_reserved_keys", func(t *testing.T) {
		const bareQuery = `{}`
		mapID, mapSpan := searchBaselineIDs(t, mapSrv, bareQuery)
		if mapID != wantTraceID || mapSpan != wantSpanID {
			t.Fatalf("Map /api/search TraceID/SpanID = %q/%q, want %q/%q (test's own expectation, not just Map==JSON)",
				mapID, mapSpan, wantTraceID, wantSpanID)
		}
		jsonID, jsonSpan := searchBaselineIDs(t, jsonSrv, bareQuery)
		if jsonID != wantTraceID || jsonSpan != wantSpanID {
			t.Errorf("JSON /api/search TraceID/SpanID = %q/%q, want %q/%q — canonicalSampleProjections' "+
				"mapConcat(ResourceAttributes, ...) must reconstruct the __cerberus_traceID/__cerberus_spanID "+
				"reserved keys exactly like Map", jsonID, jsonSpan, wantTraceID, wantSpanID)
		}
	})

	t.Run("select_generic_resource_attribute", func(t *testing.T) {
		q := `{} | select(resource.deploy.env)`
		mapAttrs := searchBaselineSelectedAttrs(t, mapSrv, q)
		if mapAttrs["deploy.env"] != "prod" {
			t.Fatalf("Map /api/search select(resource.deploy.env) = %v, want deploy.env=prod (test's own expectation, not just Map==JSON)", mapAttrs)
		}
		jsonAttrs := searchBaselineSelectedAttrs(t, jsonSrv, q)
		if jsonAttrs["deploy.env"] != "prod" {
			t.Errorf("JSON /api/search select(resource.deploy.env) = %v, want deploy.env=prod — the generic "+
				"ResourceAttributes content must reach the wire through the JSON reconstruction too", jsonAttrs)
		}
	})
}

// searchBaselineIDs issues /api/search with query q and returns the
// single expected trace's TraceID and its single span's SpanID. Fails the
// test outright on a non-200 or an unexpected trace/span count, since a
// TYPE_MISMATCH / ILLEGAL_TYPE_OF_ARGUMENT failure surfaces as a 5xx with
// no IDs to compare at all, and observeSpan silently drops a row whose
// __cerberus_spanID extraction failed rather than erroring.
func searchBaselineIDs(t *testing.T, srv *httptest.Server, q string) (traceID, spanID string) {
	t.Helper()
	reqURL := srv.URL + "/api/search?q=" + url.QueryEscape(q) + "&" + jsonAttrSearchWindowQS
	resp, err := http.Get(reqURL)
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
		t.Fatalf("expected exactly 1 trace, got %d: %s", len(sr.Traces), body)
	}
	tr := sr.Traces[0]
	var spans []tempo.SpanSetSpan
	for _, ss := range tr.SpanSets {
		spans = append(spans, ss.Spans...)
	}
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d: %s", len(spans), body)
	}
	return tr.TraceID, spans[0].SpanID
}

// searchBaselineSelectedAttrs issues /api/search with query q and returns
// the flattened `| select(...)` Attributes of the single expected span.
func searchBaselineSelectedAttrs(t *testing.T, srv *httptest.Server, q string) map[string]string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(q) + "&" + jsonAttrSearchWindowQS)
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
	var spans []tempo.SpanSetSpan
	for _, tr := range sr.Traces {
		for _, ss := range tr.SpanSets {
			spans = append(spans, ss.Spans...)
		}
	}
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d: %s", len(spans), body)
	}
	out := map[string]string{}
	for _, kv := range spans[0].Attributes {
		if kv.Value.StringValue != nil {
			out[kv.Key] = *kv.Value.StringValue
		}
	}
	return out
}
