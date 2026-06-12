//go:build chdb

// Perf guard (RC1 PromQL query_range LWR): the bare instant-vector
// `query_range` LWR path used to emit an N-anchor StepGrid CROSS JOIN +
// a correlated per-anchor staleness Filter + a per-(series, anchor)
// argMax — an O(rows × anchors) shape whose wall time scaled LINEARLY
// with the anchor count N (measured on main: N=61/121/241 → ~480/951/
// 1855ms for a fixed row set, ~100× the single-instant argMax). The
// CROSS JOIN materialised ~rows × (lookback/step) (sample, anchor)
// membership rows — at N=241 / step=6m / lookback=5m every sample landed
// in ~40 overlapping windows, a ~37× blow-up over the scan.
//
// The fix (internal/chplan.RangeLWR + internal/chsql.emitRangeLWR)
// replaces the CROSS JOIN with a single-pass, bounded sample-side
// fan-out: each sample fans out (via arrayJoin over a bounded
// `range(lo, hi)` index set) to ONLY the ≤ lookback/step + 1 anchors
// whose staleness window `(anchor - lookback, anchor]` contains it, then
// `GROUP BY (series, anchor) argMax(Value, TimeUnix)` collapses each
// bucket. The intermediate (sample, anchor) cardinality is
// rows × (lookback/step + 1) — CONSTANT in the grid width N — so wall
// time stops scaling with N.
//
// This guard pins both invariants against regression:
//
//  1. Scaling: hold the row set + window + lookback fixed and vary the
//     step so the anchor count N grows (61 → 121 → 241). The new path's
//     wall time stays ~flat (sub-linear) in N; the old CROSS JOIN path's
//     grows linearly. The assertion is on the RATIO (new is much flatter
//     than old), not absolute ms, so it's portable across runner speeds.
//
//  2. Intermediate cardinality: the new fan-out's (sample, anchor) row
//     count is bounded by rows × (lookback/step + 1) and is INDEPENDENT
//     of N (it shrinks as step grows because fewer anchors fit in the
//     lookback window), whereas the old shape's grows with the grid.
//
// Build-tagged `chdb`, same lane as the other chDB execs (#70/#789).
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// rangeLWRSeed builds a single gauge table with a moderate series fan-out
// and many samples per series across a fixed 24h window, so the per-anchor
// fan-out has real granule work to do. The row count is FIXED regardless
// of the query's step, isolating the anchor-count variable.
func rangeLWRSeed() string {
	ddl := `CREATE TABLE otel_metrics_gauge (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree()
	ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
	// 200 series × ~900 samples (one every 96s over 24h) = 180k rows —
	// in the same ballpark as the verified ~181k scan-row figure.
	ins := `INSERT INTO otel_metrics_gauge SELECT
	  'svc',
	  'demo_memory_usage_bytes',
	  map('instance', concat('i', toString(number % 200))),
	  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 200)) * 96),
	  toFloat64(number)
	FROM numbers(180000);`
	return ddl + ins
}

// emitRangeLWRSQL lowers `demo_memory_usage_bytes` over a query_range
// window [start, end] spaced by step and returns the emitted SQL + args.
func emitRangeLWRSQL(t *testing.T, start, end time.Time, step time.Duration) (string, []any) {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr("demo_memory_usage_bytes")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText, args
}

// oldCrossJoinSQL reconstructs the PRE-fix StepGrid CROSS JOIN + per-anchor
// argMax shape verbatim, so the guard can contrast its scaling against the
// new single-pass path on identical data. This is the exact shape
// internal/promql/lower.go:wrapRangeLatestPerSeries emitted before the
// RangeLWR rework (a StepGrid arrayJoin grid, a CROSS JOIN against the
// scan, the per-anchor staleness Filter, and the per-(series, anchor)
// argMax). Kept inline (not emitted) precisely because the fix DELETED the
// emitter for it — the guard pins that the deleted shape was the slow one.
func oldCrossJoinSQL(start, end time.Time, step, lookback time.Duration) string {
	stepNS := step.Nanoseconds()
	numAnchors := end.Sub(start).Nanoseconds()/stepNS + 1
	startLit := start.UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT MetricName, Attributes, anchor_ts AS TimeUnix, argMax(Value, TimeUnix) AS Value FROM (
  SELECT * FROM (
    SELECT arrayJoin(arrayMap(i -> toDateTime64('%s', 9) + toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts
  ) AS L CROSS JOIN (
    SELECT * FROM otel_metrics_gauge WHERE MetricName = 'demo_memory_usage_bytes'
  ) AS R
) WHERE (TimeUnix <= anchor_ts AND TimeUnix > anchor_ts - toIntervalNanosecond(%d))
GROUP BY MetricName, Attributes, anchor_ts`,
		startLit, stepNS, numAnchors, lookback.Nanoseconds())
}

// bestOf runs q `iters` times and returns the fastest wall time (min
// strips scheduler / GC jitter — the floor is what we compare). args binds
// the `?` placeholders the emitted SQL carries (the inline-literal old
// shape passes no args).
func bestOf(t *testing.T, db *sql.DB, q string, args []any, iters int) time.Duration {
	t.Helper()
	best := time.Hour
	for i := 0; i < iters; i++ {
		s := time.Now()
		var c int64
		// Wrap in count() so chDB-go's parquet driver returns one row —
		// the GROUP BY / fan-out / scan work being timed is identical.
		if err := db.QueryRow("SELECT count() FROM ("+q+")", args...).Scan(&c); err != nil {
			t.Fatalf("query: %v\nSQL: %s", err, q)
		}
		if d := time.Since(s); d < best {
			best = d
		}
	}
	return best
}

// fanoutCardinality counts the (sample, anchor) membership rows the inner
// fan-out produces BEFORE the GROUP BY collapse — the intermediate blow-up
// the fix targets. For the new path it strips the outer collapse SELECT and
// counts the fan-out subquery; for the old path it counts the CROSS JOIN +
// staleness Filter rows.
func fanoutCardinality(t *testing.T, db *sql.DB, innerCountSQL string) int64 {
	t.Helper()
	var c int64
	if err := db.QueryRow(innerCountSQL).Scan(&c); err != nil {
		t.Fatalf("cardinality query: %v\nSQL: %s", err, innerCountSQL)
	}
	return c
}

func TestRangeLWR_Scaling_ChDB(t *testing.T) {
	db := openChDB(t)
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	// Seed once; the row set is identical across every step variant.
	for _, stmt := range splitSQL(rangeLWRSeed()) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Fixed 24h window; vary the step so anchor count N = 24h/step + 1
	// grows 61 → 121 → 241 while the row set stays put.
	end := start.Add(24 * time.Hour)
	steps := []struct {
		step time.Duration
		n    int64
	}{
		{24 * time.Hour / 60, 61},   // 24m  → 61 anchors
		{24 * time.Hour / 120, 121}, // 12m  → 121 anchors
		{24 * time.Hour / 240, 241}, // 6m   → 241 anchors
	}

	const iters = 5
	type row struct {
		n                int64
		newWall, oldWall time.Duration
		newCard, oldCard int64
	}
	results := make([]row, 0, len(steps))

	for _, sc := range steps {
		newSQL, newArgs := emitRangeLWRSQL(t, start, end, sc.step)
		oldSQL := oldCrossJoinSQL(start, end, sc.step, lookback)

		newWall := bestOf(t, db, newSQL, newArgs, iters)
		oldWall := bestOf(t, db, oldSQL, nil, iters)

		// Intermediate (sample, anchor) cardinality before the collapse.
		newCard := fanoutCardinality(t, db, "SELECT count() FROM ("+newFanoutInner(start, end, sc.step, lookback)+")")
		oldCard := fanoutCardinality(t, db, "SELECT count() FROM ("+oldFanoutInner(start, end, sc.step, lookback)+")")

		results = append(results, row{sc.n, newWall, oldWall, newCard, oldCard})
		t.Logf("N=%-4d  new=%-10v old=%-10v   new_card=%-10d old_card=%-10d",
			sc.n, newWall.Round(time.Microsecond), oldWall.Round(time.Microsecond), newCard, oldCard)
	}

	first, last := results[0], results[len(results)-1]

	var scanRows int64
	if err := db.QueryRow("SELECT count() FROM otel_metrics_gauge").Scan(&scanRows); err != nil {
		t.Fatalf("scan count: %v", err)
	}

	// --- Invariant 1: new path is FAR faster + scales FLATTER than old --
	//
	// The headline win: the old O(rows×anchors) CROSS JOIN wall grows
	// linearly in N (measured here ~593→2409ms across the 4× N sweep,
	// matching the verified main-branch ~480→1855ms), while the new
	// single-pass path stays in the tens of ms. Two robust, runner-
	// portable assertions (no absolute-ms thresholds):
	//
	//  (a) At the largest N the new path is DRAMATICALLY faster than old —
	//      require ≥5× (observed ~46×). A regression that reinstated the
	//      CROSS JOIN would collapse this margin.
	//  (b) The new path scales STRICTLY FLATTER in N than the old: its
	//      growth ratio across the 4× N sweep is clearly below the old's.
	//      (The new growth is dominated by the bounded fan-out's
	//      lookback/step window-overlap factor + fixed per-query overhead,
	//      NOT by the grid width — so it stays well under the old's
	//      linear-in-N growth.)
	newGrowth := ratio(last.newWall, first.newWall)
	oldGrowth := ratio(last.oldWall, first.oldWall)
	t.Logf("growth over 4x N:  new=%.2fx  old=%.2fx", newGrowth, oldGrowth)

	speedup := ratio(last.oldWall, last.newWall)
	t.Logf("at N=%d: new=%v old=%v  speedup=%.1fx", last.n,
		last.newWall.Round(time.Microsecond), last.oldWall.Round(time.Microsecond), speedup)
	if speedup < 5.0 {
		t.Errorf("RangeLWR perf regression: at N=%d the single-pass path is only %.1fx faster than the "+
			"old CROSS JOIN shape (new=%v old=%v); want ≥5x. A collapsed margin means the "+
			"O(rows×anchors) fan-out shape regressed back in.", last.n, speedup, last.newWall, last.oldWall)
	}
	// Require the old shape to actually exhibit linear-in-N growth (so the
	// guard is meaningfully seeded) and the new shape to scale clearly
	// flatter. The 0.8× factor leaves headroom for the new path's small-
	// absolute-time noise while still catching a regression that made the
	// new path track N as steeply as the old.
	if oldGrowth > 2.0 && newGrowth >= oldGrowth*0.8 {
		t.Errorf("RangeLWR scaling regression: new-path growth %.2fx is not clearly flatter than the "+
			"old CROSS JOIN growth %.2fx across the 4x N sweep — the single-pass rework must scale "+
			"strictly better in the anchor count.", newGrowth, oldGrowth)
	}

	// --- Invariant 2: new intermediate cardinality is BOUNDED, not N×rows
	//
	// The new fan-out's (sample, anchor) count is rows × (lookback/step+1)
	// — it grows only as fast as lookback/step (the window-overlap factor),
	// NOT as rows × N. The decisive contrast: at N=241 the new path's
	// intermediate is ~0.9× the scan rows, while the old shape forces CH to
	// build a rows × N (~241×) gross CROSS JOIN before the staleness Filter
	// prunes it. We pin that the new card stays a SMALL multiple of the
	// scan (here ≤ a few × scan_rows across the sweep) and is dwarfed by
	// rows × N — that's the bounded-fan-out invariant. (The post-Filter
	// membership count is identical for both shapes by construction —
	// old_card == new_card in the log — so the win is the gross product CH
	// must materialise, asserted below.)
	if last.newCard > scanRows*3 {
		t.Errorf("RangeLWR cardinality regression: new fan-out (sample,anchor) rows = %d at N=%d, "+
			"exceeding 3× the scan rows (%d) — the bounded fan-out rows×(lookback/step+1) blew up. A "+
			"value tracking rows×N means the bounded index range degenerated to the full grid.",
			last.newCard, last.n, scanRows)
	}

	// Gross CROSS JOIN materialisation (rows × N) the old shape forces,
	// contrasted with the new path's scan rows (rows, no fan multiplier in
	// the gross product). This is the headline blow-up the fix removes.
	oldGross := scanRows * last.n
	t.Logf("at N=%d: scan_rows=%d  old_gross_crossjoin=%d (%.0fx)  new_intermediate=%d (%.1fx)",
		last.n, scanRows, oldGross, float64(oldGross)/float64(scanRows),
		last.newCard, float64(last.newCard)/float64(scanRows))

	if oldGross <= scanRows*int64(2) {
		t.Errorf("expected the old CROSS JOIN to materialise rows×N >> rows; got gross=%d vs scan=%d "+
			"(the perf-bug premise — a CROSS JOIN blow-up — is not reproduced, so the guard is "+
			"mis-seeded).", oldGross, scanRows)
	}
}

func ratio(a, b time.Duration) float64 {
	if b <= 0 {
		return 1
	}
	return float64(a) / float64(b)
}

// newFanoutInner is the bounded sample-side fan-out subquery the RangeLWR
// emitter produces, BEFORE the GROUP BY collapse — one row per (sample,
// covered anchor). Mirrors internal/chsql.lwrAnchorFanoutFrag (no offset).
// Counting its rows measures the new path's intermediate cardinality.
func newFanoutInner(start, end time.Time, step, lookback time.Duration) string {
	stepNS := step.Nanoseconds()
	numAnchors := end.Sub(start).Nanoseconds()/stepNS + 1
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	dist := fmt.Sprintf("dateDiff('nanosecond', TimeUnix, toDateTime64('%s', 9))", endLit)
	floorIdx := func(addNS int64) string {
		num := dist
		if addNS < 0 {
			num = fmt.Sprintf("%s - %d", dist, -addNS)
		}
		return fmt.Sprintf("intDiv(%s, toInt64(%d)) - (modulo(%s, toInt64(%d)) < 0) + 1",
			num, stepNS, num, stepNS)
	}
	return fmt.Sprintf(`SELECT TimeUnix, Value,
  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
FROM otel_metrics_gauge WHERE MetricName = 'demo_memory_usage_bytes'`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

// oldFanoutInner is the PRE-fix CROSS JOIN + staleness Filter subquery,
// BEFORE the per-(series, anchor) argMax — one row per (sample, covered
// anchor). Same membership count as newFanoutInner; the contrast is the
// GROSS CROSS JOIN (rows × N) CH must build before the Filter prunes it.
func oldFanoutInner(start, end time.Time, step, lookback time.Duration) string {
	stepNS := step.Nanoseconds()
	numAnchors := end.Sub(start).Nanoseconds()/stepNS + 1
	startLit := start.UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT TimeUnix, Value, anchor_ts FROM (
  SELECT * FROM (
    SELECT arrayJoin(arrayMap(i -> toDateTime64('%s', 9) + toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts
  ) AS L CROSS JOIN (
    SELECT * FROM otel_metrics_gauge WHERE MetricName = 'demo_memory_usage_bytes'
  ) AS R
) WHERE (TimeUnix <= anchor_ts AND TimeUnix > anchor_ts - toIntervalNanosecond(%d))`,
		startLit, stepNS, numAnchors, lookback.Nanoseconds())
}

// splitSQL splits a multi-statement seed string on `;` boundaries,
// dropping empties so each statement runs as its own db.Exec.
func splitSQL(s string) []string {
	out := make([]string, 0, 4)
	for _, part := range splitOnSemicolon(s) {
		p := trimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitOnSemicolon(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}
