//go:build chdb

// Perf guard (RC1 PromQL histogram_quantile query_range): the classic /
// native histogram_quantile range path used to emit, for the
// per-(series, anchor) bucket aggregate, an N-anchor StepGrid CROSS JOIN
// + a correlated per-anchor staleness Filter + a per-(series, anchor)
// argMax(BucketCounts, TimeUnix) / argMax(ExplicitBounds, TimeUnix) — the
// same O(rows × anchors) compute fan-out the bare-selector LWR path
// carried before #804, just with array-valued aggregates. Measured on the
// old shape: gross CROSS JOIN = scan_rows × N, wall time ~linear in N.
//
// The fix (internal/chplan.RangeBucketFanout + internal/chsql.
// emitRangeBucketFanout) ports the histogram bucket aggregate to the same
// single-pass, bounded sample-side fan-out RangeLWR introduced: each
// histogram sample fans out (via arrayJoin over a bounded `range(lo, hi)`
// index set) to ONLY the ≤ lookback/step + 1 anchors whose staleness
// window `(anchor - lookback, anchor]` contains it, then `GROUP BY
// (series, anchor)` collapses each bucket with the variant aggregates
// (argMax / sumForEach / merge). The intermediate (sample, anchor)
// cardinality is rows × (lookback/step + 1) — CONSTANT in the grid width
// N — so wall time stops scaling with N.
//
// This guard pins both invariants against regression, mirroring
// range_lwr_scaling_chdb_test.go but over the classic-histogram bucket
// aggregate stage (the array-valued argMax collapse that the
// RangeBucketFanout node owns; the HistogramQuantile interpolation
// wrapped on top is a constant per (series, anchor) and is excluded so
// the contrast isolates the fan-out):
//
//  1. Scaling: hold the row set + window + lookback fixed and vary the
//     step so the anchor count N grows (61 → 121 → 241). The new path's
//     wall time stays ~flat (sub-linear) in N; the old CROSS JOIN path's
//     grows linearly. Asserted on the RATIO, not absolute ms.
//
//  2. Intermediate cardinality: the new fan-out's (sample, anchor) row
//     count is bounded by rows × (lookback/step + 1), INDEPENDENT of N,
//     whereas the old shape forces a rows × N gross CROSS JOIN.
//
// Build-tagged `chdb`, same lane as the other chDB execs (#70/#789).
package perf

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogramRangeSeed builds a single classic-histogram table with a
// moderate series fan-out and many samples per series across a fixed 24h
// window, so the per-anchor bucket fan-out has real granule work. The row
// count is FIXED regardless of the query's step, isolating the
// anchor-count variable. Each row carries a 3-bucket classic histogram so
// the argMax(BucketCounts) / argMax(ExplicitBounds) array aggregates the
// RangeBucketFanout collapse runs are exercised, not just a scalar.
func histogramRangeSeed() string {
	ddl := `CREATE TABLE otel_metrics_histogram (
	  MetricName String, Attributes Map(String,String),
	  TimeUnix DateTime64(9),
	  BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
	) ENGINE = MergeTree()
	ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
	// 200 series × ~900 samples (one every 96s over 24h) = 180k rows —
	// same shape/scale as the RangeLWR guard's gauge seed.
	ins := `INSERT INTO otel_metrics_histogram SELECT
	  'http_request_duration',
	  map('instance', concat('i', toString(number % 200))),
	  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 200)) * 96),
	  [number % 5 + 1, number % 7 + 2, number % 3 + 3],
	  [0.1, 0.5, 1.0]
	FROM numbers(180000);`
	return ddl + ins
}

// newHistogramFanoutInner is the bounded sample-side fan-out + GROUP BY
// collapse the RangeBucketFanout emitter produces for the classic bare
// path — the (series, anchor) argMax(BucketCounts/ExplicitBounds, TimeUnix)
// over only the in-window anchors. Mirrors emitRangeBucketFanout (no
// offset). This is the exact inner aggregate stage the histogram range
// lowering now emits; timing it isolates the fan-out cost from the
// (constant) quantile interpolation wrapped on top.
func newHistogramFanoutInner(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
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
	return fmt.Sprintf(`SELECT Attributes, anchor_ts,
	  argMax(BucketCounts, TimeUnix) AS BucketCounts,
	  argMax(ExplicitBounds, TimeUnix) AS ExplicitBounds
	FROM (
	  SELECT *, arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	  FROM otel_metrics_histogram WHERE MetricName = 'http_request_duration'
	)
	GROUP BY Attributes, anchor_ts`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

// oldHistogramCrossJoinInner reconstructs the PRE-fix StepGrid CROSS JOIN
// + per-anchor staleness Filter + per-(series, anchor) array argMax
// verbatim — the exact inner aggregate stage the histogram range lowering
// emitted before the RangeBucketFanout rework. Kept inline (not emitted)
// because the fix DELETED this shape from the lowering; the guard pins
// that the deleted shape was the slow one.
func oldHistogramCrossJoinInner(start time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	startLit := start.UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT Attributes, anchor_ts,
	  argMax(BucketCounts, TimeUnix) AS BucketCounts,
	  argMax(ExplicitBounds, TimeUnix) AS ExplicitBounds
	FROM (
	  SELECT * FROM (
	    SELECT arrayJoin(arrayMap(i -> toDateTime64('%s', 9) + toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts
	  ) AS L CROSS JOIN (
	    SELECT * FROM otel_metrics_histogram WHERE MetricName = 'http_request_duration'
	  ) AS R
	) WHERE (TimeUnix <= anchor_ts AND TimeUnix > anchor_ts - toIntervalNanosecond(%d))
	GROUP BY Attributes, anchor_ts`,
		startLit, stepNS, numAnchors, lookback.Nanoseconds())
}

// newHistogramMembership / oldHistogramMembership strip the GROUP BY
// collapse to count the raw (sample, anchor) membership rows — the
// intermediate cardinality the fix bounds.
func newHistogramMembership(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
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
	return fmt.Sprintf(`SELECT TimeUnix,
	  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	FROM otel_metrics_histogram WHERE MetricName = 'http_request_duration'`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

func oldHistogramMembership(start time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	startLit := start.UTC().Format("2006-01-02 15:04:05.000000000")
	return fmt.Sprintf(`SELECT TimeUnix, anchor_ts FROM (
	  SELECT * FROM (
	    SELECT arrayJoin(arrayMap(i -> toDateTime64('%s', 9) + toIntervalNanosecond(i * %d), range(0, %d))) AS anchor_ts
	  ) AS L CROSS JOIN (
	    SELECT * FROM otel_metrics_histogram WHERE MetricName = 'http_request_duration'
	  ) AS R
	) WHERE (TimeUnix <= anchor_ts AND TimeUnix > anchor_ts - toIntervalNanosecond(%d))`,
		startLit, stepNS, numAnchors, lookback.Nanoseconds())
}

// assertHistogramRangeEmitsSinglePass is a belt-and-braces check that the
// production lowering actually routes the classic bare histogram_quantile
// range query through RangeBucketFanout (no CrossJoin / StepGrid in the
// emitted SQL), so the guard is timing the shape the lowering really
// emits.
func assertHistogramRangeEmitsSinglePass(t *testing.T, start, end time.Time, step time.Duration) {
	t.Helper()
	sqlText := emitHistogramRangeSQL(t, start, end, step)
	upper := strings.ToUpper(sqlText)
	for _, banned := range []string{"CROSS JOIN", "STEPGRID"} {
		if strings.Contains(upper, banned) {
			t.Fatalf("histogram_quantile range SQL still contains %q — the single-pass "+
				"RangeBucketFanout rework regressed:\n%s", banned, sqlText)
		}
	}
	if !strings.Contains(sqlText, "arrayJoin(arrayMap") {
		t.Fatalf("histogram_quantile range SQL missing the bounded arrayJoin fan-out:\n%s", sqlText)
	}
}

func TestHistogramRange_Scaling_ChDB(t *testing.T) {
	db := openChDB(t)
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS default"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	for _, stmt := range splitSQL(histogramRangeSeed()) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	// The production lowering must emit the single-pass shape (no CROSS
	// JOIN) before we trust the inline contrast below.
	assertHistogramRangeEmitsSinglePass(t, start, end, 24*time.Hour/240)

	steps := []struct {
		step time.Duration
		n    int64
	}{
		{24 * time.Hour / 60, 61},   // 24m → 61 anchors
		{24 * time.Hour / 120, 121}, // 12m → 121 anchors
		{24 * time.Hour / 240, 241}, // 6m  → 241 anchors
	}

	const iters = 5
	type row struct {
		n                int64
		newWall, oldWall time.Duration
		newCard, oldCard int64
	}
	results := make([]row, 0, len(steps))

	for _, sc := range steps {
		newSQL := newHistogramFanoutInner(end, sc.step, lookback, sc.n)
		oldSQL := oldHistogramCrossJoinInner(start, sc.step, lookback, sc.n)

		newWall := bestOf(t, db, newSQL, nil, iters)
		oldWall := bestOf(t, db, oldSQL, nil, iters)

		newCard := fanoutCardinality(t, db, "SELECT count() FROM ("+newHistogramMembership(end, sc.step, lookback, sc.n)+")")
		oldCard := fanoutCardinality(t, db, "SELECT count() FROM ("+oldHistogramMembership(start, sc.step, lookback, sc.n)+")")

		results = append(results, row{sc.n, newWall, oldWall, newCard, oldCard})
		t.Logf("N=%-4d  new=%-10v old=%-10v   new_card=%-10d old_card=%-10d",
			sc.n, newWall.Round(time.Microsecond), oldWall.Round(time.Microsecond), newCard, oldCard)
	}

	first, last := results[0], results[len(results)-1]

	var scanRows int64
	if err := db.QueryRow("SELECT count() FROM otel_metrics_histogram").Scan(&scanRows); err != nil {
		t.Fatalf("scan count: %v", err)
	}

	// --- Invariant 1: new path is FAR faster + scales FLATTER than old ---
	newGrowth := ratio(last.newWall, first.newWall)
	oldGrowth := ratio(last.oldWall, first.oldWall)
	t.Logf("growth over 4x N:  new=%.2fx  old=%.2fx", newGrowth, oldGrowth)

	speedup := ratio(last.oldWall, last.newWall)
	t.Logf("at N=%d: new=%v old=%v  speedup=%.1fx", last.n,
		last.newWall.Round(time.Microsecond), last.oldWall.Round(time.Microsecond), speedup)
	if speedup < 5.0 {
		t.Errorf("histogram range perf regression: at N=%d the single-pass RangeBucketFanout path is "+
			"only %.1fx faster than the old CROSS JOIN bucket-aggregate shape (new=%v old=%v); want ≥5x. "+
			"A collapsed margin means the O(rows×anchors) fan-out regressed back in.",
			last.n, speedup, last.newWall, last.oldWall)
	}
	if oldGrowth > 2.0 && newGrowth >= oldGrowth*0.8 {
		t.Errorf("histogram range scaling regression: new-path growth %.2fx is not clearly flatter than "+
			"the old CROSS JOIN growth %.2fx across the 4x N sweep — the single-pass rework must scale "+
			"strictly better in the anchor count.", newGrowth, oldGrowth)
	}

	// --- Invariant 2: new intermediate cardinality is BOUNDED, not N×rows ---
	if last.newCard > scanRows*3 {
		t.Errorf("histogram range cardinality regression: new fan-out (sample,anchor) rows = %d at N=%d, "+
			"exceeding 3× the scan rows (%d) — the bounded fan-out rows×(lookback/step+1) blew up.",
			last.newCard, last.n, scanRows)
	}

	oldGross := scanRows * last.n
	t.Logf("at N=%d: scan_rows=%d  old_gross_crossjoin=%d (%.0fx)  new_intermediate=%d (%.1fx)",
		last.n, scanRows, oldGross, float64(oldGross)/float64(scanRows),
		last.newCard, float64(last.newCard)/float64(scanRows))

	if oldGross <= scanRows*int64(2) {
		t.Errorf("expected the old CROSS JOIN to materialise rows×N >> rows; got gross=%d vs scan=%d "+
			"(the perf-bug premise is not reproduced, so the guard is mis-seeded).", oldGross, scanRows)
	}
}

// emitHistogramRangeSQL lowers `histogram_quantile(0.95,
// http_request_duration_bucket)` over a query_range window and returns the
// emitted SQL — the production shape the guard asserts is single-pass.
func emitHistogramRangeSQL(t *testing.T, start, end time.Time, step time.Duration) string {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr("histogram_quantile(0.95, http_request_duration_bucket)")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	sqlText, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText
}
