//go:build chdb

// chDB-backed proof for the key-enumeration half of cerberus issue #2775:
// the three label-name / tag-name discovery endpoints (Loki
// internal/api/loki/labels.go, Prom internal/api/prom/metadata.go, Tempo
// internal/api/tempo/search_tags.go) now emit `arrayJoin(<col>.keys)`
// instead of `arrayJoin(mapKeys(<col>))`. All three funnel through the
// SAME chsql.Qual(col, "keys") construction this file exercises directly,
// so one test covers the shared mechanism rather than three near-identical
// per-package copies.
//
// The existence-pre-filter half of #2775 (mapContains(<col>, ?) ->
// has(<col>.keys, ?)) is DELIBERATELY NOT implemented — see
// search_tag_values.go's mapContainsFrag doc comment for the chDB
// investigation (EXPLAIN QUERY TREE + EXPLAIN indexes=1 on a real
// ClickHouse 26.5.1.1) that found the subcolumn spelling loses
// skip-index-match compatibility with a conventionally-declared
// `INDEX ... mapKeys(<col>) TYPE bloom_filter` index, with no
// confirmed read-cost win to offset that regression.
package chsql_test

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// TestMapKeysSubcolumn_MatchesMapKeys_ChDB seeds a Map column with several
// distinct keys and asserts arrayJoin(<col>.keys) returns the EXACT same
// distinct key set as the arrayJoin(mapKeys(<col>)) idiom it replaces —
// the semantics-preservation half of #2775's "definitionally identical"
// claim, proven against a real ClickHouse engine rather than asserted.
func TestMapKeysSubcolumn_MatchesMapKeys_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(`CREATE TABLE t (
		Key        String,
		Attributes Map(String, String)
	) ENGINE = MergeTree ORDER BY Key`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES
		('r1', map('service.name', 'api', 'env', 'prod')),
		('r2', map('service.name', 'api', 'region', 'us')),
		('r3', map())`); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	oldForm := mustDistinctKeys(t, db, "arrayJoin(mapKeys(Attributes))")
	newForm := mustDistinctKeys(t, db, "arrayJoin(Attributes.keys)")

	if len(oldForm) == 0 {
		t.Fatal("sanity: expected at least one key from the seeded rows")
	}
	if !equalStrings(oldForm, newForm) {
		t.Fatalf("arrayJoin(Attributes.keys) = %v; want the same set as arrayJoin(mapKeys(Attributes)) = %v", newForm, oldForm)
	}
}

// TestMapKeysSubcolumn_ReadsOnlyKeysStream_ChDB proves the read-cost claim
// (cerberus issue #2775's motivating win): on a default-configured server
// (optimize_functions_to_subcolumns and the analyzer both default-on),
// ClickHouse's ReadFromMergeTree step for arrayJoin(mapKeys(<col>)) ALREADY
// projects down to the same `<col>.keys Array(String)` physical subcolumn
// arrayJoin(<col>.keys) requests directly — so the explicit spelling is
// execution-cost-neutral on a healthy default deployment, and its value is
// exactly the issue's own framing: the win becomes UNCONDITIONAL (no longer
// contingent on that setting) rather than a new win on top of it. Unlike
// the query-tree rewrite mapContains(<col>, ?) receives (which this file's
// package doc explains does NOT carry through to a matching ReadFromMergeTree
// projection — see search_tag_values.go), the mapKeys(<col>) form's
// subcolumn projection is confirmed end-to-end here.
func TestMapKeysSubcolumn_ReadsOnlyKeysStream_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(`CREATE TABLE t (
		Key        String,
		Attributes Map(String, String)
	) ENGINE = MergeTree ORDER BY Key`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES ('r1', map('alpha', 'v'))`); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	oldHeader := mustReadHeader(t, db, "arrayJoin(mapKeys(Attributes))")
	newHeader := mustReadHeader(t, db, "arrayJoin(Attributes.keys)")

	const wantSubstring = "Attributes.keys Array(String)"
	if !strings.Contains(oldHeader, wantSubstring) {
		t.Errorf("arrayJoin(mapKeys(Attributes)) ReadFromMergeTree header = %q; want it to contain %q (the analyzer's own subcolumn projection)", oldHeader, wantSubstring)
	}
	if !strings.Contains(newHeader, wantSubstring) {
		t.Errorf("arrayJoin(Attributes.keys) ReadFromMergeTree header = %q; want it to contain %q", newHeader, wantSubstring)
	}
}

// mustDistinctKeys runs `SELECT DISTINCT <keysExpr> AS k FROM t ORDER BY k`
// and returns the sorted key list.
func mustDistinctKeys(t *testing.T, db *sql.DB, keysExpr string) []string {
	t.Helper()
	rows, err := db.Query("SELECT DISTINCT " + keysExpr + " AS k FROM t ORDER BY k")
	if err != nil {
		t.Fatalf("query %s: %v", keysExpr, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// mustReadHeader runs `EXPLAIN PLAN header = 1 SELECT DISTINCT <keysExpr>
// FROM t` and returns the ReadFromMergeTree step's declared Header line —
// the physical column projection ClickHouse's storage layer pulls off
// disk, which chsql.Qual(col, "keys")'s doc comment cites as the direct
// (non-optimizer-dependent) proof of the keys-only decode.
func mustReadHeader(t *testing.T, db *sql.DB, keysExpr string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN PLAN header = 1 SELECT DISTINCT " + keysExpr + " FROM t")
	if err != nil {
		t.Fatalf("explain %s: %v", keysExpr, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return plan.String()
}

// equalStrings reports whether a and b hold the same elements in the same
// order (both inputs are pre-sorted by the caller).
func equalStrings(a, b []string) bool {
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

// TestMapKeysSubcolumnQual_RendersDottedIdentifier pins the exact SQL
// chsql.Qual(col, "keys") produces — a backtick-quoted qualifier.name pair
// — without a live database, guarding the emitter shape the two chDB tests
// above exercise end-to-end.
func TestMapKeysSubcolumnQual_RendersDottedIdentifier(t *testing.T) {
	frag := chsql.Call("arrayJoin", chsql.Qual("ResourceAttributes", "keys"))
	b := chsql.NewBuilder()
	frag(b)
	got, _, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := "arrayJoin(`ResourceAttributes`.`keys`)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
