//go:build chdb

// chDB-backed proof of cerberus issue #3065 item 1: does `| compare()`'s
// generic attribute fan-out (compareAttrPairsExpr's `generic()` closure in
// internal/traceql/metrics_compare.go, which builds
// arrayZip(mapKeys(<col>), mapValues(<col>)) then arrayFilter/arrayMap over
// it) work against a JSON-typed SpanAttributes column?
//
// The issue names this as a standing gap: "the compare() spanset-metrics
// operator's well-known/generic attribute fan-out reads the WHOLE attribute
// map at once via mapKeys/mapValues/arrayZip/arrayFilter to project every
// attribute as a labeled column ... against a JSON-typed column this still
// fails at query time with ILLEGAL_TYPE_OF_ARGUMENT". That gap existed
// before cerberus issue #3063's PR (#3071) generalised
// internal/chsql/attr_strategy_fullmap.go's bounded-depth reconstruction:
// exprFunc now substitutes ANY bare JSON-strategy chplan.ColumnRef argument
// to chplan.FnMapKeys / FnMapValues (jsonFullMapFns) with
// jsonFullMapReconstruction(col) before the normal Fn-name resolution runs
// — a mechanism generic over every chplan tree, not specific to LogQL's own
// full-map call sites. compareAttrPairsExpr's generic() closure already
// builds its fan-out out of exactly those two Fn identifiers
// (chplan.FnMapKeys / chplan.FnMapValues) applied to a bare
// *chplan.ColumnRef, so #3065's item 1 needs NO code change in
// internal/traceql/metrics_compare.go — this file is the empirical proof
// that the shared #3071 substitution already closes it, run against
// compare()'s real emitted SQL end-to-end (parse -> lower -> emit ->
// ClickHouse -> BaselineAggregator-style post-processing), not merely
// re-derived by reading exprFunc's dispatch order.
package tempo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// compareTracesDDL renders an otel_traces-shaped table with an independent
// SpanAttributes type, matching the columns compareAttrPairsExpr and the
// compare() lowering read (status/kind/name intrinsics, Duration,
// Timestamp, the two attribute maps).
func compareTracesDDL(table, spanAttrType string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String,
    ServiceName String,
    Duration Int64,
    Timestamp DateTime64(9),
    SpanAttributes %s,
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY (Timestamp);`, table, spanAttrType)
}

// compareSeed seeds three single-span traces exercising the compare()
// selection/baseline split (`{status = error}`) plus a generic (not in
// wellKnownSpanAttrs) SpanAttributes key so the test proves the GENERIC
// mapKeys/mapValues fan-out — not the well-known dedicated-column path,
// which #3062's own attr_strategy_json_chdb_test.go already covers via
// FieldAccess/FnMapContainsKey — reconstructs correctly against JSON:
//
//	s1 status=Error   custom_tag="checkout"  -> selection cohort
//	s2 status=Unset   custom_tag="checkout"  -> baseline cohort
//	s3 status=Unset   custom_tag absent      -> baseline cohort, attr absent
func compareSeed(table, presentAttrLit, absentAttrLit string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('e0000000000000000000000000000001', '5000000000000001', '', 'checkout', 'Server', 'Error', '', '', '', 'svc', 200000000, toDateTime64('2026-05-12 10:01:00.000000000', 9), %s, map()),
    ('e0000000000000000000000000000002', '5000000000000002', '', 'checkout', 'Server', 'Unset', '', '', '', 'svc', 150000000, toDateTime64('2026-05-12 10:01:30.000000000', 9), %s, map()),
    ('e0000000000000000000000000000003', '5000000000000003', '', 'other', 'Server', 'Unset', '', '', '', 'svc', 100000000, toDateTime64('2026-05-12 10:02:00.000000000', 9), %s, map());`,
		table, presentAttrLit, presentAttrLit, absentAttrLit)
}

// compareServer builds a tempo.Handler over a compare-shaped otel_traces
// table with attrStrategies wired the same way
// attr_strategy_json_chdb_test.go's newAttrStrategyChDBServer does.
func compareServer(t *testing.T, c *chclienttest.Client, table, spanAttrType, presentAttrLit, absentAttrLit string, strategies chsql.AttrStrategies) *httptest.Server {
	t.Helper()
	c.Seed(t, compareTracesDDL(table, spanAttrType))
	c.Seed(t, compareSeed(table, presentAttrLit, absentAttrLit))
	s := schema.DefaultOTelTraces()
	s.SpansTable = table
	h := tempo.New(c, s, "v-test", nil)
	h.SetAttrStrategies(strategies)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestTraceQL_JSONAttrStrategy_CompareGenericFanOut_ChDB runs
// `{} | compare({status = error})` against a Map-typed control table and a
// JSON-typed (SpanAttributes) table seeded identically, and asserts both
// surface the SAME generic `span.custom_tag` series in both the selection
// and baseline cohorts — proving compareAttrPairsExpr's generic()
// mapKeys/mapValues/arrayZip/arrayFilter fan-out reconstructs a JSON-typed
// column's full attribute set exactly like a genuine Map column, with no
// query-time ILLEGAL_TYPE_OF_ARGUMENT.
func TestTraceQL_JSONAttrStrategy_CompareGenericFanOut_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "attr_strategy_compare_map", "attr_strategy_compare_json"
	strategies := chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON}

	mapSrv := compareServer(t, c, mapTable, "Map(String, String)",
		"map('custom_tag', 'checkout')", "map()", nil)
	jsonSrv := compareServer(t, c, jsonTable, "JSON",
		`'{"custom_tag":"checkout"}'`, `'{}'`, strategies)

	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 12, 10, 3, 0, 0, time.UTC)
	params := map[string]string{
		"start": fmt.Sprintf("%d", start.Unix()),
		"end":   fmt.Sprintf("%d", end.Unix()),
		"step":  "180s",
	}

	mapGot := customTagSeries(t, mapSrv, metricsQueryRangeURL(mapSrv.URL, `{} | compare({status = error}, 10)`, params))
	jsonGot := customTagSeries(t, jsonSrv, metricsQueryRangeURL(jsonSrv.URL, `{} | compare({status = error}, 10)`, params))

	want := map[string]float64{
		"selection:checkout": 1,
		"baseline:checkout":  1,
	}
	if !floatMapEq(mapGot, want) {
		t.Fatalf("Map compare() custom_tag series = %v, want %v (test's own expectation, not just Map==JSON)", mapGot, want)
	}
	if !floatMapEq(jsonGot, want) {
		t.Errorf("JSON compare() custom_tag series = %v, want %v — compareAttrPairsExpr's generic "+
			"mapKeys/mapValues fan-out must reconstruct a JSON-typed column exactly like Map", jsonGot, want)
	}
}

// customTagSeries issues the compare() query and returns, for every series
// carrying a "span.custom_tag" label, a "<meta>:<value>" -> total-count map
// (summed across the zero-filled grid) — the shape the test compares
// between the Map and JSON variants.
func customTagSeries(t *testing.T, srv *httptest.Server, u string) map[string]float64 {
	t.Helper()
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
	got := map[string]float64{}
	for _, s := range out.Series {
		var meta, attrVal string
		hasCustomTag := false
		for _, l := range s.Labels {
			switch l.Key {
			case "__meta_type":
				meta = l.Value
			case "span.custom_tag":
				hasCustomTag = true
				attrVal = l.Value
			}
		}
		if !hasCustomTag || (meta != "selection" && meta != "baseline") {
			continue
		}
		var total float64
		for _, sm := range s.Samples {
			total += sm.Value
		}
		got[meta+":"+attrVal] = total
	}
	return got
}

func floatMapEq(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
