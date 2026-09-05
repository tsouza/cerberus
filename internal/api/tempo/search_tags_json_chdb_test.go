//go:build chdb

// chDB-backed proof of cerberus issue #3065 item 2: /api/search/tags,
// /api/v2/search/tags and /api/search/tag/{name}/values
// (search_tags.go / search_tag_values.go) build their SQL directly
// against chsql.NewQuery() rather than through a chplan tree, so they
// never reached engine.emitForHead / chsql.Emit's ctx-based
// AttrStrategies threading at all — Handler.AttrStrategies (cerberus
// issue #3062) had no effect on them. internal/api/tempo/attr_strategy.go
// (distinctAttrKeysFrag) plus threading chsql.AttrStrategies explicitly
// onto every chsql.QueryBuilder these two files build (mirroring
// internal/api/loki/attr_strategy.go's identical fix for cerberus issue
// #3063 point 2) is this file's target.
//
// Both a Resource-scope and a Span-scope JSON-typed column are exercised
// (not just one), since #3065 point 2 names BOTH /search/tags' per-scope
// key discovery AND /search/tag/{name}/values' per-scope, auto-scope
// (leading-dot union) and existence-pre-filter shapes — every one of
// which reads through a DIFFERENT ad-hoc Frag helper
// (distinctAttrKeysFrag / mapAtFrag / mapContainsFrag /
// attrValueArrayJoinFrag / mapContainsAnyFrag) that must all resolve the
// SAME threaded AttrStrategies to agree with each other and with the
// query path's own per-key JSON rendering (cerberus issue #3062).
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chsql"
)

// tagDiscoverySeedMap / tagDiscoverySeedJSON seed three single-span traces
// exercising every missing/present/empty shape the discovery endpoints
// must handle identically to the Map baseline:
//
//	s1 span.custom_tag="alpha"  resource.deploy.env="prod"   (both present)
//	s2 span.custom_tag absent   resource.deploy.env="canary" (span key
//	                            missing entirely; resource key present)
//	s3 span.custom_tag=""       resource.deploy.env=""       (present but
//	                            empty on both sides — /tags must still
//	                            report the KEY; /tag/values must NOT
//	                            report the empty string as a value)
func tagDiscoverySeedMap(table string) string {
	return `INSERT INTO ` + table + ` VALUES
    ('a1000000000000000000000000000001', '7000000000000001', '', 'op1', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', map('custom_tag', 'alpha'), map('deploy.env', 'prod')),
    ('a1000000000000000000000000000002', '7000000000000002', '', 'op2', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', '', '', '', map(), map('deploy.env', 'canary')),
    ('a1000000000000000000000000000003', '7000000000000003', '', 'op3', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', map('custom_tag', ''), map('deploy.env', ''));`
}

func tagDiscoverySeedJSON(table string) string {
	return `INSERT INTO ` + table + ` VALUES
    ('a1000000000000000000000000000001', '7000000000000001', '', 'op1', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000001', 9), 'Unset', '', '', '', '{"custom_tag":"alpha"}', '{"deploy.env":"prod"}'),
    ('a1000000000000000000000000000002', '7000000000000002', '', 'op2', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000002', 9), 'Unset', '', '', '', '{}', '{"deploy.env":"canary"}'),
    ('a1000000000000000000000000000003', '7000000000000003', '', 'op3', 'Server', 100, toDateTime64('2026-05-01 11:00:00.000000003', 9), 'Unset', '', '', '', '{"custom_tag":""}', '{"deploy.env":""}');`
}

// tagDiscoveryWindowQS is this file's fixed discovery-request window,
// wide enough to cover the 2026-05-01 seed above.
const tagDiscoveryWindowQS = "start=1777593600&end=1777680000"

// TestTags_JSONAttrStrategy_KeyDiscovery_ChDB proves /api/search/tags
// (V1, both scope=span and scope=resource) surfaces the SAME key set for
// a JSON-typed SpanAttributes/ResourceAttributes column as it does for a
// Map-typed one — including the empty-value row (s3), which must still
// contribute its key (key PRESENCE, not value non-emptiness, is what
// /tags reports).
func TestTags_JSONAttrStrategy_KeyDiscovery_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "tags_discovery_map", "tags_discovery_json"
	strategies := chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON, "ResourceAttributes": chsql.AttrStrategyJSON}

	mapSrv := newAttrStrategyChDBServer(t, c, mapTable,
		jsonAttrTracesDDL(mapTable, "Map(String, String)", "Map(String, String)"), tagDiscoverySeedMap(mapTable), nil)
	jsonSrv := newAttrStrategyChDBServer(t, c, jsonTable,
		jsonAttrTracesDDL(jsonTable, "JSON", "JSON"), tagDiscoverySeedJSON(jsonTable), strategies)

	for _, scope := range []string{"span", "resource"} {
		t.Run(scope, func(t *testing.T) {
			mapTags := searchTags(t, mapSrv, scope)
			jsonTags := searchTags(t, jsonSrv, scope)

			var wantKey string
			switch scope {
			case "span":
				wantKey = "custom_tag"
			case "resource":
				wantKey = "deploy.env"
			}
			if !containsStr(mapTags, wantKey) {
				t.Fatalf("Map /api/search/tags?scope=%s = %v, want it to contain %q (test's own expectation, not just Map==JSON)", scope, mapTags, wantKey)
			}
			if !containsStr(jsonTags, wantKey) {
				t.Errorf("JSON /api/search/tags?scope=%s = %v, want it to contain %q — distinctAttrKeysFrag's "+
					"JSONAllPaths branch must report every key a Map .keys subcolumn would", scope, jsonTags, wantKey)
			}
		})
	}
}

// searchTags issues GET /api/search/tags?scope=<scope> and returns the
// sorted tagNames list.
func searchTags(t *testing.T, srv *httptest.Server, scope string) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search/tags?scope=" + scope + "&" + tagDiscoveryWindowQS)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchTagsResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	sort.Strings(sr.TagNames)
	return sr.TagNames
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestTagValues_JSONAttrStrategy_ChDB proves /api/search/tag/{name}/values
// against a JSON-typed SpanAttributes/ResourceAttributes column
// reproduces the Map baseline exactly for every discovery shape #3065
// point 2 names: single-scope (span./resource.), auto-scope (leading-dot
// union of both maps), and the empty-value exclusion (s3's empty string
// must never surface as a value).
func TestTagValues_JSONAttrStrategy_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	const mapTable, jsonTable = "tag_values_discovery_map", "tag_values_discovery_json"
	strategies := chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON, "ResourceAttributes": chsql.AttrStrategyJSON}

	mapSrv := newAttrStrategyChDBServer(t, c, mapTable,
		jsonAttrTracesDDL(mapTable, "Map(String, String)", "Map(String, String)"), tagDiscoverySeedMap(mapTable), nil)
	jsonSrv := newAttrStrategyChDBServer(t, c, jsonTable,
		jsonAttrTracesDDL(jsonTable, "JSON", "JSON"), tagDiscoverySeedJSON(jsonTable), strategies)

	cases := []struct {
		name string
		tag  string
		want []string
	}{
		{"span_scoped", "span.custom_tag", []string{"alpha"}},
		{"resource_scoped", "resource.deploy.env", []string{"canary", "prod"}},
		{"auto_scope_leading_dot", ".deploy.env", []string{"canary", "prod"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapGot := tagValues(t, mapSrv, tc.tag)
			if !equalStrSlices(mapGot, tc.want) {
				t.Fatalf("Map /api/search/tag/%s/values = %v, want %v (test's own expectation, not just Map==JSON)", tc.tag, mapGot, tc.want)
			}
			jsonGot := tagValues(t, jsonSrv, tc.tag)
			if !equalStrSlices(jsonGot, tc.want) {
				t.Errorf("JSON /api/search/tag/%s/values = %v, want %v — mapAtFrag/mapContainsFrag's "+
					"Builder.MapAt/MapContains delegation must reconstruct the JSON column exactly like Map",
					tc.tag, jsonGot, tc.want)
			}
		})
	}
}

// tagValues issues GET /api/search/tag/<name>/values and returns the
// sorted tagValues list.
func tagValues(t *testing.T, srv *httptest.Server, name string) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/search/tag/" + url.PathEscape(name) + "/values?" + tagDiscoveryWindowQS)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchTagValuesResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	sort.Strings(sr.TagValues)
	return sr.TagValues
}
