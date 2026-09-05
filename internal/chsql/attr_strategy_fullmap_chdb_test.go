//go:build chdb

// chDB-backed proof of cerberus issue #3063's point 1: a LogQL full-map
// operation (mapConcat/mapFilter/mapApply/mapKeys/mapValues against a bare
// attribute-map ColumnRef, or a bare projection of one — see
// internal/logql's detected_level synthesis, structured-metadata
// projection and parser-stage label merges) must work against a
// JSON-typed attribute column exactly as it already does against a
// Map(String,String) one. This file pins jsonFullMapReconstruction
// (attr_strategy_fullmap.go) against real chDB, mirroring
// attr_strategy_json_chdb_test.go's rigor for the per-key case: genuine
// execution, not a hand-written comparison string, and missing/present/
// empty-value/nested/beyond-bound seeding rather than one happy-path row.
package chsql_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// fullMapSeedDDL mirrors attrStrategyJSONSeedDDL's Map/JSON pairing, with
// three additions specific to the full-map reconstruction:
//
//   - row 4: a 3-segment dotted key (k8s.pod.name) nested two levels deep
//     by ClickHouse's JSON default — well within maxJSONAttrFlattenDepth,
//     so it must fully reconstruct to the flat key "k8s.pod.name".
//   - row 5: a 5-segment dotted key (a.b.c.d.e) nested FOUR levels deep —
//     past maxJSONAttrFlattenDepth's bound (which fully flattens up to
//     4-segment keys) — so the JSON side is EXPECTED to diverge from the
//     Map side here, pinning the documented bounded-depth limitation
//     rather than asserting the two always agree unconditionally.
const fullMapSeedDDL = `
CREATE OR REPLACE TABLE fullmap_map (
    id UInt32,
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree ORDER BY id;

CREATE OR REPLACE TABLE fullmap_json (
    id UInt32,
    ResourceAttributes JSON
) ENGINE = MergeTree ORDER BY id;

INSERT INTO fullmap_map VALUES
    (1, map('service.name', 'foo', 'http.status_code', '200')),
    (2, map('service.name', 'bar')),
    (3, map('service.name', '')),
    (4, map('k8s.pod.name', 'p1', 'k8s.namespace.name', 'ns1')),
    (5, map('a.b.c.d.e', 'deep'));

INSERT INTO fullmap_json VALUES
    (1, '{"service.name":"foo","http.status_code":"200"}'),
    (2, '{"service.name":"bar"}'),
    (3, '{"service.name":""}'),
    (4, '{"k8s.pod.name":"p1","k8s.namespace.name":"ns1"}'),
    (5, '{"a.b.c.d.e":"deep"}');
`

// fullMapStrategies is the AttrStrategies every reconstruction test below
// resolves ResourceAttributes against on the JSON side.
var fullMapStrategies = chsql.AttrStrategies{"ResourceAttributes": chsql.AttrStrategyJSON}

// renderFullMapProject emits `SELECT <expr> AS out FROM <table> ORDER BY
// id` as a chplan.Project — the same node internal/logql's
// Lang.ProjectSamples builds for a log-stream query's Attributes column —
// through the real chsql.Emit entry point, so this exercises
// emitProject's projectedExpr substitution exactly as production traffic
// would.
func renderFullMapProject(t *testing.T, table string, strategies chsql.AttrStrategies, expr chplan.Expr) (string, []any) {
	t.Helper()
	scan := &chplan.Scan{Table: table}
	proj := &chplan.Project{
		Input:       scan,
		Projections: []chplan.Projection{{Expr: expr, Alias: "out"}},
	}
	ctx := context.Background()
	if strategies != nil {
		ctx = chsql.WithAttrStrategies(ctx, strategies)
	}
	sqlStr, args, err := chsql.Emit(ctx, proj)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return sqlStr + " ORDER BY id", args
}

func queryToStringRows(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, query)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
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

// resourceAttrsCol is the shared operand every case below runs its
// full-map function against.
func resourceAttrsCol() *chplan.ColumnRef { return &chplan.ColumnRef{Name: "ResourceAttributes"} }

// canonicalMapString renders e (a Map(String,String)-valued expression) as
// a string with its entries in a canonical key order via mapSort, then
// toString. mapConcat/mapFilter's OWN key order can legitimately differ
// between the Map baseline (insertion order) and the JSON reconstruction
// (JSONExtractKeysAndValuesRaw's own order) without either being wrong —
// this wrapper makes the comparison order-independent, the same way
// production code never depends on a Map's iteration order.
func canonicalMapString(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Fn:   chplan.FnToString,
		Args: []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnMapSort, Args: []chplan.Expr{e}}},
	}
}

// TestJSONFullMapReconstruction_MapConcat pins the dominant real shape:
// internal/logql's detected_level synthesis (withDetectedLevelAndColumns)
// calls mapConcat(<bare ResourceAttributes>, <synthesized map>) on
// essentially every log-stream query. The JSON side must return the same
// merged map as the Map side for every row, including the present-empty
// and absent-key cases mapConcat's right-hand operand doesn't touch.
func TestJSONFullMapReconstruction_MapConcat(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fullMapSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapConcatExpr := func() chplan.Expr {
		return &chplan.FuncCall{
			Fn: chplan.FnMapMerge,
			Args: []chplan.Expr{
				resourceAttrsCol(),
				&chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{&chplan.LitString{V: "extra"}, &chplan.LitString{V: "1"}}},
			},
		}
	}

	mapSQL, mapArgs := renderFullMapProject(t, "fullmap_map", nil, canonicalMapString(mapConcatExpr()))
	jsonSQL, jsonArgs := renderFullMapProject(t, "fullmap_json", fullMapStrategies, canonicalMapString(mapConcatExpr()))

	mapGot := queryToStringRows(t, db, mapSQL, mapArgs)
	jsonGot := queryToStringRows(t, db, jsonSQL, jsonArgs)

	// Rows 1-4 sit within maxJSONAttrFlattenDepth and must match the Map
	// baseline exactly. Row 5 (5-segment key) is asserted separately below
	// — it is the documented bounded-depth divergence, not a bug.
	if len(mapGot) != 5 || len(jsonGot) != 5 {
		t.Fatalf("row count: Map %d, JSON %d, want 5 each", len(mapGot), len(jsonGot))
	}
	for i := 0; i < 4; i++ {
		if mapGot[i] != jsonGot[i] {
			t.Errorf("row %d: Map mapConcat = %s, JSON reconstruction+mapConcat = %s, want equal", i+1, mapGot[i], jsonGot[i])
		}
	}
	t.Logf("row 5 (beyond depth bound): Map = %s, JSON = %s", mapGot[4], jsonGot[4])
}

// TestJSONFullMapReconstruction_BeyondDepthBound_ChDB pins the DOCUMENTED
// limitation explicitly (maxJSONAttrFlattenDepth's doc comment): a key
// nested past the bound reconstructs with its remaining structure as an
// unparsed raw JSON substring under its partial dotted key — present and
// non-empty, never silently dropped — rather than fully flattening to the
// original dotted key.
func TestJSONFullMapReconstruction_BeyondDepthBound_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fullMapSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mapKeysExpr := &chplan.FuncCall{Fn: chplan.FnMapKeys, Args: []chplan.Expr{resourceAttrsCol()}}
	toString := &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{mapKeysExpr}}

	jsonSQL, jsonArgs := renderFullMapProject(t, "fullmap_json", fullMapStrategies, toString)
	jsonGot := queryToStringRows(t, db, jsonSQL, jsonArgs)
	if len(jsonGot) != 5 {
		t.Fatalf("row count = %d, want 5", len(jsonGot))
	}
	// Row 5 seeded a.b.c.d.e (5 segments); maxJSONAttrFlattenDepth fully
	// flattens up to 4-segment keys, so the reconstructed key set carries
	// the partial 4-segment prefix, never the full 5-segment original and
	// never nothing at all.
	row5 := jsonGot[4]
	if got := row5; got != "['a.b.c.d']" {
		t.Fatalf("row 5 mapKeys = %s, want ['a.b.c.d'] (the documented partial-key fallback)", got)
	}
}

// TestJSONFullMapReconstruction_MapFilter pins structuredMetadataExpr's
// exact shape: mapFilter dropping any (k, v) pair whose v is the empty string, against a bare attribute column.
func TestJSONFullMapReconstruction_MapFilter(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fullMapSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	filterExpr := func() chplan.Expr {
		return &chplan.FuncCall{
			Fn: chplan.FnMapFilter,
			Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{"k", "v"},
					Body: &chplan.Binary{
						Op:    chplan.OpNe,
						Left:  &chplan.BareIdent{Name: "v"},
						Right: &chplan.LitString{V: ""},
					},
				},
				resourceAttrsCol(),
			},
		}
	}
	mapSQL, mapArgs := renderFullMapProject(t, "fullmap_map", nil, canonicalMapString(filterExpr()))
	jsonSQL, jsonArgs := renderFullMapProject(t, "fullmap_json", fullMapStrategies, canonicalMapString(filterExpr()))

	mapGot := queryToStringRows(t, db, mapSQL, mapArgs)
	jsonGot := queryToStringRows(t, db, jsonSQL, jsonArgs)

	for i := 0; i < 4; i++ {
		if mapGot[i] != jsonGot[i] {
			t.Errorf("row %d: Map mapFilter = %s, JSON reconstruction+mapFilter = %s, want equal", i+1, mapGot[i], jsonGot[i])
		}
	}
	// Row 3's present-but-empty service.name must be DROPPED on both sides
	// — the case mapFilter exists to prove, distinct from mapConcat above.
	if mapGot[2] != "{}" {
		t.Fatalf("row 3 Map mapFilter = %s, want {} (present-but-empty value dropped)", mapGot[2])
	}
}

// TestJSONFullMapReconstruction_BareProjection pins projectedExpr's own
// case: a bare ResourceAttributes ColumnRef projected with NO wrapping
// function at all — the shape internal/logql's Lang.ProjectSamples emits
// when neither a label-mutating pipeline stage nor detected_level
// synthesis applies. Without emitProject's substitution this selects the
// raw JSON value instead of a genuine Map(String,String).
func TestJSONFullMapReconstruction_BareProjection(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fullMapSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The projected expr is the BARE ColumnRef itself — no toString/mapSort
	// wrapping — because projectedExpr only substitutes a bare
	// *chplan.ColumnRef; wrapping it here would defeat the exact case this
	// test exists to cover. The string comparison is applied OUTSIDE, at
	// the raw SQL text level, wrapping the rendered subquery (which itself
	// carries no ORDER BY — added here, once, at the outermost level).
	bareProject := func(table string) (string, []any) {
		scan := &chplan.Scan{Table: table}
		proj := &chplan.Project{
			Input: scan,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "id"}, Alias: "id"},
				{Expr: resourceAttrsCol(), Alias: "out"},
			},
		}
		ctx := context.Background()
		if table == "fullmap_json" {
			ctx = chsql.WithAttrStrategies(ctx, fullMapStrategies)
		}
		inner, args, err := chsql.Emit(ctx, proj)
		if err != nil {
			t.Fatalf("emit: %v", err)
		}
		return "SELECT toString(mapSort(out)) FROM (" + inner + ") ORDER BY id", args
	}

	mapSQL, mapArgs := bareProject("fullmap_map")
	jsonSQL, jsonArgs := bareProject("fullmap_json")

	mapGot := queryToStringRows(t, db, mapSQL, mapArgs)
	jsonGot := queryToStringRows(t, db, jsonSQL, jsonArgs)

	for i := 0; i < 4; i++ {
		if mapGot[i] != jsonGot[i] {
			t.Errorf("row %d: Map bare projection = %s, JSON reconstruction = %s, want equal", i+1, mapGot[i], jsonGot[i])
		}
	}
}

// TestJSONFullMapReconstruction_MapValues pins mapValues, TraceQL's
// compare() fan-out's own building block (internal/traceql/metrics_compare.go)
// alongside mapKeys — both resolve through the same jsonFullMapFns
// substitution since they share the chsql emitter.
func TestJSONFullMapReconstruction_MapValues(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(fullMapSeedDDL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	valuesExpr := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnMapValues, Args: []chplan.Expr{resourceAttrsCol()}},
	}}
	toString := &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{valuesExpr}}

	mapSQL, mapArgs := renderFullMapProject(t, "fullmap_map", nil, toString)
	jsonSQL, jsonArgs := renderFullMapProject(t, "fullmap_json", fullMapStrategies, toString)

	mapGot := queryToStringRows(t, db, mapSQL, mapArgs)
	jsonGot := queryToStringRows(t, db, jsonSQL, jsonArgs)

	for i := 0; i < 4; i++ {
		if mapGot[i] != jsonGot[i] {
			t.Errorf("row %d: Map mapValues = %s, JSON reconstruction+mapValues = %s, want equal", i+1, mapGot[i], jsonGot[i])
		}
	}
}
