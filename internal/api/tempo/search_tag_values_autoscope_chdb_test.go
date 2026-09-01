//go:build chdb

// chDB-backed correctness roundtrip for cerberus issue #2870's auto-scope
// materialized-column routing (buildAutoScopeUnionAttributeValuesSQL). The
// SQL-shape test in search_tag_values_autoscope_materialized_test.go pins
// which Frags the routed query is built from; this file proves the
// EXECUTED result of the routed (UNION ALL) query is byte-identical, as a
// set, to the executed result of the unrouted (arrayJoin-over-both-maps)
// query the auto-scope form used before this feature — the same evidence
// bar cerberus issue #2776 set for the single-scope routing (see that
// issue's PR body / docs/operations.md).
//
// Every materialized column here is declared DEFAULT <map>[<key>], the
// exact DDL shape internal/schema/ddl.renderTraceMaterializedAttrColumns
// issues in production (cerberus issue #2776's schema.Traces doc): the
// column is therefore, by construction, value-identical to what the
// map-only path reads for that key on every row, seeded or not — so a
// divergence between the routed and unrouted result sets below can only
// come from buildAutoScopeUnionAttributeValuesSQL's own union/dedup shape,
// never from the seed data disagreeing with itself.

package tempo_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// autoScopeWindowBase anchors every seeded row well away from wall-clock
// drift, mirroring internal/api/loki's chdb test convention (see
// map_key_order_chdb_test.go's keyOrderWindowBase).
var autoScopeWindowBase = time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

// autoScopeSeedTable is the minimal otel_traces projection
// buildAutoScopeUnionAttributeValuesSQL and its unrouted arrayJoin
// predecessor both read: the two source Maps, plus one materialized
// column per (scope, key) pair this file's four cases exercise. Engine =
// Memory mirrors the loki chdb tests — none of these queries depends on
// MergeTree ordering.
const autoScopeSeedTable = "CREATE TABLE otel_traces (\n" +
	"    Timestamp DateTime64(9),\n" +
	"    SpanAttributes Map(String, String),\n" +
	"    ResourceAttributes Map(String, String),\n" +
	"    `__cerberus_materialized_http.status_code` LowCardinality(String) DEFAULT SpanAttributes['http.status_code'],\n" +
	"    `__cerberus_materialized_k8s.namespace.name` LowCardinality(String) DEFAULT ResourceAttributes['k8s.namespace.name'],\n" +
	"    `__cerberus_materialized_span_both.key` LowCardinality(String) DEFAULT SpanAttributes['both.key'],\n" +
	"    `__cerberus_materialized_resource_both.key` LowCardinality(String) DEFAULT ResourceAttributes['both.key']\n" +
	") ENGINE = Memory;"

// autoScopeSeedRow is one seeded span: the literal CH map() expressions
// for both attribute maps. Writing them out longhand (rather than
// building from a Go map) keeps each row's exact key/value set legible
// next to the case it exercises.
type autoScopeSeedRow struct {
	spanMapSQL, resourceMapSQL string
}

// autoScopeSeedRows covers all four buildAttributeValuesSQL routing cases
// (cerberus issue #2870) plus, within the two single-side cases, a row
// where the OTHER (unmaterialized-for-that-key) map ALSO happens to carry
// the key — an unusual but valid shape that specifically exercises the
// map-subscript fallback arm merging with the materialized-column arm,
// not just the materialized arm alone:
//
//   - both.key: materialized in BOTH maps. r0/r1 give the same value
//     ("v1") from two DIFFERENT rows/scopes — proving the union DEDUPES
//     across scopes, not just within one. r2 gives two DISTINCT values
//     ("v2" span, "v3" resource) on the SAME row.
//   - http.status_code: materialized in SPAN only. r3 is span-only
//     ("200"); r4 carries a span value ("500") AND a resource value
//     ("999") for the SAME key name — the resource side has no
//     materialized column for this key, so it must fall back to the map
//     subscript and still contribute "999" to the union.
//   - k8s.namespace.name: materialized in RESOURCE only, mirrored: r5 is
//     resource-only ("ns-a"); r6 carries a resource value ("ns-b") AND a
//     span value ("ns-from-span") for the same key name.
//   - rpc.method: materialized in NEITHER map (r7/r8) — the routing miss
//     that keeps buildAttributeValuesSQL on its unrouted arrayJoin path;
//     included so the "routed vs unrouted" comparison below also covers
//     the trivial case where routed IS unrouted, byte for byte.
func autoScopeSeedRows() []autoScopeSeedRow {
	return []autoScopeSeedRow{
		{spanMapSQL: "map('both.key','v1')", resourceMapSQL: "map()"},
		{spanMapSQL: "map()", resourceMapSQL: "map('both.key','v1')"},
		{spanMapSQL: "map('both.key','v2')", resourceMapSQL: "map('both.key','v3')"},
		{spanMapSQL: "map('http.status_code','200')", resourceMapSQL: "map()"},
		{spanMapSQL: "map('http.status_code','500')", resourceMapSQL: "map('http.status_code','999')"},
		{spanMapSQL: "map()", resourceMapSQL: "map('k8s.namespace.name','ns-a')"},
		{spanMapSQL: "map('k8s.namespace.name','ns-from-span')", resourceMapSQL: "map('k8s.namespace.name','ns-b')"},
		{spanMapSQL: "map('rpc.method','GetUser')", resourceMapSQL: "map()"},
		{spanMapSQL: "map()", resourceMapSQL: "map('rpc.method','ResourceRPC')"},
	}
}

// autoScopeMaterializedSchema is the routed schema: the curated set of
// (scope, key) -> column entries an operator opted into (cerberus issue
// #2776's CERBERUS_SCHEMA_TRACES_MATERIALIZED_ATTRS_ENABLED), covering
// three of autoScopeSeedRows's four keys. `rpc.method` is deliberately
// absent from both maps — the fourth case, materialized-in-neither.
func autoScopeMaterializedSchema() schema.Traces {
	s := schema.DefaultOTelTraces()
	s.MaterializedSpanAttributeColumns = map[string]string{
		"http.status_code": "__cerberus_materialized_http.status_code",
		"both.key":         "__cerberus_materialized_span_both.key",
	}
	s.MaterializedResourceAttributeColumns = map[string]string{
		"k8s.namespace.name": "__cerberus_materialized_k8s.namespace.name",
		"both.key":           "__cerberus_materialized_resource_both.key",
	}
	return s
}

// seedAutoScopeClient seeds otel_traces with autoScopeSeedRows, one row
// per second from autoScopeWindowBase, and returns the chDB client plus
// the [start, end) window bracketing every seeded row.
func seedAutoScopeClient(t *testing.T) (*chclienttest.Client, time.Time, time.Time) {
	t.Helper()
	const tsFmt = "2006-01-02 15:04:05.000"

	rows := autoScopeSeedRows()
	seed := autoScopeSeedTable
	for i, r := range rows {
		ts := autoScopeWindowBase.Add(time.Duration(i) * time.Second).Format(tsFmt)
		seed += fmt.Sprintf(
			"\nINSERT INTO otel_traces (Timestamp, SpanAttributes, ResourceAttributes) VALUES"+
				" (toDateTime64('%s', 9), %s, %s);",
			ts, r.spanMapSQL, r.resourceMapSQL,
		)
	}

	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	start := autoScopeWindowBase.Add(-time.Minute)
	end := autoScopeWindowBase.Add(time.Duration(len(rows))*time.Second + time.Minute)
	return c, start, end
}

// sortedCopy returns a sorted copy of vs, leaving the input untouched.
func sortedCopy(vs []string) []string {
	out := make([]string, len(vs))
	copy(out, vs)
	sort.Strings(out)
	return out
}

// TestBuildAutoScopeUnionAttributeValuesSQL_ChDB_MatchesUnroutedMapOnlyResult
// is the correctness roundtrip cerberus issue #2870 requires: for each of
// the four materialized-routing cases, the ROUTED query (materialized
// schema) and the UNROUTED query (schema.DefaultOTelTraces(), which for
// every key here has nil registries and so always takes the unchanged
// arrayJoin-over-both-maps path) run against the SAME seeded rows must
// return the exact same set of distinct values.
func TestBuildAutoScopeUnionAttributeValuesSQL_ChDB_MatchesUnroutedMapOnlyResult(t *testing.T) {
	c, start, end := seedAutoScopeClient(t)
	ctx := context.Background()
	unroutedSchema := schema.DefaultOTelTraces()
	routedSchema := autoScopeMaterializedSchema()

	cases := []struct {
		name string
		key  string
		want []string
	}{
		{"materialized_in_both", "both.key", []string{"v1", "v2", "v3"}},
		{"materialized_in_span_only", "http.status_code", []string{"200", "500", "999"}},
		{"materialized_in_resource_only", "k8s.namespace.name", []string{"ns-a", "ns-b", "ns-from-span"}},
		{"materialized_in_neither", "rpc.method", []string{"GetUser", "ResourceRPC"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unroutedSQL, unroutedArgs := tempo.BuildAttributeValuesSQLForTest(
				unroutedSchema, tc.key, tempo.AttrMapScopeAnyForTest, nil, start, end,
			)
			routedSQL, routedArgs := tempo.BuildAttributeValuesSQLForTest(
				routedSchema, tc.key, tempo.AttrMapScopeAnyForTest, nil, start, end,
			)

			unroutedGot, err := c.QueryStrings(ctx, unroutedSQL, unroutedArgs...)
			if err != nil {
				t.Fatalf("unrouted query failed: %v\nSQL: %s", err, unroutedSQL)
			}
			routedGot, err := c.QueryStrings(ctx, routedSQL, routedArgs...)
			if err != nil {
				t.Fatalf("routed query failed: %v\nSQL: %s", err, routedSQL)
			}

			unroutedSorted := sortedCopy(unroutedGot)
			routedSorted := sortedCopy(routedGot)

			// First pin the routed result against the case's own known-good
			// expectation, so a failure names the actual wrong VALUES...
			if fmt.Sprint(routedSorted) != fmt.Sprint(sortedCopy(tc.want)) {
				t.Errorf("routed result = %v, want %v", routedSorted, tc.want)
			}
			// ...then the load-bearing #2870 assertion: routed and
			// unrouted must agree exactly, proving the union/dedup shape
			// changes nothing about the ANSWER, only how it is computed.
			if fmt.Sprint(routedSorted) != fmt.Sprint(unroutedSorted) {
				t.Errorf("routed result diverges from unrouted map-only result:\nrouted:   %v\nunrouted: %v",
					routedSorted, unroutedSorted)
			}
		})
	}
}
