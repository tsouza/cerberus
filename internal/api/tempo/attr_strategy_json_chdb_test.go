//go:build chdb

// chDB-backed proof of cerberus issue #3062's two open questions, which
// #2777's own PR body flagged as TraceQL-specific risk beyond what the
// LogQL differentials (internal/chsql/attr_strategy_json_chdb_test.go)
// covered:
//
//  1. Comparison typing — does a JSON dynamic-subcolumn read compose
//     correctly with TraceQL's numeric coercion (coerceFieldAccess's
//     toFloat64OrNull wrap), matching Map's "NULL for absent/non-numeric,
//     never a query-aborting cast error" contract? Answered empirically
//     below: yes, because chsql.exprFieldAccess's JSON branch normalises
//     missing-vs-empty exactly like exprMapAccess's does (see its own
//     doc), so the string toFloat64OrNull receives is byte-identical
//     between the two physical storages for every case exercised here.
//  2. Structural-join / nested-set emitters — do they route a JSON-typed
//     attribute predicate through the SAME attrStrategies the plain scan
//     path uses, or does the closure/union machinery's own QueryBuilder
//     composition (rightArm/leftArm built via NewQuery(), never calling
//     WithAttrStrategies themselves) silently fall back to the Map
//     shape? Answered empirically below: the strategy survives, because
//     QueryBuilder.Frag() shares the enclosing Builder rather than
//     spinning up its own (see builder.go's subquerySQL/Frag), so
//     whichever QueryBuilder the emitter's own emitSelect chokepoint
//     stamped is the one every nested arm renders against.
//
// Both variants run through tempo.Handler's full /api/search pipeline
// (parse -> lower -> wrap -> optimize -> emit -> ClickHouse -> response
// shaping), with Handler.SetAttrStrategies wired exactly as
// cmd/cerberus's mountAPIHeads wires it from a real preflight run — this
// file only replaces preflight's boot probe with a direct
// SetAttrStrategies call so the DDL's column type can be asserted
// against directly rather than re-deriving it from system.columns.
//
// ResourceAttributes stays Map(String,String) in EVERY table this file
// seeds, even the "JSON" variant — only SpanAttributes flips. This is
// deliberate, not an oversight: canonicalSampleProjections /
// sampleProjectionsWithSelected (handler.go) unconditionally build the
// /api/search response's Attributes column via
// mapConcat(ResourceAttributes, map('__cerberus_traceID', ..., ...)) —
// a genuine FULL-MAP read over ResourceAttributes needed on EVERY
// response (the synthetic trace/parent-span-id keys toTraceSummaries
// groups by), not something a query can opt out of. That is a full-map
// operation in exactly the sense cerberus issue #3065 scopes as a
// follow-up (JSONAllPaths-based reconstruction — a plain
// CAST(json_col, 'Map(String,String)') was tried and rejected empirically:
// ClickHouse 26.5 raises TYPE_MISMATCH, "Unsupported types to CAST AS
// Map"), so a JSON-typed ResourceAttributes column breaks EVERY
// /api/search response today regardless of the query's own filter
// shape — filed as #3065's item 0, ahead of compare()/tag-discovery,
// specifically because it blocks the baseline endpoint rather than an
// advanced operator. This file's own tests exercise the per-key
// FieldAccess/FnMapContainsKey mechanism the wiring in this PR actually
// fixes (SpanAttributes), which does not depend on that follow-up.
//
// The Map and JSON variants use DISTINCT table names (via a custom
// schema.Traces.SpansTable, not the package's shared "otel_traces"
// tracesDDL/tracesSeed constants other files here use) rather than two
// same-named tables in sequence: chclienttest.NewChDB's session is
// process-global (chdb-go caches one engine per process — see its own
// doc, and internal/chsqltest's identical note) and Client.Seed silently
// PROMOTES "CREATE TABLE" to "CREATE OR REPLACE TABLE" (testsql's
// PromoteCreateTable), so two same-named "otel_traces" tables built by
// two separate newAttrStrategyChDBServer calls would silently replace
// one another — the second CREATE would retype the FIRST server's table
// out from under it. Distinct table names let both servers coexist
// through the whole test, exactly like the LogQL differential's
// attrs_map/attrs_json tables in an isolated database.
package tempo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// jsonAttrTracesDDL renders an otel_traces-shaped table named table with
// spanAttrType / resourceAttrType as its SpanAttributes / ResourceAttributes
// column types independently — either "Map(String, String)" or "JSON" —
// so the Map and JSON variants stay byte-identical apart from that type
// and their table name. Independent rather than one shared type because
// this file's tests deliberately keep ResourceAttributes Map-typed even
// in the "JSON" variant — see the package doc for why.
func jsonAttrTracesDDL(table, spanAttrType, resourceAttrType string) string {
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
    SpanAttributes %s,
    ResourceAttributes %s
) ENGINE = MergeTree() ORDER BY (Timestamp);`, table, spanAttrType, resourceAttrType)
}

// jsonAttrComparisonSeedMap / jsonAttrComparisonSeedJSON seed four
// independent single-span traces into table, exercising every
// missing-vs-present shape a numeric comparison must handle:
//
//	s1 http.status_code="200"  (numeric string    -> matches > 100)
//	s2 (key absent entirely)   (missing            -> NULL, no match)
//	s3 http.status_code="abc"  (non-numeric string -> NULL, no match)
//	s4 http.status_code=""     (present, empty     -> NULL, no match;
//	                             but IS present for `!= nil`)
func jsonAttrComparisonSeedMap(table string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('c0000000000000000000000000000001', '3000000000000001', '', 's1', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', map('http.status_code', '200'), map()),
    ('c0000000000000000000000000000002', '3000000000000002', '', 's2', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', '', '', '', map(), map()),
    ('c0000000000000000000000000000003', '3000000000000003', '', 's3', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', map('http.status_code', 'abc'), map()),
    ('c0000000000000000000000000000004', '3000000000000004', '', 's4', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000004', 9), 'Unset', '', '', '', map('http.status_code', ''), map());`, table)
}

// jsonAttrComparisonSeedJSON seeds the JSON-variant table. ResourceAttributes
// stays map() (Map(String,String)) even here — see the package doc for
// why only SpanAttributes flips to a JSON literal.
func jsonAttrComparisonSeedJSON(table string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('c0000000000000000000000000000001', '3000000000000001', '', 's1', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', '{"http.status_code":"200"}', map()),
    ('c0000000000000000000000000000002', '3000000000000002', '', 's2', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', '', '', '', '{}', map()),
    ('c0000000000000000000000000000003', '3000000000000003', '', 's3', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', '{"http.status_code":"abc"}', map()),
    ('c0000000000000000000000000000004', '3000000000000004', '', 's4', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000004', 9), 'Unset', '', '', '', '{"http.status_code":""}', map());`, table)
}

// jsonAttrSearchWindowQS is the fixed /api/search start/end reused across
// this file's tests (matches search_select_plain_filter_chdb_test.go's
// own window, wide enough to cover 2026-05-01).
const jsonAttrSearchWindowQS = "start=1777593600&end=1777680000&limit=20&spss=20"

// newAttrStrategyChDBServer builds a tempo.Handler over a table named
// table (created by ddl, populated by seed) with attrStrategies wired via
// SetAttrStrategies exactly as cmd/cerberus wires preflight's resolved
// Result.TracesAttrStrategies — see tempo.Handler.SetAttrStrategies's doc
// for why a setter is needed rather than a plain field assignment. The
// schema's SpansTable is overridden to table so Map and JSON variants can
// coexist in the shared chDB session under distinct table names (see this
// file's own package doc).
func newAttrStrategyChDBServer(t *testing.T, c *chclienttest.Client, table, ddl, seed string, attrStrategies chsql.AttrStrategies) *httptest.Server {
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

// searchSpanIDs issues /api/search for query and returns the sorted set
// of matched span IDs across every returned trace/spanset — the
// comparison unit both tests below use, since it is agnostic to
// spanset/trace grouping shape and only cares about WHICH spans matched.
func searchSpanIDs(t *testing.T, srv *httptest.Server, query string) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(query) + "&" + jsonAttrSearchWindowQS)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s\nquery: %s", resp.StatusCode, body, query)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	var ids []string
	for _, tr := range sr.Traces {
		for _, ss := range tr.SpanSets {
			for _, sp := range ss.Spans {
				ids = append(ids, sp.SpanID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// TestTraceQL_JSONAttrStrategy_ComparisonTyping_ChDB is half 1: it runs
// an equality match, a numeric comparison (the open "comparison typing"
// question), and an existence check against BOTH a Map-typed and a
// JSON-typed otel_traces-shaped table, for every missing/non-numeric/empty
// shape jsonAttrComparisonSeedMap/JSON seeds — proving chsql's
// exprFieldAccess JSON branch reproduces Map's contract for the exact
// query shapes TraceQL builds (FieldAccess reads, toFloat64OrNull
// coercion, FnMapContainsKey existence), not merely that the two happen
// to agree by accident on one shape.
func TestTraceQL_JSONAttrStrategy_ComparisonTyping_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "attr_strategy_cmp_map", "attr_strategy_cmp_json"
	strategies := chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON}
	mapSrv := newAttrStrategyChDBServer(t, c, mapTable,
		jsonAttrTracesDDL(mapTable, "Map(String, String)", "Map(String, String)"), jsonAttrComparisonSeedMap(mapTable), nil)
	jsonSrv := newAttrStrategyChDBServer(t, c, jsonTable,
		jsonAttrTracesDDL(jsonTable, "JSON", "Map(String, String)"), jsonAttrComparisonSeedJSON(jsonTable), strategies)

	cases := []struct {
		name  string
		query string
		want  []string // sorted span IDs
	}{
		{
			name:  "equality",
			query: `{ span.http.status_code = "200" }`,
			want:  []string{"3000000000000001"},
		},
		{
			name:  "numeric_comparison",
			query: `{ span.http.status_code > 100 }`,
			want:  []string{"3000000000000001"},
		},
		{
			name:  "existence",
			query: `{ span.http.status_code != nil }`,
			want:  []string{"3000000000000001", "3000000000000003", "3000000000000004"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapGot := searchSpanIDs(t, mapSrv, c.query)
			jsonGot := searchSpanIDs(t, jsonSrv, c.query)
			if !equalStrSlices(mapGot, c.want) {
				t.Fatalf("Map query %q = %v, want %v (test's own expectation, not just Map==JSON)", c.query, mapGot, c.want)
			}
			if !equalStrSlices(jsonGot, c.want) {
				t.Errorf("JSON query %q = %v, want %v — chsql's exprFieldAccess JSON branch must reproduce "+
					"the Map subscript's comparison-typing contract exactly", c.query, jsonGot, c.want)
			}
		})
	}
}

// jsonAttrStructuralSeedMap / jsonAttrStructuralSeedJSON seed a two-span
// trace into table — root (resource.service.name="frontend") with one
// child (span.http.status_code="500") — plus an unrelated single-span
// trace that must never match the structural relation.
func jsonAttrStructuralSeedMap(table string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('d0000000000000000000000000000001', '4000000000000001', '', 'GET /home', 'Server', 1000, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend')),
    ('d0000000000000000000000000000001', '4000000000000002', '4000000000000001', 'checkout', 'Server', 700, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Error', '', '', '', map('http.status_code', '500'), map()),
    ('d0000000000000000000000000000002', '4000000000000003', '', 'solo', 'Server', 300, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', map('http.status_code', '500'), map('service.name', 'other'));`, table)
}

// jsonAttrStructuralSeedJSON seeds the JSON-variant table. ResourceAttributes
// stays map() (Map(String,String)) even here — see the package doc — so
// this seed differs from jsonAttrStructuralSeedMap only in SpanAttributes'
// literal shape.
func jsonAttrStructuralSeedJSON(table string) string {
	return fmt.Sprintf(`INSERT INTO %s VALUES
    ('d0000000000000000000000000000001', '4000000000000001', '', 'GET /home', 'Server', 1000, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', '{}', map('service.name', 'frontend')),
    ('d0000000000000000000000000000001', '4000000000000002', '4000000000000001', 'checkout', 'Server', 700, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Error', '', '', '', '{"http.status_code":"500"}', map()),
    ('d0000000000000000000000000000002', '4000000000000003', '', 'solo', 'Server', 300, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', '{"http.status_code":"500"}', map('service.name', 'other'));`, table)
}

// TestTraceQL_JSONAttrStrategy_StructuralJoin_ChDB is half 2: a `>>`
// structural-child query filters the LEFT (ancestor) operand on a
// (Map-typed, in both variants — see the package doc) RESOURCE attribute
// and the RIGHT (descendant) operand on a JSON-typed SPAN attribute —
// settling empirically whether structural_join.go's rightArm/leftArm
// QueryBuilders (built via NewQuery(), never calling WithAttrStrategies
// themselves — see this file's own package doc) inherit the strategy the
// outer emitSelect chokepoint stamped, or silently fall back to the Map
// shape and either fail with ILLEGAL_TYPE_OF_ARGUMENT or (worse) silently
// match nothing. Mixing one Map-typed and one JSON-typed carrier column
// inside a single structural plan additionally proves attrStrategies
// applies PER COLUMN within one composite plan, not merely per-request.
// The unrelated second trace shares the descendant's attribute value but
// not the ancestor relationship, so an over-wide match (the strategy
// leaking scope, or the relation collapsing to an unscoped predicate)
// would be caught by an unexpected extra span ID.
func TestTraceQL_JSONAttrStrategy_StructuralJoin_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "attr_strategy_struct_map", "attr_strategy_struct_json"
	strategies := chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON}
	mapSrv := newAttrStrategyChDBServer(t, c, mapTable,
		jsonAttrTracesDDL(mapTable, "Map(String, String)", "Map(String, String)"), jsonAttrStructuralSeedMap(mapTable), nil)
	jsonSrv := newAttrStrategyChDBServer(t, c, jsonTable,
		jsonAttrTracesDDL(jsonTable, "JSON", "Map(String, String)"), jsonAttrStructuralSeedJSON(jsonTable), strategies)

	const query = `{ resource.service.name = "frontend" } >> { span.http.status_code = "500" }`
	want := []string{"4000000000000002"} // only the checkout child under the frontend root

	mapGot := searchSpanIDs(t, mapSrv, query)
	if !equalStrSlices(mapGot, want) {
		t.Fatalf("Map structural query = %v, want %v (test's own expectation, not just Map==JSON)", mapGot, want)
	}
	jsonGot := searchSpanIDs(t, jsonSrv, query)
	if !equalStrSlices(jsonGot, want) {
		t.Errorf("JSON structural query = %v, want %v — the structural-join emitter's rightArm/leftArm "+
			"QueryBuilders must inherit the outer emitSelect chokepoint's AttrStrategies, not fall back to Map",
			jsonGot, want)
	}
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
