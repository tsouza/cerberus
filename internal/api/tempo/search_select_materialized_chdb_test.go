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
	"slices"
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
// toInt32OrNull(<map>[key]). ServiceName is the OTel-CH exporter's own
// column, derived here from the resource map the seeds write.
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
    ServiceName LowCardinality(String) DEFAULT ResourceAttributes['service.name'],
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
	return newMaterializedServer(t, materialized, materializedSelectSeed)
}

// newMaterializedServer serves /api/search and /api/metrics over the
// materialized table seeded with seed, with the default materialized
// attribute registry routed when materialized is true and the map-only
// schema otherwise.
func newMaterializedServer(t *testing.T, materialized bool, seed string) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, materializedSelectDDL)
	c.Seed(t, seed)
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

// materializedMalformedSeed is one trace whose spans carry a well-formed
// http.status_code ("500", twice), a malformed one ("x5") and none at all.
// The numeric materialized column holds toInt32OrNull of the cell, so the
// malformed value and the absent one are both NULL there, while the map
// keeps them apart: "x5" and "".
const materializedMalformedSeed = `INSERT INTO otel_traces (TraceId, SpanId, ParentSpanId, SpanName, SpanKind, Duration, Timestamp, StatusCode, SpanAttributes, ResourceAttributes) VALUES
    ('d0000000000000000000000000000001', '4000000000000001', '', 'root', 'Server', 3000, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', map('rpc.method', 'Root', 'http.status_code', '500'), map('service.name', 'gateway')),
    ('d0000000000000000000000000000001', '4000000000000002', '4000000000000001', 'child-a', 'Client', 1000, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', map('rpc.method', 'ChildA', 'http.status_code', 'x5'), map('service.name', 'gateway')),
    ('d0000000000000000000000000000001', '4000000000000003', '4000000000000001', 'child-b', 'Client', 1000, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', map('rpc.method', 'ChildB'), map('service.name', 'gateway')),
    ('d0000000000000000000000000000001', '4000000000000004', '4000000000000001', 'child-c', 'Client', 1000, toDateTime64('2026-05-01 11:00:00.000000004', 9), 'Unset', map('rpc.method', 'ChildC', 'http.status_code', '500'), map('service.name', 'gateway'));`

// metricsSeriesOf decodes a /api/metrics/query_range body into its series,
// keyed by label set.
func metricsSeriesOf(t *testing.T, body string) map[string][]float64 {
	t.Helper()
	var resp struct {
		Series []struct {
			Labels []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue *string `json:"stringValue"`
					IntValue    *string `json:"intValue"`
				} `json:"value"`
			} `json:"labels"`
			Samples []struct {
				Value float64 `json:"value"`
			} `json:"samples"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	out := map[string][]float64{}
	for _, s := range resp.Series {
		var labels []string
		for _, l := range s.Labels {
			v := "<untyped>"
			switch {
			case l.Value.StringValue != nil:
				v = *l.Value.StringValue
			case l.Value.IntValue != nil:
				v = "int:" + *l.Value.IntValue
			}
			labels = append(labels, l.Key+"="+v)
		}
		var values []float64
		for _, sm := range s.Samples {
			values = append(values, sm.Value)
		}
		out[strings.Join(labels, ",")] = values
	}
	return out
}

// TestMetricsByNumericMaterializedKey_ChDB groups a metrics query by the
// numeric materialized attribute (http.status_code) over spans carrying a
// well-formed value, a malformed one and none. A group key is an output, so
// it must be the attribute's own value: the malformed "x5" is its own series
// and the absent attribute is the "" series. Reading the Nullable(Int32)
// column instead turns "x5" into NULL and folds it into the "" series.
func TestMetricsByNumericMaterializedKey_ChDB(t *testing.T) {
	materialized := newMaterializedServer(t, true, materializedMalformedSeed)
	mapOnly := newMaterializedServer(t, false, materializedMalformedSeed)
	path := func(q string) string {
		return "/api/metrics/query_range?" + url.Values{
			"q": {q}, "start": {"1777633200"}, "end": {"1777636800"}, "step": {"3600s"},
		}.Encode()
	}
	for _, query := range []string{
		`{ } | count_over_time() by (span.http.status_code)`,
		`{ } | count_over_time() by (span.http.status_code, span.rpc.method)`,
		`{ kind = client } | count_over_time() by (span.http.status_code)`,
	} {
		t.Run(query, func(t *testing.T) {
			got := metricsSeriesOf(t, materializedAttributeBody(t, materialized, path(query)))
			want := metricsSeriesOf(t, materializedAttributeBody(t, mapOnly, path(query)))
			if !maps.EqualFunc(got, want, func(a, b []float64) bool { return slices.Equal(a, b) }) {
				t.Errorf("materialized series differ from the map-only series\n materialized: %v\n map-only:     %v", got, want)
			}
			malformed := false
			for labels := range got {
				malformed = malformed || strings.Contains(labels, "span.http.status_code=x5")
			}
			if !malformed {
				t.Errorf("no series for the malformed value x5: %v", got)
			}
		})
	}
	t.Run("series values", func(t *testing.T) {
		got := metricsSeriesOf(t, materializedAttributeBody(t, materialized, path(`{ } | count_over_time() by (span.http.status_code)`)))
		want := map[string][]float64{
			"span.http.status_code=500": {0, 2},
			"span.http.status_code=x5":  {0, 1},
			"span.http.status_code=":    {0, 1},
		}
		if !maps.EqualFunc(got, want, func(a, b []float64) bool { return slices.Equal(a, b) }) {
			t.Errorf("series = %v, want %v", got, want)
		}
	})
}

// TestMaterializedAttributeReadsPastClosedRows_ChDB reads a materialized
// attribute in each stage that can follow a structural operator: a metrics
// by() key, a metrics function's operand, a compare() selection, a search
// by() key that compares, and a spanset aggregate. The structural-join span
// envelope carries the attribute maps but no materialized column, so each
// read must go through the attribute map, and the answer must equal the
// map-only one.
func TestMaterializedAttributeReadsPastClosedRows_ChDB(t *testing.T) {
	materialized := newMaterializedServer(t, true, materializedSelectSeed)
	mapOnly := newMaterializedServer(t, false, materializedSelectSeed)
	metrics := func(q string) string {
		return "/api/metrics/query_range?" + url.Values{
			"q": {q}, "start": {"1777633200"}, "end": {"1777636800"}, "step": {"3600s"},
		}.Encode()
	}
	search := func(q string) string {
		return "/api/search?q=" + url.QueryEscape(q) + "&start=1777593600&end=1777680000&limit=20&spss=20"
	}
	for _, tc := range []struct {
		query   string
		path    func(string) string
		carries string // a grouped value, sample or matched trace the answer must hold
	}{
		{`{ } >> { } | rate() by (span.rpc.method)`, metrics, "ChildA"},
		{`{ kind = server } >> { kind = client } | count_over_time() by (span.http.status_code)`, metrics, "503"},
		{`{ kind = server } >> { kind = client } | max_over_time(span.http.status_code)`, metrics, `"value":503`},
		{`{ kind = server } >> { kind = client } | quantile_over_time(span.http.status_code, .5)`, metrics, `"samples"`},
		{`{ kind = server } >> { kind = client } | compare({ span.rpc.method = "ChildA" })`, metrics, "ChildA"},
		{`{ } >> { } | by(span.rpc.method = "ChildA")`, search, "c0000000000000000000000000000001"},
		{`{ } >> { } | by(span.http.status_code = 503)`, search, "c0000000000000000000000000000001"},
		{`{ kind = server } >> { kind = client } | max(span.http.status_code) > 500`, search, "c0000000000000000000000000000001"},
	} {
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

// TestSearchByMaterializedKey_ChDB groups a search by a bare materialized
// attribute: the unscoped spelling, whose span-then-resource read mixes the
// numeric column with the resource map, and a by() over the structural-join
// span envelope, which carries no materialized column. Each must answer
// exactly what the same data answers with materialization off.
func TestSearchByMaterializedKey_ChDB(t *testing.T) {
	materialized := newMaterializedServer(t, true, materializedMalformedSeed)
	mapOnly := newMaterializedServer(t, false, materializedMalformedSeed)
	for _, query := range []string{
		`{ } | by(.http.status_code)`,
		`{ } >> { } | by(span.rpc.method)`,
		`{ } >> { } | by(span.http.status_code)`,
	} {
		t.Run(query, func(t *testing.T) {
			path := "/api/search?q=" + url.QueryEscape(query) + "&start=1777593600&end=1777680000&limit=20&spss=20"
			got := materializedAttributeBody(t, materialized, path)
			if want := materializedAttributeBody(t, mapOnly, path); got != want {
				t.Errorf("materialized answer differs from the map-only answer\n materialized: %s\n map-only:     %s", got, want)
			}
			if !strings.Contains(got, "d0000000000000000000000000000001") {
				t.Errorf("answer does not carry the seeded trace: %s", got)
			}
		})
	}
}
