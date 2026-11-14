//go:build chdb

// Row-level differential for the per-row reduction every classic-histogram
// layout merge shares (classicBucketRowEntriesExpr, classic_bucket_bound_runs.go),
// executed through the sumMap path's own per-row argument.
//
// classic_bucket_merge_summap_chdb_test.go compares the two merge strategies
// END TO END, through the lowering entry point — which is the right layer for
// the merge's own answer, but cannot reach this half: at BOTH call sites the
// rows handed to the merge come from a per-series stage that already projects
// a sorted, distinct, finite layout as their ExplicitBounds, so no query
// shape can seed the merge a row whose stored layout repeats a bound, is not
// ascending, or carries a non-finite entry.
//
// The reduction still has to answer such a row the way the has-filter fold
// reads it — "sum every bucket whose OWN bound is finite and <= u", at every
// distinct finite bound u the row carries — because which per-series strategy
// feeds the merge is a boot-time feature decision (lower_strategy.go), not a
// property the expression may assume. So it is executed DIRECTLY here,
// against deliberately malformed rows, and compared with that reading
// transcribed into plain ClickHouse.
package promql

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// summapRowProbeTable is this file's own table — the process-shared chDB
// session makes every fixture's tables visible to every other test in the
// binary (fixture_chdb_test.go's header), so the name must not collide.
const summapRowProbeTable = "classic_bucket_merge_summap_row_probe"

const summapRowProbeSeed = `
CREATE OR REPLACE TABLE ` + summapRowProbeTable + ` (
    Case String,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = Memory;
INSERT INTO ` + summapRowProbeTable + ` VALUES
    ('ascending distinct', [2, 3, 4], [1.0, 2.0, 5.0]),
    ('repeated bound', [2, 3, 4], [1.0, 1.0, 5.0]),
    ('descending bounds', [6, 1], [5.0, 2.0]),
    ('unordered with repeat', [1, 2, 3, 4], [5.0, 1.0, 2.0, 1.0]),
    ('leading empty buckets', [0, 0, 5], [1.0, 2.0, 3.0]),
    ('non-finite interior bound', [2, 7, 4], [1.0, inf, 3.0]),
    ('negative infinity bound', [9, 2, 4], [-inf, 1.0, 3.0]),
    ('trailing overflow element', [2, 3, 4, 7], [1.0, 2.0, 5.0]),
    ('single bucket', [5], [1.0]);
`

// summapRowFoldReadingSQL is the has-filter fold's per-row reading —
// "sum every bucket whose OWN bound is finite and <= u" — transcribed into
// plain ClickHouse and evaluated at every distinct finite bound the row
// carries, paired as a (bound, cumulative count) map. It is deliberately
// written from the fold's semantics rather than built from this package's
// expression trees: a shared builder would move in lockstep with the code
// under test and prove nothing.
//
// Both sides are folded through arrayReduce('sumMap', …) before comparison,
// which is what collapses this file's own duplicate keys (sumMap sums a
// row's repeated keys) and, on BOTH sides equally, drops a key whose value
// is exactly zero.
const summapRowFoldReadingSQL = `arrayReduce('sumMap',
    [arraySort(arrayDistinct(arrayFilter(b -> isFinite(b), ExplicitBounds)))],
    [arrayMap(u -> arraySum(arrayMap((b, c) -> if(isFinite(b) AND b <= u, toFloat64(c), 0.),
                                     ExplicitBounds,
                                     arraySlice(BucketCounts, 1, length(ExplicitBounds)))),
              arraySort(arrayDistinct(arrayFilter(b -> isFinite(b), ExplicitBounds))))])`

// summapRowDistinctBoundsSQL is the bound set the reduction must key the row
// by: every distinct finite bound it carries, ascending.
const summapRowDistinctBoundsSQL = `arraySort(arrayDistinct(arrayFilter(b -> isFinite(b), ExplicitBounds)))`

// TestClassicBucketSumMapRowEntries_MatchesFoldReading executes
// classicBucketSumMapRowEntriesExpr over every row in summapRowProbeSeed and
// asserts its (bound -> cumulative count) map is the one the has-filter
// fold reads off the same row, that it keys the row by exactly its distinct
// finite bounds in ascending order, and that each entry counts once.
func TestClassicBucketSumMapRowEntries_MatchesFoldReading(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range testsql.SplitStatements(summapRowProbeSeed) {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	s := schema.DefaultOTelMetrics()
	entries := classicBucketSumMapRowEntriesExpr(s)
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: summapRowProbeTable},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Case"}, Alias: "Case"},
			{Expr: entries, Alias: "sm_entries"},
			{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
			{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
		},
	}
	inner, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	rows, err := db.Query(`SELECT Case, `+
		`toString(arrayReduce('sumMap', [sm_entries.1], [sm_entries.2])), `+
		`toString(`+summapRowFoldReadingSQL+`), `+
		`toString(sm_entries.1), toString(`+summapRowDistinctBoundsSQL+`), `+
		`toString(sm_entries.3) FROM (`+inner+`)`, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var name, got, want, keys, wantKeys, ones string
		if err := rows.Scan(&name, &got, &want, &keys, &wantKeys, &ones); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if got != want {
			t.Errorf("%s: the row reduction reads %s, the has-filter fold reads %s",
				name, got, want)
		}
		if keys != wantKeys {
			t.Errorf("%s: the row reduction keys the row by %s, want its distinct finite bounds %s",
				name, keys, wantKeys)
		}
		// One count per entry: sumMap keys the merged layout by them, and
		// an entry counted twice would double a bound's weight nowhere the
		// ladder shows, while a missing one would let sumMap drop the key.
		if wantOnes := entryOnes(wantKeys); ones != wantOnes {
			t.Errorf("%s: per-entry counts %s, want %s", name, ones, wantOnes)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if want := strings.Count(summapRowProbeSeed, "\n    ('"); seen != want {
		t.Fatalf("compared %d rows, seed carries %d — the probe is not reading what it seeded", seen, want)
	}
}

// entryOnes renders the all-ones array ClickHouse prints for a row whose
// distinct finite bounds render as keys (`[1,2,5]` -> `[1,1,1]`).
func entryOnes(keys string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(keys, "["), "]")
	if inner == "" {
		return "[]"
	}
	return "[" + strings.TrimSuffix(strings.Repeat("1,", strings.Count(inner, ",")+1), ",") + "]"
}
