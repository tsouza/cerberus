//go:build chdb

// chDB-backed proof for cerberus issue #2818's actual resolution: deleting
// the redundant max(TimeUnix) aggregate from VectorJoin's roleOne
// StepAligned arm (internal/chsql/vector_join.go's joinSideFrag) is safe,
// and argMax(Value, TimeUnix)'s own tie-break — not
// matchCheckGuardFrag's uniqueness HAVING, which fires AFTER aggregation
// and guards only Attributes uniqueness — governs which Value survives a
// genuine TimeUnix tie within one (match-key, anchor) group.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

const (
	vectorJoinStepAlignedRedundantMaxLeftTable  = "vector_join_stepaligned_redundant_max_left"
	vectorJoinStepAlignedRedundantMaxRightTable = "vector_join_stepaligned_redundant_max_right"
)

// TestVectorJoinRoleOneStepAligned_TiedTimestamp_ChDB seeds a
// group_left(instance) join (left=roleMany, right=roleOne) with
// StepAligned=true. The right ("one") side carries a "tied" series with
// TWO rows sharing the exact same TimeUnix but different Values — the
// scenario matchCheckGuardFrag's HAVING throwIf does NOT resolve, since it
// guards only Attributes uniqueness and fires after aggregation completes,
// not before. It asserts:
//
//   - The query executes and returns exactly one row per series (the
//     group-key read did not break the per-side aggregation or the JOIN).
//   - The joined TimeUnix equals the shared tied timestamp exactly — the
//     now-direct group-key read reports the same value the deleted
//     max(TimeUnix) aggregate would have (trivially, since every row in
//     the group already shared it), pinning that the rewrite changed
//     nothing observable for the timestamp axis.
//   - The joined Value falls within the genuinely-tied candidate set,
//     proving argMax(Value, TimeUnix) still executes correctly and picks
//     an actual tied row rather than erroring or returning a value outside
//     the tied set.
func TestVectorJoinRoleOneStepAligned_TiedTimestamp_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(vectorJoinStepAlignedRedundantMaxLeftTable)); err != nil {
		t.Fatalf("create left seed table: %v", err)
	}
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(vectorJoinStepAlignedRedundantMaxRightTable)); err != nil {
		t.Fatalf("create right seed table: %v", err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tiedTS := start.Add(90 * time.Second)
	normalTS := start.Add(30 * time.Second)

	type sample struct {
		series   string
		instance string
		ts       time.Time
		value    float64
	}
	seedTable := func(table string, rows []sample) {
		for i, s := range rows {
			insert := fmt.Sprintf(
				"INSERT INTO %s (MetricName, Attributes, TimeUnix, Value) VALUES ('m', map('series', '%s', 'instance', '%s'), toDateTime64('%s', 9), %v)",
				table, s.series, s.instance, s.ts.Format("2006-01-02 15:04:05.000000000"), s.value,
			)
			if _, err := db.Exec(insert); err != nil {
				t.Fatalf("seed %s row %d: %v", table, i, err)
			}
		}
	}
	// Left ("many") side: one row per series, matching on the bare series
	// label so it survives group_left's Attributes overlay unchanged.
	seedTable(vectorJoinStepAlignedRedundantMaxLeftTable, []sample{
		{"normal", "left-inst", normalTS, 1},
		{"tied", "left-inst", tiedTS, 1},
	})
	// Right ("one") side: the "tied" series carries two rows at the exact
	// same TimeUnix — same Attributes (same series+instance), different
	// Value, a genuine argMax tie the StepAligned GROUP BY (match-key,
	// TimeUnix) does not itself disambiguate.
	seedTable(vectorJoinStepAlignedRedundantMaxRightTable, []sample{
		{"normal", "right-inst", normalTS, 100},
		{"tied", "right-inst", tiedTS, 1000},
		{"tied", "right-inst", tiedTS, 2000}, // same TimeUnix as the row above
	})

	plan := &chplan.VectorJoin{
		Left:  &chplan.Scan{Table: vectorJoinStepAlignedRedundantMaxLeftTable},
		Right: &chplan.Scan{Table: vectorJoinStepAlignedRedundantMaxRightTable},
		Op:    chplan.OpAdd,
		// on(series): the two sides deliberately carry different
		// `instance` values (left-inst / right-inst) so the match key
		// must be reduced to `series` alone — matching group_left's
		// typical real-world shape (matching on a shared identity label,
		// not the full label set).
		Match:            chplan.VectorMatch{On: true, Labels: []string{"series"}},
		Card:             chplan.CardManyToOne, // left=roleMany, right=roleOne
		Include:          []string{"instance"},
		StepAligned:      true,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}

	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	rows := vectorJoinStepAlignedRedundantMaxRows(t, db, sql, args)
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2 (one per series); rows=%v\nSQL: %s", len(rows), rows, sql)
	}

	tiedCandidates := map[float64]bool{1001: true, 2001: true} // L(1) + R(1000|2000)
	for _, r := range rows {
		switch r.series {
		case "normal":
			if !r.timestamp.Equal(normalTS) {
				t.Errorf("series %q: TimeUnix = %s, want %s", r.series, r.timestamp, normalTS)
			}
			if r.value != 101 {
				t.Errorf("series %q: Value = %v, want 101", r.series, r.value)
			}
		case "tied":
			if !r.timestamp.Equal(tiedTS) {
				t.Errorf("series %q: TimeUnix = %s, want %s (the shared tied timestamp — the deleted max(TimeUnix) restated nothing else)", r.series, r.timestamp, tiedTS)
			}
			if !tiedCandidates[r.value] {
				t.Errorf("series %q: Value %v is not one of argMax's tied candidates %v", r.series, r.value, tiedCandidates)
			}
		default:
			t.Errorf("unexpected series %q", r.series)
		}
	}
}

type vectorJoinStepAlignedRedundantMaxRow struct {
	series    string
	timestamp time.Time
	value     float64
}

func vectorJoinStepAlignedRedundantMaxRows(t *testing.T, db *sql.DB, query string, args []any) []vectorJoinStepAlignedRedundantMaxRow {
	t.Helper()
	q := "SELECT Attributes['series'], TimeUnix, Value FROM (" + query + ") ORDER BY 1"
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, q)
	}
	defer rows.Close()
	var out []vectorJoinStepAlignedRedundantMaxRow
	for rows.Next() {
		var r vectorJoinStepAlignedRedundantMaxRow
		if err := rows.Scan(&r.series, &r.timestamp, &r.value); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].series < out[j].series })
	return out
}
