//go:build chdb

// chDB-backed coverage of every Tempo statement the handler emits on its own
// — outside the engine's Lang-driven threading — against a table whose
// SpanAttributes and ResourceAttributes are both JSON-typed: the metrics
// exemplars, the histogram and scalar metrics matrices, the missing-root
// lookup, and the tag-discovery lookups the gRPC service shares. Each one
// renders a Map subscript, which ClickHouse rejects on a JSON column
// (ILLEGAL_TYPE_OF_ARGUMENT), unless the request's traces strategies reach
// it; the exemplars and the root lookup degrade silently rather than fail
// the request, so their answers are asserted, not only the status.

package tempo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// threadingTable is the JSON-typed spans table this file seeds.
const threadingTable = "strategies_threading_json"

// threadingJSONStrategies resolves both attribute maps to JSON, as the boot
// probe resolves them for threadingTable.
var threadingJSONStrategies = chsql.SchemaStrategies{Attrs: chsql.AttrStrategies{
	"SpanAttributes":     chsql.AttrStrategyJSON,
	"ResourceAttributes": chsql.AttrStrategyJSON,
}}

// The seed's window: every span sits in [threadingStart, threadingEnd).
var (
	threadingStart = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	threadingEnd   = threadingStart.Add(3 * time.Minute)
)

// threadingSeed seeds two traces. Trace a has a root span r (service
// frontend, no custom_tag) and a child c (service backend,
// custom_tag=checkout); trace b is a single root span s (service backend,
// custom_tag=checkout). `{ span.custom_tag = "checkout" }` matches c and s,
// so trace a's root is not in the result and the handler looks it up.
func threadingSeed() string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('a0000000000000000000000000000001', '1000000000000001', '', 'root', 'Server', 400000000, toDateTime64('2026-05-12 10:00:10.000000000', 9), 'Unset', '', '', '', '{}', '{"service.name":"frontend"}'),
    ('a0000000000000000000000000000001', '1000000000000002', '1000000000000001', 'child', 'Client', 200000000, toDateTime64('2026-05-12 10:00:20.000000000', 9), 'Unset', '', '', '', '{"custom_tag":"checkout"}', '{"service.name":"backend"}'),
    ('b0000000000000000000000000000002', '2000000000000001', '', 'solo', 'Server', 100000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), 'Unset', '', '', '', '{"custom_tag":"checkout"}', '{"service.name":"backend"}');`,
		threadingTable)
}

// newThreadingHandler seeds threadingTable and returns a handler over it
// whose cell holds strategies.
func newThreadingHandler(t *testing.T, strategies chsql.SchemaStrategies) *tempo.Handler {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, jsonAttrTracesDDL(threadingTable, "JSON", "JSON"))
	c.Seed(t, threadingSeed())
	s := schema.DefaultOTelTraces()
	s.SpansTable = threadingTable
	h := tempo.New(c, s, "v-test", nil)
	h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{Traces: strategies})
	return h
}

// serve mounts h on a test server.
func serve(t *testing.T, h *tempo.Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// getOK GETs u and returns the body, failing the test on a non-200.
func getOK(t *testing.T, u string) string {
	t.Helper()
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

// threadingMetrics runs a metrics query_range over the seed window.
func threadingMetrics(t *testing.T, srv *httptest.Server, q string) tempo.MetricsQueryRangeResponse {
	t.Helper()
	body := getOK(t, metricsQueryRangeURL(srv.URL, q, map[string]string{
		"start": fmt.Sprint(threadingStart.Unix()),
		"end":   fmt.Sprint(threadingEnd.Unix()),
		"step":  "60s",
	}))
	var out tempo.MetricsQueryRangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	return out
}

// TestJSONAttrs_MetricsRateExemplars_ChDB pins that a TraceQL metrics range
// query over JSON-typed attributes answers its matrix and attaches its
// exemplars: the matrix statement (runMetricsWindow) and the exemplars
// statement (attachMetricsExemplars) both read span.custom_tag.
func TestJSONAttrs_MetricsRateExemplars_ChDB(t *testing.T) {
	srv := serve(t, newThreadingHandler(t, threadingJSONStrategies))
	out := threadingMetrics(t, srv, `{ span.custom_tag = "checkout" } | count_over_time() by (span.custom_tag)`)
	if len(out.Series) != 1 {
		t.Fatalf("series = %+v, want one span.custom_tag=checkout series", out.Series)
	}
	var total float64
	for _, s := range out.Series[0].Samples {
		total += s.Value
	}
	if total != 2 {
		t.Errorf("count_over_time total = %v, want 2 (spans c and s)", total)
	}
	var spanIDs []string
	for _, ex := range out.Series[0].Exemplars {
		spanIDs = append(spanIDs, ex.SpanID)
	}
	slices.Sort(spanIDs)
	if want := []string{"1000000000000002", "2000000000000001"}; !slices.Equal(spanIDs, want) {
		t.Errorf("exemplar span IDs = %v, want %v", spanIDs, want)
	}
}

// TestJSONAttrs_MetricsHistogram_ChDB pins the histogram metrics matrix
// (execMetricsRangeHistogram) over a JSON-typed attribute filter.
func TestJSONAttrs_MetricsHistogram_ChDB(t *testing.T) {
	srv := serve(t, newThreadingHandler(t, threadingJSONStrategies))
	out := threadingMetrics(t, srv, `{ span.custom_tag = "checkout" } | histogram_over_time(duration)`)
	var total float64
	for _, s := range out.Series {
		for _, sm := range s.Samples {
			total += sm.Value
		}
	}
	if total != 2 {
		t.Errorf("histogram_over_time total count = %v over %d series, want 2 (spans c and s)", total, len(out.Series))
	}
}

// TestJSONAttrs_SearchResolvesMissingRoot_ChDB pins the missing-root lookup
// (resolveTraceRoots) over a JSON-typed ResourceAttributes: trace a's
// matched span is its child, so the root's service — frontend, read from
// ResourceAttributes by the lookup — names the trace.
func TestJSONAttrs_SearchResolvesMissingRoot_ChDB(t *testing.T) {
	srv := serve(t, newThreadingHandler(t, threadingJSONStrategies))
	body := getOK(t, srv.URL+"/api/search?"+url.Values{
		"q":     {`{ span.custom_tag = "checkout" }`},
		"start": {fmt.Sprint(threadingStart.Unix())},
		"end":   {fmt.Sprint(threadingEnd.Unix())},
		"limit": {"20"},
	}.Encode())
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	roots := map[string]string{}
	for _, tr := range sr.Traces {
		roots[tr.TraceID] = tr.RootServiceName + "/" + tr.RootTraceName
	}
	want := map[string]string{
		"a0000000000000000000000000000001": "frontend/root",
		"b0000000000000000000000000000002": "backend/solo",
	}
	if len(roots) != len(want) || roots["a0000000000000000000000000000001"] != want["a0000000000000000000000000000001"] ||
		roots["b0000000000000000000000000000002"] != want["b0000000000000000000000000000002"] {
		t.Errorf("root service/name per trace = %v, want %v", roots, want)
	}
}

// TestJSONAttrs_TagDiscoveryExports_ChDB pins the tag-discovery lookups the
// gRPC service reaches through the exported Handler methods, over JSON-typed
// attributes: the attribute tag-name scan, the attribute tag-value scan,
// and an intrinsic tag-value scan narrowed by a span-attribute query.
func TestJSONAttrs_TagDiscoveryExports_ChDB(t *testing.T) {
	h := newThreadingHandler(t, threadingJSONStrategies)
	ctx := h.WithRequestStrategies(context.Background())

	scopes, err := h.CollectAttributeTagScopes(ctx, "span", tempo.TagsRouteV2, "", threadingStart, threadingEnd)
	if err != nil {
		t.Fatalf("CollectAttributeTagScopes: %v", err)
	}
	var spanTags []string
	for _, s := range scopes {
		spanTags = append(spanTags, s.Tags...)
	}
	if !slices.Contains(spanTags, "custom_tag") {
		t.Errorf("span tag names = %v, want custom_tag", spanTags)
	}

	attr, err := h.ResolveTagName("span.custom_tag")
	if err != nil {
		t.Fatalf("ResolveTagName: %v", err)
	}
	values, _, err := h.FetchTagValues(ctx, attr, tempo.TagsRouteV2, "", threadingStart, threadingEnd)
	if err != nil {
		t.Fatalf("FetchTagValues(span.custom_tag): %v", err)
	}
	if !slices.Equal(values, []string{"checkout"}) {
		t.Errorf("span.custom_tag values = %v, want [checkout]", values)
	}

	name, err := h.ResolveTagName("name")
	if err != nil {
		t.Fatalf("ResolveTagName: %v", err)
	}
	names, _, err := h.FetchTagValues(ctx, name, tempo.TagsRouteV2, `{ span.custom_tag = "checkout" }`, threadingStart, threadingEnd)
	if err != nil {
		t.Fatalf("FetchTagValues(name, q): %v", err)
	}
	if !slices.Equal(names, []string{"child", "solo"}) {
		t.Errorf("span names narrowed by span.custom_tag = %v, want [child solo]", names)
	}
}

// TestJSONAttrs_HTTPIntrinsicTagValuesNarrowedByQuery_ChDB pins the HTTP
// twin of the intrinsic tag-value scan (handleSearchTagValuesV2), whose
// narrowing filter reads a JSON-typed span attribute.
func TestJSONAttrs_HTTPIntrinsicTagValuesNarrowedByQuery_ChDB(t *testing.T) {
	srv := serve(t, newThreadingHandler(t, threadingJSONStrategies))
	body := getOK(t, srv.URL+"/api/v2/search/tag/name/values?"+url.Values{
		"q":     {`{ span.custom_tag = "checkout" }`},
		"start": {fmt.Sprint(threadingStart.Unix())},
		"end":   {fmt.Sprint(threadingEnd.Unix())},
	}.Encode())
	var out struct {
		TagValues []struct {
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	var names []string
	for _, v := range out.TagValues {
		names = append(names, v.Value)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"child", "solo"}) {
		t.Errorf("span names narrowed by span.custom_tag = %v, want [child solo]", names)
	}
}

// TestJSONAttrs_StoreAfterServingReachesNextRequest_ChDB pins that the
// handler reads the strategies cell per request rather than once: a cell
// that held the Map strategies while a request was served, then had the
// JSON strategies stored — the schema re-probe's late Store — renders the
// next request for JSON, on the search path (the long-lived TraceQL Lang)
// and on the metrics path alike.
func TestJSONAttrs_StoreAfterServingReachesNextRequest_ChDB(t *testing.T) {
	window := map[string]string{
		"start": fmt.Sprint(threadingStart.Unix()),
		"end":   fmt.Sprint(threadingEnd.Unix()),
		"step":  "60s",
	}
	search := func(base string) string {
		return base + "/api/search?" + url.Values{
			"q":     {`{ span.custom_tag = "checkout" }`},
			"start": {window["start"]},
			"end":   {window["end"]},
		}.Encode()
	}
	metrics := func(base string) string {
		return metricsQueryRangeURL(base, `{ span.custom_tag = "checkout" } | count_over_time()`, window)
	}
	for name, route := range map[string]func(string) string{"search": search, "metrics": metrics} {
		t.Run(name, func(t *testing.T) {
			h := newThreadingHandler(t, chsql.SchemaStrategies{})
			srv := serve(t, h)
			resp, err := http.Get(route(srv.URL))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if body := readBody(t, resp); resp.StatusCode == http.StatusOK {
				t.Fatalf("Map strategies over a JSON table answered 200 (%s): the control cannot tell the stored value apart", body)
			}
			h.Strategies.Store(chsql.ResolvedSchemaStrategies{Traces: threadingJSONStrategies})
			getOK(t, route(srv.URL))
		})
	}
}
