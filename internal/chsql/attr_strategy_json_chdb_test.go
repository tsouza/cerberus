//go:build chdb

// chDB-backed proof of cerberus issue #2777's missing-vs-empty semantics
// decision: a raw ClickHouse JSON dynamic-subcolumn path read and a Map
// subscript disagree on what a MISSING key returns (NULL vs the empty
// string), and chsql's exprMapAccess JSON branch (builder.go) deliberately
// normalises that difference away with a coalesce-to-empty-string wrap so
// every existing chplan-level lowering — written against the Map contract
// — keeps working unmodified against a JSON-typed column. This file pins
// BOTH halves:
//
//  1. the raw discrepancy exists (justifying the coalesce design choice in
//     the first place — a substrate where Map and JSON already agreed
//     would make the wrap dead code), and
//  2. chsql's ACTUAL emitted SQL (via the public chsql.NewQuery /
//     QueryBuilder.WithAttrStrategies surface, not a hand-written
//     comparison string) reproduces the Map contract exactly: the same
//     missing-key value, and the same present-vs-absent distinction via
//     FnMapContainsKey/mapContains vs has(JSONAllPaths(...)).
//
// This is SEPARATE from, and not a substitute for,
// TestAttrStrategy_MapPathByteIdentical (attr_strategy_test.go) — that
// test is the refactor guard proving the pre-#2777 Map path emits
// byte-identical SQL; this one is evidence the NEW JSON path actually
// works against real ClickHouse (chDB, version pinned by versions.yaml's
// chdb_substrate).
package chsql_test

import (
	"database/sql"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// attrStrategyJSONSeedDDL creates one Map-typed and one JSON-typed
// attribute table with logically equivalent content, so every query below
// runs against both and compares.
//
// Row 1: both keys present, http.status_code="200".
// Row 2: only service.name present — http.status_code absent.
// Row 3: service.name present but set to the EMPTY STRING — the
// present-but-empty case that must read back as the empty string (not
// NULL) on the JSON side, exactly as a Map's stored empty value does,
// distinguishing it from row 2's genuinely absent key.
const attrStrategyJSONSeedDDL = `
CREATE OR REPLACE TABLE attrs_map (
    id UInt32,
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree ORDER BY id;

CREATE OR REPLACE TABLE attrs_json (
    id UInt32,
    ResourceAttributes JSON
) ENGINE = MergeTree ORDER BY id;

INSERT INTO attrs_map VALUES
    (1, map('service.name', 'foo', 'http.status_code', '200')),
    (2, map('service.name', 'bar')),
    (3, map('service.name', ''));

INSERT INTO attrs_json VALUES
    (1, '{"service.name":"foo","http.status_code":"200"}'),
    (2, '{"service.name":"bar"}'),
    (3, '{"service.name":""}');
`

// renderAttrSelect renders "SELECT <expr> FROM <table> ORDER BY id" via
// the public chsql surface, with strategies applied via
// WithAttrStrategies (nil for the Map-typed table, which never calls it —
// mirroring how a production caller that never resolved a JSON strategy
// never calls it either).
func renderAttrSelect(t *testing.T, table string, strategies chsql.AttrStrategies, expr chplan.Expr) (string, []any) {
	t.Helper()
	var renderErr error
	qb := chsql.NewQuery().
		Select(func(b *chsql.Builder) {
			renderErr = b.Expr(expr)
		}).
		From(chsql.Col(table)).
		OrderBy(chsql.Col("id"), false)
	if strategies != nil {
		qb = qb.WithAttrStrategies(strategies)
	}
	sql, args := qb.Build()
	if renderErr != nil {
		t.Fatalf("render select expr: %v", renderErr)
	}
	return sql, args
}

// queryStringColumn runs sql/args and returns one Nullable(String) column,
// row order preserved (the query already carries ORDER BY id).
func queryStringColumn(t *testing.T, db *sql.DB, query string, args []any) []sql.NullString {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, query)
	}
	defer rows.Close()
	var out []sql.NullString
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration: %v", err)
	}
	return out
}

// queryBoolColumn runs sql/args and returns one UInt8-as-bool column.
func queryBoolColumn(t *testing.T, db *sql.DB, query string, args []any) []bool {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, query)
	}
	defer rows.Close()
	var out []bool
	for rows.Next() {
		var v uint8
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v != 0)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration: %v", err)
	}
	return out
}

func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }

var nsNull = sql.NullString{}

// TestAttrStrategyJSON_RawPathDiffersFromMap is half 1 of the pinning: a
// raw ClickHouse JSON dynamic-subcolumn read and a Map subscript
// genuinely disagree on a missing key (NULL vs the empty string) — the
// discrepancy chsql's coalesce wrap exists to paper over. This query is
// deliberately NOT run through chsql (no coalesce) so it shows what the
// JSON type does on its own.
func TestAttrStrategyJSON_RawPathDiffersFromMap(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(attrStrategyJSONSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapGot := queryStringColumn(t, db,
		"SELECT ResourceAttributes['http.status_code'] FROM attrs_map ORDER BY id", nil)
	jsonRawGot := queryStringColumn(t, db,
		"SELECT ResourceAttributes.`http.status_code`.:String FROM attrs_json ORDER BY id", nil)

	wantMap := []sql.NullString{ns("200"), ns(""), ns("")}
	if !equalNullStrings(mapGot, wantMap) {
		t.Fatalf("Map subscript = %v, want %v", mapGot, wantMap)
	}
	wantJSONRaw := []sql.NullString{ns("200"), nsNull, nsNull}
	if !equalNullStrings(jsonRawGot, wantJSONRaw) {
		t.Fatalf("raw JSON path read = %v, want %v (row 2/3 must be NULL, not the empty string, "+
			"proving the raw JSON path genuinely differs from Map's default-value semantics)",
			jsonRawGot, wantJSONRaw)
	}
}

// TestAttrStrategyJSON_MapAccessMatchesMapSemantics is half 2: chsql's
// ACTUAL rendered SQL (via chplan.MapAccess + AttrStrategyJSON, the exact
// path a LogQL label matcher against a JSON-typed ResourceAttributes
// column takes) reproduces the Map subscript's missing-key value exactly
// — the empty string, never NULL — for both a genuinely-absent key (row
// 2) and a present-but-empty one (row 3, which must read back as the
// empty string, not merely "non-NULL").
func TestAttrStrategyJSON_MapAccessMatchesMapSemantics(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(attrStrategyJSONSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapAccess := func(col string) *chplan.MapAccess {
		return &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: "ResourceAttributes"},
			Key: &chplan.LitString{V: col},
		}
	}

	for _, key := range []string{"service.name", "http.status_code"} {
		key := key
		t.Run(key, func(t *testing.T) {
			mapSQL, mapArgs := renderAttrSelect(t, "attrs_map", nil, mapAccess(key))
			jsonSQL, jsonArgs := renderAttrSelect(t, "attrs_json",
				chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}, mapAccess(key))

			mapGot := queryStringColumn(t, db, mapSQL, mapArgs)
			jsonGot := queryStringColumn(t, db, jsonSQL, jsonArgs)

			if !equalNullStrings(mapGot, jsonGot) {
				t.Errorf("key %q: Map rendering = %v, JSON rendering = %v — chsql's JSON MapAccess "+
					"branch must reproduce the Map subscript's missing-key contract exactly",
					key, mapGot, jsonGot)
			}
		})
	}
}

// TestAttrStrategyJSON_MapContainsKeyMatchesMapSemantics pins
// FnMapContainsKey's JSON branch (has(JSONAllPaths(...), ...)) against
// mapContains's real existence semantics: both must distinguish
// present-with-empty-value (row 3) from genuinely-absent (row 2), which
// exprMapAccess's coalesce-to-empty-string wrap deliberately gives up —
// this is the check that recovers it.
func TestAttrStrategyJSON_MapContainsKeyMatchesMapSemantics(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(attrStrategyJSONSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapContains := func(col string) *chplan.FuncCall {
		return &chplan.FuncCall{
			Fn: chplan.FnMapContainsKey,
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "ResourceAttributes"},
				&chplan.LitString{V: col},
			},
		}
	}

	for _, key := range []string{"service.name", "http.status_code"} {
		key := key
		t.Run(key, func(t *testing.T) {
			mapSQL, mapArgs := renderAttrSelect(t, "attrs_map", nil, mapContains(key))
			jsonSQL, jsonArgs := renderAttrSelect(t, "attrs_json",
				chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}, mapContains(key))

			mapGot := queryBoolColumn(t, db, mapSQL, mapArgs)
			jsonGot := queryBoolColumn(t, db, jsonSQL, jsonArgs)

			if len(mapGot) != len(jsonGot) {
				t.Fatalf("key %q: row count mismatch: Map %d, JSON %d", key, len(mapGot), len(jsonGot))
			}
			for i := range mapGot {
				if mapGot[i] != jsonGot[i] {
					t.Errorf("key %q row %d: mapContains=%v has(JSONAllPaths(...))=%v, want equal",
						key, i+1, mapGot[i], jsonGot[i])
				}
			}
		})
	}

	// Explicit sanity check on the shape that matters most: row 3
	// (present, empty value) must report TRUE (present) on both sides,
	// distinguishing it from row 2 (absent, FALSE) — this is exactly the
	// distinction exprMapAccess's coalesce wrap cannot make on its own.
	mapSQL, mapArgs := renderAttrSelect(t, "attrs_map", nil, mapContains("service.name"))
	jsonSQL, jsonArgs := renderAttrSelect(t, "attrs_json",
		chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}, mapContains("service.name"))
	wantPresence := []bool{true, true, true} // service.name is present (possibly empty) on every row
	if got := queryBoolColumn(t, db, mapSQL, mapArgs); !equalBools(got, wantPresence) {
		t.Fatalf("Map mapContains(service.name) = %v, want %v", got, wantPresence)
	}
	if got := queryBoolColumn(t, db, jsonSQL, jsonArgs); !equalBools(got, wantPresence) {
		t.Fatalf("JSON has(JSONAllPaths(...), service.name) = %v, want %v", got, wantPresence)
	}
}

func equalNullStrings(a, b []sql.NullString) bool {
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

func equalBools(a, b []bool) bool {
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
