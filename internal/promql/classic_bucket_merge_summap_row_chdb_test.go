//go:build chdb

// Row-level differential for the per-row half of classic_bucket_merge_summap.go.
//
// classic_bucket_merge_summap_chdb_test.go compares the two merge strategies
// END TO END, through the lowering entry point — which is the right layer for
// the merge's own answer, but cannot reach this half: at BOTH call sites the
// rows handed to the merge come from a per-series stage that already projects
// classicBucketUnionBoundsExpr (arraySort + arrayDistinct + finite-filtered)
// as their ExplicitBounds, so no query shape can seed the merge a row whose
// stored layout repeats a bound, is not ascending, or carries a non-finite
// entry.
//
// classicBucketSumMapRowArgs still has to answer such a row the way
// classicBucketRowCumulativeExpr does — the two constructions read the SAME
// rows and are meant to be interchangeable, and which per-series strategy
// feeds them is a boot-time feature decision (lower_strategy.go), not a
// property this file's expressions may assume. So the pair is executed
// DIRECTLY here, against deliberately malformed rows, and compared with the
// fold's own `b <= u` reading of the same row transcribed into plain
// ClickHouse.
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

// summapRowFoldReadingSQL is classicBucketRowCumulativeExpr's definition —
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

// TestClassicBucketSumMapRowArgs_MatchesFoldReading executes
// classicBucketSumMapRowArgs over every row in summapRowProbeSeed and
// asserts its (bound -> cumulative count) map is the one the has-filter
// fold reads off the same row.
func TestClassicBucketSumMapRowArgs_MatchesFoldReading(t *testing.T) {
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
	bounds, counts := classicBucketSumMapRowArgs(s)
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: summapRowProbeTable},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Case"}, Alias: "Case"},
			{Expr: bounds, Alias: "sm_bounds"},
			{Expr: counts, Alias: "sm_counts"},
			{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
			{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
		},
	}
	inner, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	rows, err := db.Query(`SELECT Case, `+
		`toString(arrayReduce('sumMap', [sm_bounds], [sm_counts])), `+
		`toString(`+summapRowFoldReadingSQL+`), `+
		`toString(length(sm_bounds) = length(sm_counts)) FROM (`+inner+`)`, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var name, got, want, equalLengths string
		if err := rows.Scan(&name, &got, &want, &equalLengths); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if got != want {
			t.Errorf("%s: classicBucketSumMapRowArgs reads %s, the has-filter fold reads %s",
				name, got, want)
		}
		// sumMap requires equal-length key/value arrays per row; a
		// mismatch is a runtime exception on a real query, not a wrong
		// answer, so it is worth pinning where it is cheap to see.
		// ClickHouse renders the UInt8 a comparison yields as 1 / 0.
		if equalLengths != "1" {
			t.Errorf("%s: bounds and counts arrays differ in length", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if want := strings.Count(summapRowProbeSeed, "\n    ('"); seen != want {
		t.Fatalf("compared %d rows, seed carries %d — the probe is not reading what it seeded", seen, want)
	}
}
