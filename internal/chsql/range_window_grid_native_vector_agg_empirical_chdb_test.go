//go:build chdb

// Empirical pin for the six ClickHouse `-ForEach` combinator behaviours
// chplan.RangeWindowGridNativeVectorAgg's own emitter (range_window_grid_
// native_vector_agg.go) relies on and the docs do not specify (cerberus
// issue #2763). Each subtest below is the smallest query that isolates one
// behaviour, run against a real chDB session (ClickHouse 26.5.1.1 at the
// time this was written) rather than assumed from the combinator's name.
//
// Every array here is Array(Nullable(Float64)) — the exact element type
// timeSeriesRateToGrid (and the rest of the timeSeries*ToGrid family)
// returns — so a row's per-position value is either a real Float64 or NULL
// (a grid point with too few in-window samples), matching production
// exactly.
package chsql_test

import (
	"database/sql"
	"testing"

	"github.com/tsouza/cerberus/internal/chsqltest"
)

// scanJSONArray runs query (which must project exactly one column via
// toString(...) over an Array-typed aggregate result) and returns that
// column's rendered text — chDB's Go driver cannot Scan an Array(Nullable(T))
// column directly, so every query here wraps its aggregate in toString(...)
// the same way the manual probe that first surfaced these behaviours did.
func scanJSONArray(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// TestForEachCombinator_SkipsNullPerPosition pins verification #1 and #3:
// sumForEach skips a NULL array element PER POSITION (not per row), and a
// position that is NULL across EVERY row in the group answers NULL — never
// 0 — exactly the "absent point" Prometheus semantics this node's emitter
// depends on to avoid manufacturing a point at a position no contributing
// series actually reports.
func TestForEachCombinator_SkipsNullPerPosition(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	// Position 0: every row present (1+2+3=6). Position 1: one row NULL, two
	// present (10+30=40). Position 2: EVERY row NULL.
	got := scanJSONArray(t, db, `SELECT toString(sumForEach(arr)) FROM (
		SELECT [toNullable(1.0), toNullable(10.0), CAST(NULL, 'Nullable(Float64)')] AS arr
		UNION ALL SELECT [toNullable(2.0), CAST(NULL, 'Nullable(Float64)'), CAST(NULL, 'Nullable(Float64)')] AS arr
		UNION ALL SELECT [toNullable(3.0), toNullable(30.0), CAST(NULL, 'Nullable(Float64)')] AS arr
	)`)
	want := "[6,40,NULL]"
	if got != want {
		t.Errorf("sumForEach per-position NULL skip = %s, want %s", got, want)
	}
}

// TestForEachCombinator_AllFiveFnsSkipNullPerPosition extends the pin above
// to every one of the five ForEach combinators nativeGridVectorAggFn maps
// (sum/min/max/avg/count), each over a realistic GROUP-BY-keyed shape
// (multiple output groups in one query, mirroring the vecAgg level's own
// SELECT/GROUP BY), confirming per-position NULL-skip holds uniformly.
func TestForEachCombinator_AllFiveFnsSkipNullPerPosition(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	const seed = `(
		SELECT 'jobA' AS key, [toNullable(1.0), toNullable(2.0)] AS arr
		UNION ALL SELECT 'jobA' AS key, [toNullable(10.0), CAST(NULL,'Nullable(Float64)')] AS arr
		UNION ALL SELECT 'jobB' AS key, [toNullable(100.0), toNullable(200.0)] AS arr
	)`

	cases := []struct {
		fn   string
		want string
	}{
		// position 1 of jobA: only the first row (2.0) is present, so
		// sum/min/max/avg all reduce to that lone contributor.
		{"sumForEach", "[11,2]"},
		{"minForEach", "[1,2]"},
		{"maxForEach", "[10,2]"},
		{"avgForEach", "[5.5,2]"},
		{"countForEach", "[2,1]"},
	}
	for _, c := range cases {
		t.Run(c.fn, func(t *testing.T) {
			got := scanJSONArray(t, db,
				`SELECT toString(`+c.fn+`(arr)) FROM `+seed+` WHERE key = 'jobA' GROUP BY key`)
			if got != c.want {
				t.Errorf("%s(jobA) = %s, want %s", c.fn, got, c.want)
			}
		})
	}
}

// TestForEachCombinator_OrNullComposesIdentically pins verification #2: the
// two composition orders — sumOrNullForEach ("apply OrNull to the per-
// position sum, THEN ForEach it") and sumForEachOrNull ("ForEach the sum,
// THEN apply OrNull to the whole array") — answer IDENTICALLY for both an
// all-NULL position within a non-empty group and a genuinely EMPTY group.
// This is the empirical finding that justifies range_window_grid_native_
// vector_agg.go using the PLAIN combinator form (no -OrNull at all): the
// array element type is already Nullable, so plain sumForEach already
// reports NULL wherever either OrNull variant would, making the extra
// combinator layer observably inert for this node's shape.
func TestForEachCombinator_OrNullComposesIdentically(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	t.Run("all_null_position_in_nonempty_group", func(t *testing.T) {
		const seed = `(
			SELECT [toNullable(1.0), CAST(NULL,'Nullable(Float64)')] AS arr
			UNION ALL SELECT [toNullable(2.0), CAST(NULL,'Nullable(Float64)')] AS arr
		)`
		orNullFirst := scanJSONArray(t, db, `SELECT toString(sumOrNullForEach(arr)) FROM `+seed)
		forEachFirst := scanJSONArray(t, db, `SELECT toString(sumForEachOrNull(arr)) FROM `+seed)
		plain := scanJSONArray(t, db, `SELECT toString(sumForEach(arr)) FROM `+seed)
		if orNullFirst != forEachFirst || orNullFirst != plain {
			t.Errorf("sumOrNullForEach=%s sumForEachOrNull=%s sumForEach=%s; want all three equal",
				orNullFirst, forEachFirst, plain)
		}
	})

	t.Run("empty_group", func(t *testing.T) {
		const seed = `(SELECT [toNullable(1.0)] AS arr WHERE 1 = 0)`
		orNullFirst := scanJSONArray(t, db, `SELECT toString(sumOrNullForEach(arr)) FROM `+seed)
		forEachFirst := scanJSONArray(t, db, `SELECT toString(sumForEachOrNull(arr)) FROM `+seed)
		if orNullFirst != forEachFirst {
			t.Errorf("sumOrNullForEach=%s sumForEachOrNull=%s on an empty group; want equal",
				orNullFirst, forEachFirst)
		}
	})
}

// TestForEachCombinator_NaNPropagatesThroughSum pins verification #4: NaN is
// NOT treated like NULL. A NaN element is a PRESENT value that poisons the
// sum (Prometheus arithmetic semantics — a NaN sample propagates, it is
// never silently dropped like an absent one), while countForEach counts the
// NaN-bearing row as present (2), not absent (1).
func TestForEachCombinator_NaNPropagatesThroughSum(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	const seed = `(
		SELECT [toNullable(1.0)] AS arr
		UNION ALL SELECT [toNullable(nan)] AS arr
		UNION ALL SELECT [toNullable(3.0)] AS arr
	)`
	if got := scanJSONArray(t, db, `SELECT toString(sumForEach(arr)) FROM `+seed); got != "[nan]" {
		t.Errorf("sumForEach with a NaN contributor = %s, want [nan] (NaN must propagate, not be skipped)", got)
	}

	const countSeed = `(
		SELECT [toNullable(1.0)] AS arr
		UNION ALL SELECT [toNullable(nan)] AS arr
	)`
	if got := scanJSONArray(t, db, `SELECT toString(countForEach(arr)) FROM `+countSeed); got != "[2]" {
		t.Errorf("countForEach with a NaN (present, non-NULL) contributor = %s, want [2]", got)
	}
}

// TestForEachCombinator_AvgDividesByPresentCount pins verification #5:
// avgForEach's per-position denominator is the count of PRESENT (non-NULL)
// values AT THAT POSITION, not the group's total row count — so a position
// where only one of three rows reports a value still averages correctly
// over just that one contributor (10, not 10/3).
func TestForEachCombinator_AvgDividesByPresentCount(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	got := scanJSONArray(t, db, `SELECT toString(avgForEach(arr)) FROM (
		SELECT [toNullable(10.0), toNullable(10.0)] AS arr
		UNION ALL SELECT [toNullable(20.0), CAST(NULL,'Nullable(Float64)')] AS arr
		UNION ALL SELECT [toNullable(30.0), CAST(NULL,'Nullable(Float64)')] AS arr
	)`)
	want := "[20,10]"
	if got != want {
		t.Errorf("avgForEach per-position present-count divisor = %s, want %s "+
			"(position 1 must average the single present value 10, not divide by the row count 3)", got, want)
	}
}

// TestForEachCombinator_CountAllNullPositionIsZeroNotNull pins verification
// #6: countForEach's result type is Array(UInt64) — no Nullable wrapper —
// so an all-NULL position renders a literal 0, never NULL. This is exactly
// why range_window_grid_native_vector_agg.go's emitter filters the FnCount
// explode with `grid_val != 0` rather than reusing the `IS NOT NULL` filter
// the other four Fns share: a real Prometheus count() is always >= 1 when a
// point is present at all, so a 0 here is never a legitimate answer, only
// the family's absent-point sentinel wearing a different type.
func TestForEachCombinator_CountAllNullPositionIsZeroNotNull(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	var text, typeName string
	err := db.QueryRow(`SELECT toString(countForEach(arr)), toTypeName(countForEach(arr)) FROM (
		SELECT [toNullable(1.0), CAST(NULL,'Nullable(Float64)')] AS arr
		UNION ALL SELECT [toNullable(2.0), CAST(NULL,'Nullable(Float64)')] AS arr
	)`).Scan(&text, &typeName)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if text != "[2,0]" {
		t.Errorf("countForEach = %s, want [2,0] (position 1 is all-NULL across every row -> literal 0)", text)
	}
	if typeName != "Array(UInt64)" {
		t.Errorf("countForEach result type = %s, want Array(UInt64) (no Nullable wrapper)", typeName)
	}
}
