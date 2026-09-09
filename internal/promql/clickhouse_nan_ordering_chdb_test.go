//go:build chdb

// chDB-backed pin on the ClickHouse behaviour cerberus's `max`/`min` and
// `topk`/`bottomk` lowerings SILENTLY depend on: how NaN participates in
// aggregation and in ordering.
//
// Neither lowering emits any NaN handling of its own. `max(v)` /
// `min(v)` lower to ClickHouse's own `max`/`min`, and `topk(k, v)` /
// `bottomk(k, v)` lower to an `ORDER BY <Value> [DESC]` plus either
// `LIMIT k BY …` (literal k) or `row_number() OVER (… ORDER BY <Value>
// [DESC])` (computed k — see internal/chsql's emitTopK /
// emitTopKComputed). Every one of those answers is therefore whatever
// ClickHouse decides, and cerberus is only correct while ClickHouse
// happens to agree with Prometheus.
//
// Prometheus's rules, which this test is protecting:
//
//   - `max` / `min` treat NaN as ABSENT: the accumulator is replaced
//     when the incoming sample is better `|| math.IsNaN(group.floatValue)`
//     (the `parser.MAX` and `parser.MIN` arms of `promql/engine.go`'s
//     `aggregation`), so a NaN accumulator loses to any real value and a
//     group of nothing but NaN stays NaN.
//   - `topk` / `bottomk` displace the heap root when the incoming sample
//     is better `|| (math.IsNaN(group.heap[0].F) && !math.IsNaN(s.F))`
//     (the `parser.TOPK` and `parser.BOTTOMK` arms of
//     `promql/engine.go`'s `aggregationK`) — a NaN in the heap is evicted
//     by any real value, in BOTH directions. Expressed as an ordering,
//     that is "NaN sorts LAST whether the sort is ascending or
//     descending", which is not the same as a total order: it is
//     deliberately NOT the "NaN compares greatest" rule ClickHouse
//     documents for its own comparison operators.
//
// Measured on ClickHouse 26.5, `max`/`min` ignore NaN and `ORDER BY`
// places NaN last in BOTH directions, so cerberus already agrees. That
// agreement is what nothing in the tree pinned before this file: a
// future ClickHouse that made `ORDER BY … DESC` put NaN FIRST — the
// reading its own "NaN is greater than any number" comparison rule
// suggests — would flip every `topk` answer over a series set containing
// a NaN, with no failing test anywhere in the repository.
package promql_test

import (
	"database/sql"
	"math"
	"sort"
	"strings"
	"testing"
)

// nanOrderingSeed builds the probe table.
//
// The rows are inserted in FOUR separate statements on purpose. Each
// INSERT lands its own part, so the reader merges several blocks rather
// than sorting one — the shape in which a comparator that only mis-ranks
// NaN at a block boundary would show up. Within those parts the NaNs sit
// FIRST in one and LAST in another, so a comparator that merely
// preserved insertion order could not pass both directions.
//
// `CREATE OR REPLACE` because the chDB session outlives any one test
// (see fixture_chdb_test.go).
const nanOrderingSeed = `
CREATE OR REPLACE TABLE nan_ordering_probe (
    grp String,
    id String,
    v Float64
) ENGINE = MergeTree ORDER BY (grp, id);
INSERT INTO nan_ordering_probe VALUES ('mixed', 'n1', nan), ('mixed', 'a', 5.0);
INSERT INTO nan_ordering_probe VALUES ('mixed', 'c', -3.0), ('mixed', 'n2', nan);
INSERT INTO nan_ordering_probe VALUES ('mixed', 'd', 9.0), ('mixed', 'f', 1.0);
INSERT INTO nan_ordering_probe VALUES ('allnan', 'x', nan), ('allnan', 'y', nan);
`

// nanOrderingRealIDsDesc / nanOrderingRealIDsAsc are the non-NaN rows of
// the `mixed` group in each direction, written out rather than derived
// so a comparator that reversed the real values too would still fail.
var (
	nanOrderingRealIDsDesc = []string{"d", "a", "f", "c"}
	nanOrderingRealIDsAsc  = []string{"c", "f", "a", "d"}
	// The NaN rows, which must occupy the tail in BOTH directions. They
	// tie with each other, so only the SET is asserted, sorted.
	nanOrderingNaNIDs = []string{"n1", "n2"}
)

// nanOrderingSmallBlock forces the multi-block read path: with a block
// size of two rows the six-row `mixed` group is merged from several
// blocks in every query below, not sorted as one.
const nanOrderingSmallBlock = " SETTINGS max_block_size = 2"

// TestClickHouseIgnoresNaNInMaxMin pins the `max`/`min` half.
func TestClickHouseIgnoresNaNInMaxMin(t *testing.T) {
	fixture := newChDBFixture(t, nanOrderingSeed)

	for _, tc := range []struct {
		grp            string
		wantMax        float64
		wantMin        float64
		wantBothAreNaN bool
	}{
		// NaN is absent from the extremes: the answers are the real
		// rows', not `nan`.
		{grp: "mixed", wantMax: 9, wantMin: -3},
		// ...unless every value is NaN, which is the one case
		// Prometheus's own `|| math.IsNaN(group.floatValue)` leaves
		// with nothing better to fall back to.
		{grp: "allnan", wantBothAreNaN: true},
	} {
		tc := tc
		t.Run(tc.grp, func(t *testing.T) {
			var gotMax, gotMin float64
			row := fixture.db.QueryRow(
				"SELECT max(v), min(v) FROM nan_ordering_probe WHERE grp = ?", tc.grp,
			)
			if err := row.Scan(&gotMax, &gotMin); err != nil {
				t.Fatalf("scan max/min for %q: %v", tc.grp, err)
			}
			if tc.wantBothAreNaN {
				if !math.IsNaN(gotMax) || !math.IsNaN(gotMin) {
					t.Errorf("max/min over an all-NaN group = %v/%v, want nan/nan (the MAX and MIN arms of promql/engine.go's aggregation)", gotMax, gotMin)
				}
				return
			}
			if gotMax != tc.wantMax || gotMin != tc.wantMin {
				t.Errorf("max/min over %q = %v/%v, want %v/%v — ClickHouse must ignore NaN the way Prometheus's `|| math.IsNaN(group.floatValue)` does (the MAX and MIN arms of promql/engine.go's aggregation)",
					tc.grp, gotMax, gotMin, tc.wantMax, tc.wantMin)
			}
		})
	}
}

// TestClickHouseSortsNaNLastInBothDirections pins the ordering half, in
// both the plain `ORDER BY` shape internal/chsql's emitTopK emits for a
// literal k and the `row_number() OVER (… ORDER BY …)` window shape
// emitTopKComputed emits for a computed one.
//
// NaN LAST in BOTH directions is the ordering spelling of Prometheus's
// heap rule `math.IsNaN(group.heap[0].F) && !math.IsNaN(s.F)`
// (the `parser.TOPK` and `parser.BOTTOMK` arms of `promql/engine.go`'s
// `aggregationK`): a NaN sitting in the heap is displaced by any real
// value regardless of which end the heap keeps. NaN-FIRST under `DESC` — what a "NaN compares greatest"
// total order would give — would make `topk(2, v)` answer the two NaN
// series.
func TestClickHouseSortsNaNLastInBothDirections(t *testing.T) {
	fixture := newChDBFixture(t, nanOrderingSeed)

	const mixedWhere = "FROM nan_ordering_probe WHERE grp = 'mixed'"

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "plain order by desc",
			query: "SELECT id " + mixedWhere + " ORDER BY v DESC" + nanOrderingSmallBlock,
			want:  nanOrderingRealIDsDesc,
		},
		{
			name:  "plain order by asc",
			query: "SELECT id " + mixedWhere + " ORDER BY v ASC" + nanOrderingSmallBlock,
			want:  nanOrderingRealIDsAsc,
		},
		{
			// The emitTopKComputed shape: rank inside a window, then
			// read the ranks back in order.
			name: "window row_number desc",
			query: "SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY grp ORDER BY v DESC) AS rn " +
				mixedWhere + ") ORDER BY rn ASC" + nanOrderingSmallBlock,
			want: nanOrderingRealIDsDesc,
		},
		{
			name: "window row_number asc",
			query: "SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY grp ORDER BY v ASC) AS rn " +
				mixedWhere + ") ORDER BY rn ASC" + nanOrderingSmallBlock,
			want: nanOrderingRealIDsAsc,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := nanOrderingIDs(t, fixture.db, tc.query)
			if len(got) != len(tc.want)+len(nanOrderingNaNIDs) {
				t.Fatalf("query returned %d rows (%v), want %d", len(got), got, len(tc.want)+len(nanOrderingNaNIDs))
			}
			gotReal, gotNaN := got[:len(tc.want)], got[len(tc.want):]
			if strings.Join(gotReal, ",") != strings.Join(tc.want, ",") {
				t.Errorf("leading (non-NaN) order = %v, want %v", gotReal, tc.want)
			}
			// The two NaN rows tie, so only their membership in the
			// TAIL is asserted — that tail placement IS the property.
			sorted := append([]string(nil), gotNaN...)
			sort.Strings(sorted)
			if strings.Join(sorted, ",") != strings.Join(nanOrderingNaNIDs, ",") {
				t.Errorf("trailing rows = %v, want the NaN rows %v last (the topk and bottomk arms of promql/engine.go's aggregationK); full order was %v",
					gotNaN, nanOrderingNaNIDs, got)
			}
		})
	}
}

// nanOrderingIDs runs query and returns its `id` column in row order.
func nanOrderingIDs(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
