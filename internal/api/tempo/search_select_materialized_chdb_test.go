//go:build chdb

// chDB-backed pins for `| select(...)` of a MATERIALIZED trace attribute
// on /api/search, for a plain spanset and for every structural and set
// operator. A materialized attribute (schema.Traces.MaterializedSpanAttributeColumns
// / MaterializedResourceAttributeColumns) is read through its top-level
// `__cerberus_materialized_<key>` column, so every subquery between the
// table and the select() wrap projection must carry that column: the
// pruned Filter(Scan) column list and the structural / set-operation span
// envelope alike. The materialized deployment must answer exactly what the
// same data answers with materialization off.
package tempo_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// materializedSelectDDL is otel_traces with the default materialized
// attribute columns in their production shape (internal/schema/ddl's
// renderAddMaterializedAttrColumns): String keys as LowCardinality(String)
// DEFAULT <map>[key], http.status_code as Nullable(Int32) DEFAULT
// toInt32OrNull(<map>[key]).
const materializedSelectDDL = `CREATE TABLE otel_traces (
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
    ResourceAttributes Map(String, String),
    ` + "`__cerberus_materialized_rpc.method`" + ` LowCardinality(String) DEFAULT SpanAttributes['rpc.method'],
    ` + "`__cerberus_materialized_http.status_code`" + ` Nullable(Int32) DEFAULT toInt32OrNull(SpanAttributes['http.status_code']),
    ` + "`__cerberus_materialized_k8s.namespace.name`" + ` LowCardinality(String) DEFAULT ResourceAttributes['k8s.namespace.name']
) ENGINE = MergeTree() ORDER BY (Timestamp);`

// materializedSelectSeed is one trace: a Server root with two Client
// children, each span carrying distinct values for every materialized key,
// so a response that read the wrong span's value, or no value, is visible.
const materializedSelectSeed = `INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode, SpanAttributes, ResourceAttributes) VALUES
    ('c0000000000000000000000000000001', '3000000000000001', '', 'root', 'Server', 3000, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', map('rpc.method', 'Root', 'http.status_code', '200'), map('service.name', 'gateway', 'k8s.namespace.name', 'ns-root')),
    ('c0000000000000000000000000000001', '3000000000000002', '3000000000000001', 'child-a', 'Client', 1000, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', map('rpc.method', 'ChildA', 'http.status_code', '503'), map('service.name', 'gateway', 'k8s.namespace.name', 'ns-a')),
    ('c0000000000000000000000000000001', '3000000000000003', '3000000000000001', 'child-b', 'Client', 1000, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', map('rpc.method', 'ChildB', 'http.status_code', '404'), map('service.name', 'gateway', 'k8s.namespace.name', 'ns-b'));`

// newMaterializedSelectServer serves /api/search over the seeded table,
// with the default materialized attribute registry routed when
// materialized is true and the map-only schema otherwise.
func newMaterializedSelectServer(t *testing.T, materialized bool) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, materializedSelectDDL)
	c.Seed(t, materializedSelectSeed)
	s := schema.DefaultOTelTraces()
	if materialized {
		s.MaterializedSpanAttributeColumns = schema.DefaultMaterializedSpanAttributeColumns()
		s.MaterializedResourceAttributeColumns = schema.DefaultMaterializedResourceAttributeColumns()
	}
	h := tempo.New(c, s, "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// selectedValuesBySpan runs query on /api/search and returns, per matched
// span ID, the value of the selected attribute key. A span that does not
// carry key maps to "<missing>"; a non-200 answer fails the test.
func selectedValuesBySpan(t *testing.T, srv *httptest.Server, query, key string) map[string]string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(query) + "&start=1777593600&end=1777680000&limit=20&spss=20")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", query, resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	out := map[string]string{}
	for _, tr := range sr.Traces {
		for _, ss := range tr.SpanSets {
			for _, sp := range ss.Spans {
				out[sp.SpanID] = "<missing>"
				for _, kv := range sp.Attributes {
					if kv.Key != key {
						continue
					}
					switch {
					case kv.Value.StringValue != nil:
						out[sp.SpanID] = *kv.Value.StringValue
					case kv.Value.IntValue != nil:
						out[sp.SpanID] = "int:" + *kv.Value.IntValue
					default:
						out[sp.SpanID] = "<untyped>"
					}
				}
			}
		}
	}
	return out
}

// Span IDs as /api/search renders them (leading hex zeros stripped).
const (
	matRootSpan   = "3000000000000001"
	matChildASpan = "3000000000000002"
	matChildBSpan = "3000000000000003"
)

// TestSearch_SelectMaterializedAttribute_ChDB selects each default
// materialized attribute (two String-typed, one Nullable(Int32)) under a
// plain spanset and under every structural and set operator. Each answer
// must carry the selected attribute's values on the expected spans and be
// identical to the answer the same data gives with materialization off.
func TestSearch_SelectMaterializedAttribute_ChDB(t *testing.T) {
	materialized := newMaterializedSelectServer(t, true)
	mapOnly := newMaterializedSelectServer(t, false)

	selects := []struct {
		attr string
		key  string
		want map[string]string
	}{
		{"span.rpc.method", "rpc.method", map[string]string{matRootSpan: "Root", matChildASpan: "ChildA", matChildBSpan: "ChildB"}},
		{"span.http.status_code", "http.status_code", map[string]string{matRootSpan: "200", matChildASpan: "503", matChildBSpan: "404"}},
		{"resource.k8s.namespace.name", "k8s.namespace.name", map[string]string{matRootSpan: "ns-root", matChildASpan: "ns-a", matChildBSpan: "ns-b"}},
	}
	children := []string{matChildASpan, matChildBSpan}
	both := []string{matRootSpan, matChildASpan, matChildBSpan}
	spansets := []struct {
		spanset string
		spans   []string
	}{
		{`{ kind = server }`, []string{matRootSpan}},
		{`{ kind = server } > { kind = client }`, children},
		{`{ kind = server } >> { kind = client }`, children},
		{`{ kind = client } < { kind = server }`, []string{matRootSpan}},
		{`{ kind = client } << { kind = server }`, []string{matRootSpan}},
		{`{ span.rpc.method = "ChildA" } ~ { kind = client }`, []string{matChildBSpan}},
		{`{ kind = server } !> { kind = server }`, []string{matRootSpan}},
		{`{ kind = server } !< { kind = client }`, children},
		{`{ kind = server } !~ { kind = client }`, children},
		{`{ kind = client } !>> { kind = client }`, children},
		{`{ kind = client } !<< { kind = client }`, children},
		{`{ kind = server } &> { kind = client }`, both},
		{`{ kind = client } &< { kind = server }`, both},
		{`{ span.rpc.method = "ChildA" } &~ { kind = client }`, children},
		{`{ kind = server } &>> { kind = client }`, both},
		{`{ kind = client } &<< { kind = server }`, both},
		{`{ kind = server } && { kind = client }`, both},
		{`{ kind = server } || { kind = client }`, both},
	}
	for _, sel := range selects {
		for _, ss := range spansets {
			query := ss.spanset + ` | select(` + sel.attr + `)`
			t.Run(query, func(t *testing.T) {
				got := selectedValuesBySpan(t, materialized, query, sel.key)
				want := map[string]string{}
				for _, span := range ss.spans {
					want[span] = sel.want[span]
				}
				if !maps.Equal(got, want) {
					t.Errorf("materialized answer = %v, want %v", got, want)
				}
				if ref := selectedValuesBySpan(t, mapOnly, query, sel.key); !maps.Equal(got, ref) {
					t.Errorf("materialized answer %v differs from the map-only answer %v", got, ref)
				}
			})
		}
	}
}

// materializedAttributeBody runs path (a request path with its query
// string, relative to srv) and returns the 200 body; any other status
// fails the test.
func materializedAttributeBody(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", path, resp.StatusCode, body)
	}
	return body
}

// TestMaterializedAttributeReadsPastPrunedScan_ChDB covers the other plan
// stages a pruned scan feeds, each narrowed by its own ProjectionPushdown
// path: a select() straight over the table, a select() over a trace-scoped
// lookup, a select() over the structural and set-operation shapes Grafana's
// Traces Drilldown structure tab sends (nested-set annotated), a search
// by(), and a metrics by(). Each reads a materialized attribute and must
// answer exactly what the same data answers with materialization off.
func TestMaterializedAttributeReadsPastPrunedScan_ChDB(t *testing.T) {
	materialized := newMaterializedSelectServer(t, true)
	mapOnly := newMaterializedSelectServer(t, false)

	search := func(q string) string {
		return "/api/search?q=" + url.QueryEscape(q) + "&start=1777593600&end=1777680000&limit=20&spss=20"
	}
	metrics := func(q string) string {
		return "/api/metrics/query_range?" + url.Values{
			"q": {q}, "start": {"1777593600"}, "end": {"1777680000"}, "step": {"3600s"},
		}.Encode()
	}
	cases := []struct {
		query   string
		path    func(string) string
		carries string // a selected or grouped value the answer must hold
	}{
		{`{ } | select(span.rpc.method)`, search, "ChildB"},
		{`{ rootName = "root" } | select(span.rpc.method)`, search, "ChildA"},
		{`{ kind = server } >> { kind = client } | select(span.rpc.method, nestedSetParent)`, search, "ChildA"},
		{`({ kind = server } &>> { }) || ({ kind = server }) | select(resource.k8s.namespace.name, nestedSetParent)`, search, "ns-b"},
		// A by() search answers no spanSets yet (#4316), so the pin is the
		// matched trace itself.
		{`{ kind = client } | by(span.rpc.method)`, search, "c0000000000000000000000000000001"},
		{`{ } | rate() by (span.rpc.method)`, metrics, "Root"},
		{`{ } | count_over_time() by (resource.k8s.namespace.name)`, metrics, "ns-a"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := materializedAttributeBody(t, materialized, tc.path(tc.query))
			if want := materializedAttributeBody(t, mapOnly, tc.path(tc.query)); got != want {
				t.Errorf("materialized answer differs from the map-only answer\n materialized: %s\n map-only:     %s", got, want)
			}
			if !strings.Contains(got, tc.carries) {
				t.Errorf("answer does not carry %q: %s", tc.carries, got)
			}
		})
	}
}
