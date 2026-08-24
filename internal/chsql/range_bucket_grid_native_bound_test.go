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

// rangeBucketGridNativeBoundDDL is the minimal classic-histogram table shape
// RangeBucketGridNative reads. No Map-typed Attributes / ResourceAttributes /
// ServiceName columns: the plan node is built directly (bypassing PromQL
// lowering) with a bare ColumnRef group key over a plain String series id, so
// none of the schema-driven canonicalisation those columns feed is exercised
// here — this test is about the resource bound, not the lowering. (A
// Map(String,String) group key was tried first and dropped: the chdb-go
// driver's row scan cannot decode a Map column when iterating with
// rows.Next()/rows.Err() alone, unrelated to this bound.)
const rangeBucketGridNativeBoundDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    SeriesID String,
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, SeriesID, TimeUnix);
`

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
// own emit-level test style elsewhere) with the given anchor count, and
// returns the rendered (sql, args).
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
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
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
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
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
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
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

// seedGridNativeDensity bulk-inserts seriesCount x rowsPerSeries rows, EACH
// carrying a boundsCount-finite-bound layout, via a single vectorized
// `INSERT ... SELECT ... FROM numbers(...)` rather than seedGridNativeSeries'
// own one-literal-tuple-per-row string build — the row counts this file's
// density-guard tests need (tens of thousands) would make the latter slow
// for no reason, since every row is identical apart from its series id and
// timestamp and neither value needs to vary meaningfully for the guard's
// own probes (a plain count() and a max(length(...)), per
// range_bucket_grid_native_bound.go's own "Density guard" doc). Every row
// is stamped at the SAME timestamp (seedTS, inside the caller's own scan
// window) — sufficient for both probes, which only count rows and measure
// array length, never distinctness or ordering.
func seedGridNativeDensity(t *testing.T, exec func(string) error, seriesCount, rowsPerSeries, boundsCount int, seedTS time.Time) {
	t.Helper()
	bounds := make([]string, boundsCount)
	for i := range bounds {
		bounds[i] = fmt.Sprintf("%d.0", i+1)
	}
	boundsLit := "[" + strings.Join(bounds, ",") + "]"
	counts := make([]string, boundsCount+1)
	for i := range counts {
		counts[i] = "1"
	}
	countsLit := "[" + strings.Join(counts, ",") + "]"

	seedSQL := fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, SeriesID, TimeUnix, BucketCounts, ExplicitBounds)
SELECT 'http_server_request_duration', concat('svc-', toString(number %% %d)), toDateTime64('%s', 9), %s, %s
FROM numbers(%d)`,
		seriesCount, seedTS.Format("2006-01-02 15:04:05.000000000"), countsLit, boundsLit, seriesCount*rowsPerSeries)
	if err := exec(seedSQL); err != nil {
		t.Fatalf("seed density insert: %v", err)
	}
}

// TestRangeBucketGridNativeBound_DensityGuardThrowsOnHighRawRowDensity is
// issue #2523's own regression pin: at a `groups x anchors` value trivially
// far under maxRangeBucketGridNativeRows (10 series x 51 rungs x 10 anchors
// = 5,100, vs the 20,000,000 axis1 ceiling), a HIGH raw-row density alone
// must still trip the density guard — proving `groups x anchors` was never
// a complete cost model on its own (this file's header doc's own "A
// genuinely separate finding"). 35,000 raw rows x 50-finite-bound width^2
// (2,500) = 87,500,000 density-marginal units, comfortably past
// maxRangeBucketGridNativeDensityUnits (85,000,000) even before adding the
// (here trivial) groups x anchors term — see
// range_bucket_grid_native_bound.go's own "Density guard" doc for the real
// ClickHouse calibration this is grounded in.
func TestRangeBucketGridNativeBound_DensityGuardThrowsOnHighRawRowDensity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 10
	const numAnchors = 10 // groups x anchors = 10 * 51 * 10 = 5,100, trivial vs the 20,000,000 axis1 ceiling
	const rowsPerSeries = 3_500
	seedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedGridNativeDensity(t, func(s string) error { _, err := db.Exec(s); return err },
		seriesCount, rowsPerSeries, gridNativeBoundBoundsCount, seedStart)

	sqlStr, args := buildGridNativePlan(t, numAnchors)
	_, err := db.Query(sqlStr, args...)
	if err == nil {
		t.Fatal("expected the density guard's throwIf to fire for a high-raw-row-density query, got no error")
	}
	if !strings.Contains(err.Error(), chsql.RangeBucketGridNativeDensityBudgetMessage) {
		t.Errorf("query failed, but not with the expected DENSITY guard message %q: %v",
			chsql.RangeBucketGridNativeDensityBudgetMessage, err)
	}
	if strings.Contains(err.Error(), chsql.RangeBucketGridNativeBudgetMessage) {
		t.Errorf("query failed with axis1's OWN groups-x-anchors message, not the density guard's — "+
			"axis1 should never fire at this trivial groups x anchors (5,100): %v", err)
	}
}

// TestRangeBucketGridNativeBound_DensityGuardPassesLowRawRowDensity is the
// negative control for TestRangeBucketGridNativeBound_DensityGuardThrowsOnHighRawRowDensity:
// the IDENTICAL groups x anchors shape (5,100), but floor raw-row density
// (2 rows/series, matching seedGridNativeSeries' own floor) — must NOT trip
// either guard, proving the density guard's rejection above is really the
// density term firing and not some unrelated failure this emitter's SQL
// would hit at this shape regardless of row count.
func TestRangeBucketGridNativeBound_DensityGuardPassesLowRawRowDensity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 10
	const numAnchors = 10
	const rowsPerSeries = 2
	seedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedGridNativeDensity(t, func(s string) error { _, err := db.Exec(s); return err },
		seriesCount, rowsPerSeries, gridNativeBoundBoundsCount, seedStart)

	sqlStr, args := buildGridNativePlan(t, numAnchors)
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		t.Fatalf("low-density query unexpectedly failed: %v", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// TestRangeBucketGridNativeBound_DensityGuardReproducesKnownDangerousShape
// is a CI-speed-scaled reproduction of issue #2523's own reference
// real-ClickHouse finding: test/perf/nightly/sentinels.go's
// classic_histogram_quantile_by_route sentinel's real shape (3,741 series,
// 11 finite ExplicitBounds -- 12 rungs -- 60 anchors, ~350 samples/series)
// genuinely OOMs real ClickHouse at `groups x anchors` = 2,693,520,
// comfortably UNDER maxRangeBucketGridNativeRows (20,000,000) — the exact
// gap this file's density guard exists to close. This test keeps the SAME
// 12-rung layout (boundsCount=11, matching the real sentinel's own
// ExplicitBounds width) and a `groups x anchors` an order of magnitude
// below the axis1 ceiling, scaled down from 3,741 series to 100 (fast to
// seed) while keeping enough raw rows to cross the density guard's own
// threshold at that width — see range_bucket_grid_native_bound.go's own
// "Density guard" doc for why 11-finite-bound rows need roughly 700,000+
// of them to independently cross maxRangeBucketGridNativeDensityUnits.
func TestRangeBucketGridNativeBound_DensityGuardReproducesKnownDangerousShape(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketGridNativeBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec("SET allow_experimental_time_series_aggregate_functions = 1"); err != nil {
		t.Fatalf("enable experimental: %v", err)
	}

	const seriesCount = 100
	const boundsCount = 11 // matches classic_histogram_quantile_by_route's own 11 finite bounds (12 rungs)
	const numAnchors = 20  // groups x anchors = 100 * 12 * 20 = 24,000, trivial vs the 20,000,000 axis1 ceiling
	const rowsPerSeries = 7_500
	seedStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedGridNativeDensity(t, func(s string) error { _, err := db.Exec(s); return err },
		seriesCount, rowsPerSeries, boundsCount, seedStart)

	start := seedStart
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

	_, err = db.Query(sqlStr, args...)
	if err == nil {
		t.Fatal("expected the density guard's throwIf to fire for the scaled #2486/#2408 reference density, got no error")
	}
	if !strings.Contains(err.Error(), chsql.RangeBucketGridNativeDensityBudgetMessage) {
		t.Errorf("query failed, but not with the expected DENSITY guard message %q: %v",
			chsql.RangeBucketGridNativeDensityBudgetMessage, err)
	}
}
