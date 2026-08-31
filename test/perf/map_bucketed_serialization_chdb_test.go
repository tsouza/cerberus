//go:build chdb

// Correctness + read-amplification evidence for chopt's
// map_bucketed_serialization feature (cerberus issue #2774), run against a
// real ClickHouse engine (chDB 26.5.1.1 — see chdb_substrate in
// versions.yaml) rather than taken on the ClickHouse docs/blog's word alone.
//
// Two claims this file backs:
//
//  1. CORRECTNESS: a bucketed Map column (map_serialization_version=
//     'with_buckets') answers a subscript read, a mapContains predicate, and
//     a full-map read IDENTICALLY to the same data under the server's
//     default 'basic' serialization — the read side genuinely needs no
//     cerberus code change, chopt.FeatureMapBucketedSerialization's central
//     claim. The full-map comparison runs BOTH sides through mapSort, which
//     pins this feature's own documented precondition: pre-26.8
//     with_buckets does not preserve key order, so an unsorted comparison
//     would legitimately differ in ORDER while agreeing in CONTENT — the
//     exact hazard cerberus's own mapSort canonicalization
//     (internal/chplan/canonical_attributes.go) already guards every
//     stream-identity read against.
//  2. PERFORMANCE: a single-key subscript read's wall time relative to a
//     full-map read shrinks as the map widens, under bucketing — mirroring
//     the ClickHouse blog's own methodology of varying map size and timing
//     a single-key read. This file LOGS the measurement (t.Logf) rather
//     than gating CI on it: chDB carries no system.query_log (confirmed
//     directly against this build — see internal/chclienttest's own "chDB
//     has no query_log" note), so there is no deterministic byte-read count
//     available to assert against, and chDB is a single embedded-process
//     engine where raw wall time carries real CGO-boundary and scheduler
//     noise — exactly what this repo's own PERMANENT regression gate
//     (scale_wall_pin_chdb_test.go) goes to real lengths (a same-process
//     ratio against an in-run yardstick, a calibrated baseline file) to
//     cancel. map_bucketed_serialization is opt-in and experimental
//     (chopt.FeatureMapBucketedSerialization's AutoSelect is false)
//     precisely because both the win and the documented ~2x full-map-read
//     cost need a real fielded before/after — a one-off logged measurement,
//     not a calibrated CI gate, is the right amount of apparatus for a
//     feature no server runs by default.
//
// Build-tagged chdb; rides the same perf-chdb lane as the rest of this
// package (`just perf-chdb` = `go test -tags chdb ./test/perf/...`).
package perf

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// mapBucketRows is the row count seeded into every table this file builds:
// enough for the timing measurement in
// TestMapBucketedSerialization_SingleKeyReadTiming to be meaningful, small
// enough that OPTIMIZE TABLE ... FINAL — required for the bucketed layout to
// actually apply, since bucketing happens as merges rewrite parts, not at
// INSERT time — stays fast across every (key-count, bucketed/basic) table
// this file creates.
const mapBucketRows = 20_000

// mapBucketDDL renders a minimal Attributes-shaped table — an id and one Map
// column — carrying the SAME SETTINGS tail
// internal/schemaboot.mapBucketedSettings would add to a real logs/traces
// table when chopt.FeatureMapBucketedSerialization is enabled (withBuckets
// true), or the server's default 'basic' serialization otherwise.
func mapBucketDDL(table string, withBuckets bool) string {
	settings := "index_granularity = 8192"
	if withBuckets {
		settings = "map_serialization_version = 'with_buckets', " + settings
	}
	return fmt.Sprintf(
		`CREATE TABLE %s (id UInt64, m Map(String, String)) ENGINE = MergeTree ORDER BY id SETTINGS %s`,
		table, settings,
	)
}

// mapBucketKeySubscript / mapBucketKeyValue name the ONE key/value pair every
// seeded row carries identically — mapBucketSeed writes it as key index 0 of
// every row's map, so a predicate on it is guaranteed to match every row
// regardless of map width, giving every correctness/timing query below a
// fixed, table-independent expected row count (mapBucketRows).
const (
	mapBucketKeySubscript = "k0"
	mapBucketKeyValue     = "pinned-value"
)

// mapBucketSeed inserts mapBucketRows rows, each carrying a map of exactly
// keyCount entries: key 0 is always (mapBucketKeySubscript,
// mapBucketKeyValue) — the fixed pair every correctness/timing query
// filters on — and keys 1..keyCount-1 are synthetic filler
// ("k<i>" -> "v<id>_<i>") that widens the map without touching what the
// filter matches, so keyCount can vary while the expected row count stays
// mapBucketRows.
func mapBucketSeed(table string, keyCount int) string {
	if keyCount < 1 {
		keyCount = 1
	}
	pairs := make([]string, keyCount)
	pairs[0] = fmt.Sprintf("'%s', '%s'", mapBucketKeySubscript, mapBucketKeyValue)
	for i := 1; i < keyCount; i++ {
		pairs[i] = fmt.Sprintf("concat('k', toString(%d)), concat('v', toString(number), '_', toString(%d))", i, i)
	}
	return fmt.Sprintf(
		"INSERT INTO %s SELECT number AS id, map(%s) FROM numbers(%d)",
		table, strings.Join(pairs, ", "), mapBucketRows,
	)
}

// openMapBucketDB opens a fresh in-process chDB session for one subtest —
// mirroring every other *_chdb_test.go in this package (sql.Open("chdb",
// "") provisions an isolated temp-dir-backed session torn down on Close).
func openMapBucketDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mapBucketBuildTable creates + seeds + OPTIMIZE ... FINALs one table, so the
// bucketed layout (which only applies as merges rewrite parts) is actually in
// force by the time any query below runs against it.
func mapBucketBuildTable(t *testing.T, db *sql.DB, table string, withBuckets bool, keyCount int) {
	t.Helper()
	for _, stmt := range []string{
		mapBucketDDL(table, withBuckets),
		mapBucketSeed(table, keyCount),
		"OPTIMIZE TABLE " + table + " FINAL",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup exec failed:\n%s\nerr: %v", stmt, err)
		}
	}
}

// mapBucketScalarCount runs a scalar `SELECT count() ...`-shaped query and
// returns the count, failing the test on any error.
func mapBucketScalarCount(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query: %v\nquery: %s", err, query)
	}
	return n
}

// mapBucketSortedMaps reads every row's map, canonicalised via mapSort (so a
// with_buckets table's key-order variance — pre-26.8 does not preserve
// insertion order — cannot itself produce a mismatch), rendered to its
// deterministic toString() form and ordered by id. Two tables seeded with
// the same rows must return byte-identical slices from this regardless of
// their serialization setting; that equality IS the correctness claim.
func mapBucketSortedMaps(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("SELECT toString(mapSort(m)) FROM %s ORDER BY id", table))
	if err != nil {
		t.Fatalf("read sorted maps from %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan sorted map from %s: %v", table, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sorted maps from %s: %v", table, err)
	}
	return out
}

// TestMapBucketedSerialization_DDLAppliesSetting pins that a table created
// WITH the setting actually carries it in the server's own rendering of the
// schema (SHOW CREATE TABLE) — the deterministic, non-flaky half of "the DDL
// this feature emits is valid, real ClickHouse DDL a real server accepts",
// exercised here since the docker-gated real-ClickHouse compat lanes are not
// guaranteed available in every environment this test runs in.
func TestMapBucketedSerialization_DDLAppliesSetting(t *testing.T) {
	db := openMapBucketDB(t)
	const table = "mb_show_create"
	if _, err := db.Exec(mapBucketDDL(table, true)); err != nil {
		t.Fatalf("create: %v", err)
	}
	var showCreate string
	if err := db.QueryRow("SHOW CREATE TABLE " + table).Scan(&showCreate); err != nil {
		t.Fatalf("show create: %v", err)
	}
	if !strings.Contains(showCreate, "map_serialization_version = 'with_buckets'") {
		t.Errorf("SHOW CREATE TABLE did not carry the bucketed setting:\n%s", showCreate)
	}
}

// TestMapBucketedSerialization_Correctness is the primary claim: at several
// map widths, a bucketed table and a basic table seeded with IDENTICAL rows
// answer a subscript read, a mapContains predicate, and a full-map read the
// same way. This is the chDB roundtrip proving bucketed reads return the
// same values as unbucketed ones — the empirical backing for chopt's
// FeatureMapBucketedSerialization doc comment's claim that the read side
// needs no cerberus code change.
func TestMapBucketedSerialization_Correctness(t *testing.T) {
	for _, keyCount := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("keys=%d", keyCount), func(t *testing.T) {
			// chDB sessions opened within one test binary process are not
			// isolated from each other's tables (compare_window_prune_chdb_
			// test.go's own CREATE OR REPLACE TABLE navigates the same
			// thing), so table names are suffixed per keyCount to keep
			// sibling subtests from colliding.
			db := openMapBucketDB(t)
			basic, bucketed := fmt.Sprintf("mb_basic_%d", keyCount), fmt.Sprintf("mb_bucketed_%d", keyCount)
			mapBucketBuildTable(t, db, basic, false, keyCount)
			mapBucketBuildTable(t, db, bucketed, true, keyCount)

			subscriptSQL := func(table string) string {
				return fmt.Sprintf("SELECT count() FROM %s WHERE m['%s'] = '%s'",
					table, mapBucketKeySubscript, mapBucketKeyValue)
			}
			if got := mapBucketScalarCount(t, db, subscriptSQL(basic)); got != mapBucketRows {
				t.Fatalf("basic subscript read: got %d rows, want %d — seed is degenerate", got, mapBucketRows)
			}
			if got := mapBucketScalarCount(t, db, subscriptSQL(bucketed)); got != mapBucketRows {
				t.Errorf("bucketed subscript read diverges from basic: got %d rows, want %d", got, mapBucketRows)
			}

			containsSQL := func(table string) string {
				return fmt.Sprintf("SELECT count() FROM %s WHERE mapContains(m, '%s')",
					table, mapBucketKeySubscript)
			}
			if got := mapBucketScalarCount(t, db, containsSQL(basic)); got != mapBucketRows {
				t.Fatalf("basic mapContains read: got %d rows, want %d — seed is degenerate", got, mapBucketRows)
			}
			if got := mapBucketScalarCount(t, db, containsSQL(bucketed)); got != mapBucketRows {
				t.Errorf("bucketed mapContains read diverges from basic: got %d rows, want %d", got, mapBucketRows)
			}

			// Full-map read: mapSort on BOTH sides neutralises the pre-26.8
			// key-order variance with_buckets introduces, so any remaining
			// difference is a genuine CONTENT divergence, not an order one.
			basicMaps := mapBucketSortedMaps(t, db, basic)
			bucketedMaps := mapBucketSortedMaps(t, db, bucketed)
			if len(basicMaps) != len(bucketedMaps) {
				t.Fatalf("full-map read row count: basic=%d bucketed=%d", len(basicMaps), len(bucketedMaps))
			}
			for i := range basicMaps {
				if basicMaps[i] != bucketedMaps[i] {
					t.Errorf("full-map read row %d diverges:\nbasic:    %s\nbucketed: %s", i, basicMaps[i], bucketedMaps[i])
				}
			}
		})
	}
}

// mapBucketTimingKeyCounts are the map widths
// TestMapBucketedSerialization_SingleKeyReadTiming measures at, mirroring
// the ClickHouse blog's own "vary map size, measure a single-key read"
// methodology cited in chopt.FeatureMapBucketedSerialization's doc comment.
var mapBucketTimingKeyCounts = []int{32, 256, 2048}

// mapBucketTimingIters is the best-of-N repeat count bestOfWall runs per
// timed query — matching scale_wall_pin_chdb_test.go's own floor rationale
// (the minimum strips GC / scheduler jitter).
const mapBucketTimingIters = 5

// TestMapBucketedSerialization_SingleKeyReadTiming is the performance half:
// for each map width, times (best-of-N, via the SAME bestOfWall helper
// scale_wall_pin_chdb_test.go's permanent regression gate uses) a
// single-key subscript read against a full-map read, on both a basic and a
// bucketed table of that width, and logs the single-key-to-full-map wall
// ratio for each. The mechanism this backs: bucketing should shrink that
// ratio as the map widens (a single-key read decompresses one bucket
// instead of the whole map), while the basic table's ratio should track the
// map width much less. See the file header for why this test LOGS rather
// than GATES on the numbers — chDB carries no system.query_log to derive a
// deterministic byte-read count from, and this is a one-off measurement for
// an opt-in, experimental feature, not a permanent CI invariant.
func TestMapBucketedSerialization_SingleKeyReadTiming(t *testing.T) {
	db := openMapBucketDB(t)
	subscriptSQL := func(table string) string {
		return fmt.Sprintf("SELECT count() FROM %s WHERE m['%s'] = '%s'",
			table, mapBucketKeySubscript, mapBucketKeyValue)
	}
	fullMapSQL := func(table string) string {
		// sum(length(m)) forces every row's WHOLE map to decode (length()
		// reads every key), unlike the subscript predicate above.
		return fmt.Sprintf("SELECT sum(length(m)) FROM %s", table)
	}

	for _, keyCount := range mapBucketTimingKeyCounts {
		const basic, bucketed = "mb_time_basic", "mb_time_bucketed"
		mapBucketBuildTable(t, db, basic, false, keyCount)
		mapBucketBuildTable(t, db, bucketed, true, keyCount)

		// Sanity: both queries must still see every row — a wrong count here
		// would silently invalidate the timing (a query erroring out or
		// scanning a degenerate table times "nothing", not "a single-key
		// read").
		if got := mapBucketScalarCount(t, db, subscriptSQL(basic)); got != mapBucketRows {
			t.Fatalf("keys=%d: basic subscript sanity count = %d, want %d", keyCount, got, mapBucketRows)
		}
		if got := mapBucketScalarCount(t, db, subscriptSQL(bucketed)); got != mapBucketRows {
			t.Fatalf("keys=%d: bucketed subscript sanity count = %d, want %d", keyCount, got, mapBucketRows)
		}

		basicSingle := bestOfWall(t, db, subscriptSQL(basic), nil, mapBucketTimingIters)
		basicFull := bestOfWall(t, db, fullMapSQL(basic), nil, mapBucketTimingIters)
		bucketedSingle := bestOfWall(t, db, subscriptSQL(bucketed), nil, mapBucketTimingIters)
		bucketedFull := bestOfWall(t, db, fullMapSQL(bucketed), nil, mapBucketTimingIters)

		basicRatio := float64(basicSingle) / float64(basicFull)
		bucketedRatio := float64(bucketedSingle) / float64(bucketedFull)
		t.Logf(
			"keys=%-5d basic:    single=%-12v full=%-12v single/full=%.3f",
			keyCount, basicSingle, basicFull, basicRatio,
		)
		t.Logf(
			"keys=%-5d bucketed: single=%-12v full=%-12v single/full=%.3f",
			keyCount, bucketedSingle, bucketedFull, bucketedRatio,
		)

		// Drop the tables so the next key-count iteration starts from a
		// clean slate — CREATE TABLE above carries no IF NOT EXISTS.
		for _, table := range []string{basic, bucketed} {
			if _, err := db.Exec("DROP TABLE " + table); err != nil {
				t.Fatalf("drop %s: %v", table, err)
			}
		}
	}
}
