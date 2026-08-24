//go:build chdb

package chsql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// gridNativeBoundBoundsCount is the number of finite ExplicitBounds every
// seeded series carries, so each series contributes
// gridNativeBoundBoundsCount+1 rungs (the +Inf overflow) to Level 1's
// (series, le) GROUP BY the bound guards. bucketGridGroupCountBoundedSourceFrag's
// own probe reads only Level 0b's cheap rung population — no native
// aggregate, no anchor-width array — so unlike an arrayJoin-fan-out guard's
// row count this test's seed needs no presence coverage across the anchor
// grid at all: ONE row per series is enough regardless of how large
// numAnchors is, since the guard's own axis (groups x NumAnchors) is
// evaluated BEFORE the anchor-wide aggregate ever runs.
const gridNativeBoundBoundsCount = 50

// buildGridNativePlan constructs a bare chplan.RangeBucketGridNative over a
// plain Scan directly (bypassing PromQL lowering, matching this package's
// own emit-level test style elsewhere — see buildWindowSlidePlan's
// identical rationale) with the given anchor count, and returns the
// rendered (sql, args). Reuses rangeBucketWindowSlideBoundDDL
// (range_bucket_window_slide_bound_test.go): both nodes read the identical
// minimal classic-histogram table shape.
func buildGridNativePlan(t *testing.T, numAnchors int) (string, []any) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := time.Minute
	end := start.Add(time.Duration(numAnchors-1) * step)
	node := &chplan.RangeBucketGridNative{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{"SeriesID", "TimeUnix", "BucketCounts", "ExplicitBounds"},
		},
		Start:             start,
		End:               end,
		Step:              step,
		Range:             5 * time.Minute,
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "SeriesID"}},
		GroupByAliases:    []string{"SeriesID"},
		AnchorAlias:       "anchor_ts",
		TimestampCol:      "TimeUnix",
		BucketCountsCol:   "BucketCounts",
		ExplicitBoundsCol: "ExplicitBounds",
	}
	sqlStr, args, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return sqlStr, args
}

// seedGridNativeSeries inserts seriesCount series, TWO rows each one minute
// apart (bucketGridSeenFn's own per-series two-sample floor — a lone sample
// answers a NULL rate at every anchor, which Level 3's HAVING then drops
// every row for, defeating TestRangeBucketGridNativeBound_PassesWhenUnderBudget's
// own "at least one row back" assertion), every row carrying the identical
// gridNativeBoundBoundsCount-finite-bound layout. See this file's own
// gridNativeBoundBoundsCount doc for why two rows per series (not full
// anchor-grid coverage) is enough to exercise the guard regardless of
// numAnchors.
func seedGridNativeSeries(t *testing.T, exec func(string) error, seriesCount int) {
	t.Helper()
	bounds := make([]string, gridNativeBoundBoundsCount)
	for i := range bounds {
		bounds[i] = fmt.Sprintf("%d.0", i+1)
	}
	boundsLit := "[" + strings.Join(bounds, ",") + "]"
	counts := make([]string, gridNativeBoundBoundsCount+1)
	for i := range counts {
		counts[i] = "1"
	}
	countsLit := "[" + strings.Join(counts, ",") + "]"

	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_histogram (MetricName, SeriesID, TimeUnix, BucketCounts, ExplicitBounds) VALUES ")
	for i := 0; i < seriesCount; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "('http_server_request_duration', 'svc-%d', toDateTime64('2026-01-01 00:00:00', 9), %s, %s),"+
			"('http_server_request_duration', 'svc-%d', toDateTime64('2026-01-01 00:01:00', 9), %s, %s)",
			i, countsLit, boundsLit, i, countsLit, boundsLit)
	}
	if err := exec(b.String()); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// TestRangeBucketGridNativeBound_ThrowsWhenOversized proves
// maxRangeBucketGridNativeRows' throwIf guard actually fires — issue #2486
// found this node shipped with NO resource-bound guard at all and genuinely
// OOMs at real production scale; this is the check that the guard this file
// adds does not repeat that gap silently (a bound that exists in source but
// has never actually been triggered is exactly as untested as no bound at
// all).
//
// 60 series x 51 rungs/series x 6,600 anchors = 20,196,000 groups x
// anchors, comfortably past maxRangeBucketGridNativeRows (20,000,000 —
// recalibrated by issue #2522, see range_bucket_grid_native_bound.go's own
// doc for the real-ClickHouse measurement this threshold is grounded in).
// Only 120 real rows need seeding (two per series — see
// seedGridNativeSeries) — the anchor grid width is free (an
// emitter-generated timeSeriesRange, not seeded data), and (per
// gridNativeBoundBoundsCount's own doc) the guard's cheap probe needs no
// presence coverage across it either.
func TestRangeBucketGridNativeBound_ThrowsWhenOversized(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 60
	const numAnchors = 6_600 // 60 * 51 * 6,600 = 20,196,000 > maxRangeBucketGridNativeRows (20,000,000)
	seedGridNativeSeries(t, func(s string) error { _, err := db.Exec(s); return err }, seriesCount)

	sqlStr, args := buildGridNativePlan(t, numAnchors)
	_, err := db.Query(sqlStr, args...)
	if err == nil {
		t.Fatal("expected the resource bound's throwIf to fire for an oversized query, got no error")
	}
	if !strings.Contains(err.Error(), chsql.RangeBucketGridNativeBudgetMessage) {
		t.Errorf("query failed, but not with the expected resource-bound message %q: %v",
			chsql.RangeBucketGridNativeBudgetMessage, err)
	}
}

// TestRangeBucketGridNativeBound_PassesWhenUnderBudget is the negative
// control: a comfortably-under-budget query (60 series x 51 rungs x 100
// anchors = 306,000, well under budget) must NOT trip the guard, proving
// TestRangeBucketGridNativeBound_ThrowsWhenOversized's failure is really
// the size bound firing and not some unrelated query error every shape of
// this emitter's SQL would hit.
func TestRangeBucketGridNativeBound_PassesWhenUnderBudget(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 60
	const numAnchors = 100 // 60 * 51 * 100 = 306,000, well under maxRangeBucketGridNativeRows
	seedGridNativeSeries(t, func(s string) error { _, err := db.Exec(s); return err }, seriesCount)

	sqlStr, args := buildGridNativePlan(t, numAnchors)
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		t.Fatalf("under-budget query unexpectedly failed: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one row from the under-budget query")
	}
}

// TestRangeBucketGridNativeBound_PassesLowCardinalityWideAnchorShape is
// issue #2522's own regression pin: a LOW-series-cardinality, WIDE-anchor
// query — the shape a self-monitoring dashboard panel like
// `histogram_quantile(0.95, sum by (le, cerberus_ql) (rate(
// cerberus_queries_duration_seconds_bucket[5m])))` at a 24h/15s window
// (5,760 anchors) produces — must NOT trip the guard, even though
// `groups x anchors` here (60 series x 51 rungs x 5,760 anchors =
// 17,625,600) comfortably exceeds the OLD maxRangeBucketGridNativeRows
// (4,000,000) that wrongly rejected run 32688649627's real nightly
// dashboard query. See range_bucket_grid_native_bound.go's own doc for the
// real-ClickHouse measurement (8-42% of the 1 GiB cap across this exact
// shape family) grounding the new 20,000,000 threshold. Before that
// recalibration, this exact test would have failed with the old bound's
// throwIf firing.
func TestRangeBucketGridNativeBound_PassesLowCardinalityWideAnchorShape(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 60
	const numAnchors = 5_760 // 24h/15s, run 32688649627's own failing grid width
	seedGridNativeSeries(t, func(s string) error { _, err := db.Exec(s); return err }, seriesCount)

	sqlStr, args := buildGridNativePlan(t, numAnchors)
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		t.Fatalf("low-cardinality/wide-anchor query unexpectedly failed (issue #2522 regression): %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one row from the low-cardinality/wide-anchor query")
	}
}
