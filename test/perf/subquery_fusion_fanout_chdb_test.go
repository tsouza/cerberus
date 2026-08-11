//go:build chdb

// Memory-shape proof for the fused PromQL subquery emitter (#1505).
//
// A `/api/v1/query_range` over a subquery widens the INNER sample grid to the
// whole request window plus one subquery range, so the materialized path has to
// build `series × innerAnchors` rows of inner matrix before the outer reducer
// ever runs — an intermediate orders of magnitude larger than the answer. The
// fused emitter never builds it: it groups each series' samples into ONE row and
// evaluates the whole inner grid inside that row's arrays.
//
// The claim under test is a SCALING one, not a constant: the materialized inner
// matrix grows with the subquery resolution while the fused peak does not. This
// measures both at two subquery steps and pins the shape:
//
//	materialized inner matrix rows  ∝ series × innerAnchors   (doubles when the
//	                                                           sub step halves)
//	fused peak rows                 =  series                 (invariant)
//	answer rows                     =  series × outerAnchors  (invariant)
//
// A constant-factor assertion would be brittle across ClickHouse versions; the
// ratio between the two densities is a property of the emitted shape alone.
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers the "chdb" sql driver

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

const (
	fanoutTable = "otel_metrics_sum"
	// 64 series × 4h at 15s — big enough that series × innerAnchors is a real
	// intermediate, small enough to seed in-process.
	fanoutSeries       = 64
	fanoutScrape       = 15 * time.Second
	fanoutSeedDuration = 4 * time.Hour
	// The request grid: 2h at 1m, over a `[10m:<subStep>]` subquery of a
	// `rate(m[2m])`.
	fanoutRequestSpan = 2 * time.Hour
	fanoutRequestStep = time.Minute
	fanoutSubRange    = 10 * time.Minute
	fanoutInnerRange  = 2 * time.Minute
)

// fanoutSeedEnd anchors the seeded window; the request window ends here.
func fanoutSeedEnd() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }

func openFanoutDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ddl := "CREATE TABLE " + fanoutTable + ` (
  Attributes Map(String, String),
  TimeUnix DateTime64(9),
  Value Float64
) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`
	if _, err := db.Exec("DROP TABLE IF EXISTS " + fanoutTable); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create: %v", err)
	}
	// One INSERT … SELECT FROM numbers() materialises the whole grid: series
	// index = number / samplesPerSeries, sample index = number % samplesPerSeries.
	samplesPerSeries := int(fanoutSeedDuration/fanoutScrape) + 1
	seedStart := fanoutSeedEnd().Add(-fanoutSeedDuration).Format("2006-01-02 15:04:05")
	insert := fmt.Sprintf(`INSERT INTO %s
SELECT
  map('instance', concat('host-', leftPad(toString(intDiv(number, %d) %% %d), 3, '0'))) AS Attributes,
  toDateTime64('%s', 9) + INTERVAL (number %% %d) * %d SECOND AS TimeUnix,
  toFloat64(number %% %d) * 7 AS Value
FROM numbers(%d)`,
		fanoutTable,
		samplesPerSeries, fanoutSeries,
		seedStart, samplesPerSeries, int(fanoutScrape.Seconds()),
		samplesPerSeries,
		fanoutSeries*samplesPerSeries)
	if _, err := db.Exec(insert); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// fanoutPlan builds the range-mode subquery plan for a given subquery step. The
// inner grid quantities mirror promql.widenSubquerySpine: the inner window is
// widened by one subquery range and epoch step-aligned, the outer keeps the
// request's own grid.
func fanoutPlan(subStep time.Duration) *chplan.RangeWindow {
	end := fanoutSeedEnd()
	start := end.Add(-fanoutRequestSpan)
	innerStart := start.Add(-fanoutSubRange)
	inner := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: fanoutTable},
		Func:            "rate",
		Range:           fanoutInnerRange,
		Step:            subStep,
		OuterRange:      end.Sub(innerStart),
		StepAlign:       true,
		Start:           innerStart,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return &chplan.RangeWindow{
		Input:           inner,
		Func:            "max_over_time",
		Range:           fanoutSubRange,
		Step:            fanoutRequestStep,
		OuterRange:      end.Sub(start),
		Start:           start,
		End:             end,
		TimestampColumn: chplan.RangeWindowAnchorColumn,
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

func fanoutCount(t *testing.T, db *sql.DB, inner string) int64 {
	t.Helper()
	var n int64
	q := "SELECT count() FROM (" + stripTrailingSemi(inner) + ")"
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("count: %v\nSQL: %s", err, q)
	}
	return n
}

func fanoutEmit(t *testing.T, n chplan.Node) string {
	t.Helper()
	sqlStr, args, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("plan emitted %d query args; the fan-out measurement executes raw SQL", len(args))
	}
	return sqlStr
}

// TestSubqueryFusionInnerMatrixFanout_ChDB measures what #1505 removed.
func TestSubqueryFusionInnerMatrixFanout_ChDB(t *testing.T) {
	db := openFanoutDB(t)

	var seriesRows int64
	if err := db.QueryRow(
		"SELECT count() FROM (SELECT `Attributes` FROM " + fanoutTable + " GROUP BY `Attributes`)",
	).Scan(&seriesRows); err != nil {
		t.Fatalf("series count: %v", err)
	}
	if seriesRows != fanoutSeries {
		t.Fatalf("seed produced %d series, want %d", seriesRows, fanoutSeries)
	}

	type measurement struct {
		subStep       time.Duration
		innerMatrix   int64
		answer        int64
		innerAnchors  int
		fusedGroupBys int
	}
	measure := func(subStep time.Duration) measurement {
		outer := fanoutPlan(subStep)
		inner, ok := outer.Input.(*chplan.RangeWindow)
		if !ok {
			t.Fatalf("plan input is not a RangeWindow")
		}
		fusedSQL := fanoutEmit(t, outer)
		// The fused shape's peak row count is one per series BY CONSTRUCTION:
		// its only regroup is the per-series samples layer, everything above it
		// is array arithmetic inside that one row plus the final arrayJoin back
		// out to the answer. Pin that structure — the row measurements below
		// mean nothing if a second regroup ever reappears.
		groupBys := strings.Count(fusedSQL, "GROUP BY")
		if groupBys != 1 {
			t.Errorf("fused subquery SQL has %d GROUP BY clauses, want exactly 1 "+
				"(the per-series samples layer)\nSQL: %s", groupBys, fusedSQL)
		}
		return measurement{
			subStep:       subStep,
			innerMatrix:   fanoutCount(t, db, fanoutEmit(t, inner)),
			answer:        fanoutCount(t, db, fusedSQL),
			innerAnchors:  int(inner.OuterRange / subStep),
			fusedGroupBys: groupBys,
		}
	}

	coarse := measure(fanoutSubRange / 10) // 1m sub step
	fine := measure(fanoutSubRange / 20)   // 30s sub step

	for _, m := range []measurement{coarse, fine} {
		t.Logf("sub step %v: materialized inner matrix = %d rows (%d series × ~%d inner anchors), "+
			"fused peak = %d rows (1 per series), answer = %d rows",
			m.subStep, m.innerMatrix, seriesRows, m.innerAnchors, seriesRows, m.answer)
		if m.innerMatrix <= seriesRows {
			t.Fatalf("sub step %v: materialized inner matrix (%d rows) did not exceed the "+
				"series count (%d) — the seed does not exercise the fan-out",
				m.subStep, m.innerMatrix, seriesRows)
		}
	}

	// Scaling: halving the subquery step doubles the materialized intermediate
	// while the fused peak (series) and the answer (series × outer anchors) are
	// both untouched. The bound is deliberately loose on the ratio (>1.5x rather
	// than ==2x) — anchors whose window holds fewer than two samples drop on
	// both densities — but a fused peak that moved AT ALL is a hard failure.
	const minFanoutGrowth = 1.5
	growth := float64(fine.innerMatrix) / float64(coarse.innerMatrix)
	if growth < minFanoutGrowth {
		t.Errorf("halving the subquery step grew the materialized inner matrix only %.2fx "+
			"(%d -> %d rows); it must scale with the inner grid",
			growth, coarse.innerMatrix, fine.innerMatrix)
	}
	if coarse.answer != fine.answer {
		t.Errorf("answer size moved with the subquery step: %d vs %d rows — the outer grid "+
			"is the request's own and must not depend on the subquery resolution",
			coarse.answer, fine.answer)
	}

	t.Logf("fusion removes a %.0fx (sub step %v) / %.0fx (sub step %v) intermediate: "+
		"materialized builds series × inner anchors, fused builds series",
		float64(coarse.innerMatrix)/float64(seriesRows), coarse.subStep,
		float64(fine.innerMatrix)/float64(seriesRows), fine.subStep)
}
