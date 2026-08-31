//go:build chdb

// chDB-backed proof that chplan.VectorJoin.ArgAndMaxFusion (chopt.
// FeatureArgAndMaxFusion, cerberus issue #2764) produces the SAME output
// as the unfused argMax(Value, TimeUnix) + max(TimeUnix) pair it replaces,
// for the non-derived, non-StepAligned (instant-mode) roleMany "latest
// sample" arm — the same tie-invariance argument
// range_lwr_argandmax_fusion_chdb_test.go exercises for RangeLWR, applied
// to a real INNER JOIN's per-side aggregation.
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
	vectorJoinArgAndMaxFusionLeftTable  = "vector_join_argandmax_fusion_left"
	vectorJoinArgAndMaxFusionRightTable = "vector_join_argandmax_fusion_right"
)

// TestVectorJoinArgAndMaxFusion_MatchesUnfused_ChDB joins two series by
// their full Attributes (default matching, CardOneToOne — both sides
// roleMany, the arm chplan.VectorJoin.ArgAndMaxFusion's own doc names as
// fusable). The left side carries a "normal" series with one unambiguous
// sample and a "tied" series with TWO samples sharing the exact same
// TimeUnix but different Values; the right side carries a single matching
// sample per series. It asserts the fused and unfused renderings report
// the IDENTICAL join-output TimeUnix for every series (tie-invariant,
// mirroring the RangeLWR chDB test) and, for the unambiguous series, the
// identical Value; for the tied series it asserts only that each
// rendering's Value falls within the genuinely-tied candidate set,
// without asserting the two renderings agree on WHICH tied row won
// (ClickHouse documents both argMax and argAndMax as non-deterministic
// among tied rows).
func TestVectorJoinArgAndMaxFusion_MatchesUnfused_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(vectorJoinArgAndMaxFusionLeftTable)); err != nil {
		t.Fatalf("create left seed table: %v", err)
	}
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(vectorJoinArgAndMaxFusionRightTable)); err != nil {
		t.Fatalf("create right seed table: %v", err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	type sample struct {
		series string
		offset time.Duration
		value  float64
	}
	seedTable := func(table string, rows []sample) {
		for i, s := range rows {
			ts := start.Add(s.offset)
			insert := fmt.Sprintf(
				"INSERT INTO %s (MetricName, Attributes, TimeUnix, Value) VALUES ('m', map('series', '%s'), toDateTime64('%s', 9), %v)",
				table, s.series, ts.Format("2006-01-02 15:04:05.000000000"), s.value,
			)
			if _, err := db.Exec(insert); err != nil {
				t.Fatalf("seed %s row %d: %v", table, i, err)
			}
		}
	}
	seedTable(vectorJoinArgAndMaxFusionLeftTable, []sample{
		{"normal", 30 * time.Second, 1},
		{"tied", 90 * time.Second, 10},
		{"tied", 90 * time.Second, 20}, // same TimeUnix as the row above
	})
	seedTable(vectorJoinArgAndMaxFusionRightTable, []sample{
		{"normal", 30 * time.Second, 100},
		{"tied", 90 * time.Second, 1000},
	})

	plan := func(fused bool) *chplan.VectorJoin {
		return &chplan.VectorJoin{
			Left:             &chplan.Scan{Table: vectorJoinArgAndMaxFusionLeftTable},
			Right:            &chplan.Scan{Table: vectorJoinArgAndMaxFusionRightTable},
			Op:               chplan.OpAdd,
			ArgAndMaxFusion:  fused,
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}

	fusedSQL, fusedArgs, err := chsql.Emit(context.Background(), plan(true))
	if err != nil {
		t.Fatalf("emit fused: %v", err)
	}
	unfusedSQL, unfusedArgs, err := chsql.Emit(context.Background(), plan(false))
	if err != nil {
		t.Fatalf("emit unfused: %v", err)
	}
	if fusedSQL == unfusedSQL {
		t.Fatalf("fused and unfused SQL are identical — the fusion did not fire:\n%s", fusedSQL)
	}

	fusedRows := vectorJoinArgAndMaxFusionRows(t, db, fusedSQL, fusedArgs)
	unfusedRows := vectorJoinArgAndMaxFusionRows(t, db, unfusedSQL, unfusedArgs)
	if len(fusedRows) != len(unfusedRows) {
		t.Fatalf("row count differs: fused=%d unfused=%d\nfused=%v\nunfused=%v", len(fusedRows), len(unfusedRows), fusedRows, unfusedRows)
	}

	tiedCandidates := map[float64]bool{1010: true, 1020: true} // L(10|20) + R(1000)
	for i := range fusedRows {
		fr, ur := fusedRows[i], unfusedRows[i]
		if fr.series != ur.series {
			t.Fatalf("row %d: series differs: fused=%q unfused=%q", i, fr.series, ur.series)
		}
		if fr.timestamp != ur.timestamp {
			t.Errorf("series %q: join TimeUnix differs: fused=%s unfused=%s", fr.series, fr.timestamp, ur.timestamp)
		}
		switch fr.series {
		case "normal":
			if fr.value != ur.value {
				t.Errorf("series %q: Value differs (unambiguous case): fused=%v unfused=%v", fr.series, fr.value, ur.value)
			}
			if fr.value != 101 {
				t.Errorf("series %q: Value = %v, want 101", fr.series, fr.value)
			}
		case "tied":
			if !tiedCandidates[fr.value] {
				t.Errorf("series %q: fused Value %v is not one of the tied candidates %v", fr.series, fr.value, tiedCandidates)
			}
			if !tiedCandidates[ur.value] {
				t.Errorf("series %q: unfused Value %v is not one of the tied candidates %v", fr.series, ur.value, tiedCandidates)
			}
		}
	}
}

type vectorJoinArgAndMaxFusionRow struct {
	series    string
	timestamp string
	value     float64
}

func vectorJoinArgAndMaxFusionRows(t *testing.T, db *sql.DB, query string, args []any) []vectorJoinArgAndMaxFusionRow {
	t.Helper()
	q := "SELECT Attributes['series'], toString(TimeUnix), Value FROM (" + query + ") ORDER BY 1"
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, q)
	}
	defer rows.Close()
	var out []vectorJoinArgAndMaxFusionRow
	for rows.Next() {
		var r vectorJoinArgAndMaxFusionRow
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
