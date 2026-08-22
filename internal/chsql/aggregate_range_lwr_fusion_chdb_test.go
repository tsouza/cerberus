//go:build chdb

// chDB-backed proof that the RangeLWR fusion fast path
// (aggregate_range_lwr_fusion.go, issue #2442) produces IDENTICAL results
// to the unfused `Aggregate(subqueryFrag(RangeLWR))` path it replaces, for
// both fused shapes (sum() and count()).
//
// The seed is deliberately built so at least one series contributes
// MULTIPLE raw samples to the SAME anchor's staleness window (series "a"
// below): that is exactly the shape a naive count()-fusion (a plain
// count() over the fan-out instead of uniqExact(tuple(MetricName,
// Attributes))) would get wrong — it would count each of that series'
// fanned-in raw samples separately instead of the series once. The
// unfused-vs-fused comparison below would fail loudly on that seed if the
// distinct-count reasoning in emitAggregateRangeLWRFusedDistinctCount's
// doc comment were unsound.
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

// rangeLWRFusionSeedTable is the scan table both the fused and unfused
// plans below read from.
const rangeLWRFusionSeedTable = "range_lwr_fusion_metrics"

// rangeLWRFusionGrid mirrors issue #2442's own reproduction shape (a
// step/lookback grid an order of magnitude smaller so the seed stays
// hand-checkable): a 3-anchor grid one minute apart, 5-minute lookback —
// generous enough that every seeded sample is a LWR candidate for every
// anchor, so which sample argMax/uniqExact select is decided purely by
// the `ts <= anchor` membership bound, exactly like production.
var (
	rangeLWRFusionStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeLWRFusionEnd   = rangeLWRFusionStart.Add(2 * time.Minute)
)

const (
	rangeLWRFusionStep     = time.Minute
	rangeLWRFusionLookback = 5 * time.Minute
)

// TestAggregateRangeLWRFusion_MatchesUnfused_ChDB seeds three series across
// two `reason` groups — "a" and "b" share reason="Error" (with "a" landing
// THREE raw samples inside the same anchor windows), "c" carries
// reason="OK" alone — and asserts sum by (reason) / count by (reason) over
// a range query produce byte-identical row sets whether or not the
// RangeLWR fusion fires.
func TestAggregateRangeLWRFusion_MatchesUnfused_ChDB(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(chsqltest.MetricsSeedDDL(rangeLWRFusionSeedTable)); err != nil {
		t.Fatalf("create seed table: %v", err)
	}

	type sample struct {
		series string
		reason string
		offset time.Duration // relative to rangeLWRFusionStart
		value  float64
	}
	seed := []sample{
		// series "a" (reason=Error): three samples, all within every
		// anchor's 5m lookback window — the multi-fan-in-per-anchor case.
		{"a", "Error", -30 * time.Second, 1},
		{"a", "Error", 30 * time.Second, 2},
		{"a", "Error", 90 * time.Second, 3},
		// series "b" (reason=Error): one sample.
		{"b", "Error", 10 * time.Second, 10},
		// series "c" (reason=OK): one sample.
		{"c", "OK", 15 * time.Second, 100},
	}
	for i, s := range seed {
		ts := rangeLWRFusionStart.Add(s.offset)
		insert := fmt.Sprintf(
			"INSERT INTO %s (MetricName, Attributes, TimeUnix, Value) VALUES ('m', map('reason', '%s', 'series', '%s'), toDateTime64('%s', 9), %v)",
			rangeLWRFusionSeedTable, s.reason, s.series, ts.Format("2006-01-02 15:04:05.000000000"), s.value,
		)
		if _, err := db.Exec(insert); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	for _, fn := range []chplan.Fn{chplan.FnSum, chplan.FnCount} {
		t.Run(string(fn), func(t *testing.T) {
			fused := rangeLWRFusionAggregate(fn, true)
			unfused := rangeLWRFusionAggregate(fn, false)

			fusedSQL, fusedArgs, err := chsql.Emit(context.Background(), fused)
			if err != nil {
				t.Fatalf("emit fused: %v", err)
			}
			unfusedSQL, unfusedArgs, err := chsql.Emit(context.Background(), unfused)
			if err != nil {
				t.Fatalf("emit unfused: %v", err)
			}
			// The fusion is unconditional on Input's exact type — asserting
			// it actually fired here keeps this test from silently turning
			// into a no-op comparison against itself if the eligibility
			// check ever regresses to always declining.
			if fusedSQL == unfusedSQL {
				t.Fatalf("fused and unfused SQL are identical — the fusion did not fire:\n%s", fusedSQL)
			}

			gotFused := rangeLWRFusionRows(t, db, fusedSQL, fusedArgs)
			gotUnfused := rangeLWRFusionRows(t, db, unfusedSQL, unfusedArgs)
			if diff := rangeLWRFusionDiff(gotFused, gotUnfused); diff != "" {
				t.Fatalf("fused rows differ from unfused rows for %s:\n%s\nfused SQL: %s\nunfused SQL: %s", fn, diff, fusedSQL, unfusedSQL)
			}
		})
	}
}

// rangeLWRFusionAggregate builds `<fn> by (reason) (m)` over the fixed
// grid, directly at the chplan level. When direct is true, Aggregate.Input
// is the RangeLWR node itself (matchRangeLWRFusion fires); when false,
// Input is a byte-for-byte passthrough Project over the SAME RangeLWR
// (identical output columns, but a *chplan.Project type — the fusion
// declines because the match is Input's exact Go type, not its shape),
// forcing the ordinary opaque-subquery path this test compares against.
func rangeLWRFusionAggregate(fn chplan.Fn, direct bool) *chplan.Aggregate {
	lwr := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: rangeLWRFusionSeedTable},
		Start:         rangeLWRFusionStart,
		End:           rangeLWRFusionEnd,
		Step:          rangeLWRFusionStep,
		Lookback:      rangeLWRFusionLookback,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	var input chplan.Node = lwr
	if !direct {
		input = &chplan.Project{
			Input: lwr,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}, Alias: "MetricName"},
				{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
				{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
				{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			},
		}
	}
	return &chplan.Aggregate{
		Input: input,
		GroupBy: []chplan.Expr{
			&chplan.MapAccess{Map: &chplan.ColumnRef{Name: "Attributes"}, Key: &chplan.LitString{V: "reason"}},
			&chplan.ColumnRef{Name: "TimeUnix"},
		},
		GroupByAliases: []string{"gkey_0", "bucket_ts"},
		AggFuncs: []chplan.AggFunc{{
			Fn:    fn,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
			Alias: "Value",
		}},
		DropEmptyOnNoGroup: true,
	}
}

type rangeLWRFusionRow struct {
	reason string
	bucket string
	value  float64
}

func rangeLWRFusionRows(t *testing.T, db *sql.DB, query string, args []any) []rangeLWRFusionRow {
	t.Helper()
	q := "SELECT gkey_0, toString(bucket_ts), Value FROM (" + query + ") ORDER BY gkey_0, bucket_ts"
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, q)
	}
	defer rows.Close()
	var out []rangeLWRFusionRow
	for rows.Next() {
		var r rangeLWRFusionRow
		if err := rows.Scan(&r.reason, &r.bucket, &r.value); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("row iteration: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].reason != out[j].reason {
			return out[i].reason < out[j].reason
		}
		return out[i].bucket < out[j].bucket
	})
	return out
}

func rangeLWRFusionDiff(a, b []rangeLWRFusionRow) string {
	if len(a) != len(b) {
		return fmt.Sprintf("row count differs: %d vs %d\n  a=%v\n  b=%v", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("row %d differs: %+v vs %+v\n  a=%v\n  b=%v", i, a[i], b[i], a, b)
		}
	}
	return ""
}
