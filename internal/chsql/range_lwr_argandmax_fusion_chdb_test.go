//go:build chdb

// chDB-backed proof that chplan.RangeLWR.ArgAndMaxFusion (chopt.
// FeatureArgAndMaxFusion, cerberus issue #2764) produces the SAME
// SampleTimestamp output as the unfused argMax(Value, TimeUnix) +
// max(TimeUnix) pair it replaces — for a normal (unambiguous latest
// sample) case AND a duplicate-timestamp case, exercising exactly the tie
// scenario the feature's own equivalence argument turns on (see
// chopt.FeatureArgAndMaxFusion's doc: the max(TimeUnix) half is tie-
// invariant regardless of which tied row argMax/argAndMax happen to pick
// for Value).
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

const rangeLWRArgAndMaxFusionSeedTable = "range_lwr_argandmax_fusion_metrics"

// TestRangeLWRArgAndMaxFusion_MatchesUnfused_ChDB seeds two series: one
// with a single unambiguous latest sample, and one with TWO samples
// sharing the exact same TimeUnix (a duplicate-timestamp tie) but
// different Values. It asserts:
//
//   - The fused (argAndMax) and unfused (argMax + max) SQL are genuinely
//     different renderings (the fusion actually fired).
//   - Both renderings report the IDENTICAL SampleTimestamp for every
//     series — including the tied one, where max(TimeUnix) has no
//     tie-break ambiguity at all regardless of which row's Value argMax /
//     argAndMax happen to select.
//   - The normal series' Value matches exactly between the two
//     renderings (there is no tie there, so the pick is unambiguous).
//   - The tied series' Value, in EACH rendering independently, is one of
//     the two genuinely-tied candidate values — proving the fused call
//     executes correctly under a duplicate sort key rather than erroring
//     or returning a value outside the tied set. It deliberately does
//     NOT assert the two renderings pick the SAME tied Value: ClickHouse
//     documents both argMax and argAndMax as non-deterministic among
//     tied rows, so asserting equality there would pin an implementation
//     accident, not a contract.
func TestRangeLWRArgAndMaxFusion_MatchesUnfused_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(rangeLWRArgAndMaxFusionSeedTable)); err != nil {
		t.Fatalf("create seed table: %v", err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	const (
		step     = time.Minute
		lookback = 5 * time.Minute
	)

	tiedTS := start.Add(90 * time.Second)
	type sample struct {
		series string
		offset time.Duration
		value  float64
	}
	seed := []sample{
		{"normal", 30 * time.Second, 42},
		{"tied", 90 * time.Second, 100},
		{"tied", 90 * time.Second, 200}, // same TimeUnix as the row above
	}
	for i, s := range seed {
		ts := start.Add(s.offset)
		if s.series == "tied" {
			ts = tiedTS
		}
		insert := fmt.Sprintf(
			"INSERT INTO %s (MetricName, Attributes, TimeUnix, Value) VALUES ('m', map('series', '%s'), toDateTime64('%s', 9), %v)",
			rangeLWRArgAndMaxFusionSeedTable, s.series, ts.Format("2006-01-02 15:04:05.000000000"), s.value,
		)
		if _, err := db.Exec(insert); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	plan := func(fused bool) *chplan.RangeLWR {
		return &chplan.RangeLWR{
			Input:           &chplan.Scan{Table: rangeLWRArgAndMaxFusionSeedTable},
			Start:           start,
			End:             end,
			Step:            step,
			Lookback:        lookback,
			SampleTimestamp: true,
			ArgAndMaxFusion: fused,
			MetricNameCol:   "MetricName",
			AttributesCol:   "Attributes",
			TimestampCol:    "TimeUnix",
			ValueCol:        "Value",
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

	fusedRows := rangeLWRArgAndMaxFusionRows(t, db, fusedSQL, fusedArgs)
	unfusedRows := rangeLWRArgAndMaxFusionRows(t, db, unfusedSQL, unfusedArgs)

	if len(fusedRows) != len(unfusedRows) {
		t.Fatalf("row count differs: fused=%d unfused=%d\nfused=%v\nunfused=%v", len(fusedRows), len(unfusedRows), fusedRows, unfusedRows)
	}
	fusedBySeries := rangeLWRArgAndMaxFusionBySeries(fusedRows)
	unfusedBySeries := rangeLWRArgAndMaxFusionBySeries(unfusedRows)

	tiedCandidates := map[float64]bool{100: true, 200: true}
	for series, fusedByAnchor := range fusedBySeries {
		unfusedByAnchor, ok := unfusedBySeries[series]
		if !ok {
			t.Fatalf("series %q present in fused output but not unfused", series)
		}
		for anchor, fr := range fusedByAnchor {
			ur, ok := unfusedByAnchor[anchor]
			if !ok {
				t.Fatalf("series %q anchor %q present in fused output but not unfused", series, anchor)
			}
			// SampleTimestamp is tie-invariant: identical between fused
			// and unfused regardless of series.
			if fr.sampleTS != ur.sampleTS {
				t.Errorf("series %q anchor %q: SampleTimestamp differs: fused=%s unfused=%s", series, anchor, fr.sampleTS, ur.sampleTS)
			}
			switch series {
			case "normal":
				if fr.value != ur.value {
					t.Errorf("series %q anchor %q: Value differs (unambiguous case): fused=%v unfused=%v", series, anchor, fr.value, ur.value)
				}
				if fr.value != 42 {
					t.Errorf("series %q anchor %q: Value = %v, want 42", series, anchor, fr.value)
				}
			case "tied":
				if !tiedCandidates[fr.value] {
					t.Errorf("series %q anchor %q: fused Value %v is not one of the tied candidates %v", series, anchor, fr.value, tiedCandidates)
				}
				if !tiedCandidates[ur.value] {
					t.Errorf("series %q anchor %q: unfused Value %v is not one of the tied candidates %v", series, anchor, ur.value, tiedCandidates)
				}
			}
		}
	}
}

type rangeLWRArgAndMaxFusionRow struct {
	series   string
	anchor   string
	value    float64
	sampleTS string
}

func rangeLWRArgAndMaxFusionRows(t *testing.T, db *sql.DB, query string, args []any) []rangeLWRArgAndMaxFusionRow {
	t.Helper()
	q := "SELECT Attributes['series'], toString(TimeUnix), Value, toString(lwr_sample_ts) FROM (" + query + ") ORDER BY 1, 2"
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, q)
	}
	defer rows.Close()
	var out []rangeLWRArgAndMaxFusionRow
	for rows.Next() {
		var r rangeLWRArgAndMaxFusionRow
		if err := rows.Scan(&r.series, &r.anchor, &r.value, &r.sampleTS); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].series != out[j].series {
			return out[i].series < out[j].series
		}
		return out[i].anchor < out[j].anchor
	})
	return out
}

func rangeLWRArgAndMaxFusionBySeries(rows []rangeLWRArgAndMaxFusionRow) map[string]map[string]rangeLWRArgAndMaxFusionRow {
	out := map[string]map[string]rangeLWRArgAndMaxFusionRow{}
	for _, r := range rows {
		if out[r.series] == nil {
			out[r.series] = map[string]rangeLWRArgAndMaxFusionRow{}
		}
		out[r.series][r.anchor] = r
	}
	return out
}
