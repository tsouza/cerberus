//go:build chdb

// chDB-backed proof that the classic-histogram cross-series bucket merge
// budget guard (#2408, classic_bucket_merge_bound.go) actually FIRES at
// real ClickHouse execution — not merely that the emitted SQL contains the
// right tokens — and that it stays silent on a legitimate,
// well-within-budget merge, at BOTH call sites that build a
// classicBucketMergeShaping-shaped Aggregate: the instant-mode
// `histogram_quantile(phi, sum by(le)(...))` (histogram_quantile.go) and
// the range-mode query_range equivalent (histogram_quantile_range.go's
// lowerHistogramQuantileClassicAggRange — the exact function issue #2408's
// own audit names).
//
// Every seeded series carries a DISJOINT ExplicitBounds layout (series i's
// bounds are `[i*width+1 .. i*width+width]`) — classic_bucket_merge_bound.go's
// own header doc calibration seed, the worst case for the merge, where the
// merged union width and the total bucket-element volume grow together.
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// classicBucketMergeBoundSeedDDL is the OTel-CH classic-histogram table
// shape the default schema reads, matching hqNativeSeedDDL
// (histogram_quantile_classic_native_chdb_test.go, internal/chsql) — this
// package needs its own copy since Go test binaries don't share consts
// across packages.
const classicBucketMergeBoundSeedDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

// classicBucketMergeBoundMetric is the metric name every case below
// queries — distinct from other fixtures sharing this package's chDB
// session (see fixture_chdb_test.go) so this test's own predicate can never
// read a sibling fixture's rows.
const classicBucketMergeBoundMetric = "classic_bucket_merge_bound_test_metric"

// classicBucketMergeBoundSampleTS is the single sample timestamp every
// seeded series carries; classicBucketMergeBoundEvalTS (instant path) and
// the range-mode grid (below) both read it inside their lookback window.
var classicBucketMergeBoundSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// classicBucketMergeBoundEvalTS is the instant eval time — one second after
// the sample, well inside the sum_over_time(...[5m]) window.
var classicBucketMergeBoundEvalTS = classicBucketMergeBoundSampleTS.Add(time.Second)

// classicBucketMergeBoundQuery is the aggregated classic-histogram idiom
// that reaches classicBucketMergeShaping in both the instant and range
// lowerers: `sum by(le)(sum_over_time(...))`, matched against
// histogramAggShapeLowerable (sum_over_time's 1-sample floor, unlike
// rate's 2, lets a single seeded sample per series exercise it).
const classicBucketMergeBoundQuery = "histogram_quantile(0.5, sum by(le)(sum_over_time(" +
	classicBucketMergeBoundMetric + "_bucket[5m])))"

// seedClassicBucketMergeBoundRows renders seriesCount INSERT tuples, each
// carrying a disjoint width-wide ExplicitBounds layout — see this file's
// header doc.
func seedClassicBucketMergeBoundRows(seriesCount, width int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES ")
	for i := 0; i < seriesCount; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		bounds := make([]string, width)
		for j := range bounds {
			bounds[j] = fmt.Sprintf("%d.0", i*width+j+1)
		}
		counts := make([]string, width+1)
		for j := range counts {
			counts[j] = "1"
		}
		fmt.Fprintf(&b, "('%s', map('svc', 'svc-%d'), toDateTime64('%s', 9), [%s], [%s])",
			classicBucketMergeBoundMetric, i,
			classicBucketMergeBoundSampleTS.Format("2006-01-02 15:04:05"),
			strings.Join(counts, ","), strings.Join(bounds, ","))
	}
	return b.String()
}

// runClassicBucketMergeBoundInstantQuery lowers + emits the instant-mode
// query and runs it against fixture, returning the query error (nil on
// success). Wraps in `SELECT count() FROM (...)`, mirroring
// runHistogramMergeBoundQuery's identical rationale: the merged output
// carries Map/Array histogram columns chdb-go's parquet driver cannot
// decode, and a throwIf inside a column expression still fires even though
// the outer SELECT never reads that column's value.
func runClassicBucketMergeBoundInstantQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(classicBucketMergeBoundQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", classicBucketMergeBoundQuery, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, classicBucketMergeBoundEvalTS, classicBucketMergeBoundEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", classicBucketMergeBoundQuery, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", classicBucketMergeBoundQuery, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.db.Query(wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		t.Fatal("count() query returned no rows")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	return rows.Err()
}

// runClassicBucketMergeBoundRangeQuery is
// runClassicBucketMergeBoundInstantQuery's range-mode counterpart: lowers
// via LowerAtRangeOpts over a grid covering classicBucketMergeBoundSampleTS,
// reaching lowerHistogramQuantileClassicAggRange — the exact function
// issue #2408's own audit names as the shared, unguarded merge's caller in
// range mode.
func runClassicBucketMergeBoundRangeQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(classicBucketMergeBoundQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", classicBucketMergeBoundQuery, err)
	}
	gridStart := classicBucketMergeBoundSampleTS
	gridEnd := gridStart.Add(2 * time.Minute)
	const gridStep = time.Minute
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, gridStart, gridEnd, gridStep, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", classicBucketMergeBoundQuery, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", classicBucketMergeBoundQuery, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.db.Query(wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan count(): %v", err)
		}
	}
	return rows.Err()
}

// TestClassicBucketMergeBudget_ChDB_Instant_Exceeded seeds 2,000 series with
// disjoint 100-wide ExplicitBounds layouts — totalBucketVolume (T) =
// 200,000, widestRowBucketWidth = 100, cost = T x 100 = 20,000,000, twice
// maxClassicBucketMergeCostUnits (10,000,000) — and asserts the instant
// query aborts with the budget guard's own throwIf. This is the exact
// (series=2000, width=100) point classic_bucket_merge_bound.go's own header
// doc measured on real ClickHouse 25.9-alpine: a genuine
// MEMORY_LIMIT_EXCEEDED abort, which this guard now rejects before
// ClickHouse ever allocates for it.
func TestClassicBucketMergeBudget_ChDB_Instant_Exceeded(t *testing.T) {
	const seriesOverBudget = 2000
	const widthOverBudget = 100

	var b strings.Builder
	b.WriteString(classicBucketMergeBoundSeedDDL)
	b.WriteString(seedClassicBucketMergeBoundRows(seriesOverBudget, widthOverBudget) + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runClassicBucketMergeBoundInstantQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the classic-bucket merge budget guard to abort the query " +
			"(2,000 series x width 100, disjoint layouts, exceeds the cost budget), got no error")
	}
	// Mirrors histogram_merge_bound_chdb_test.go's own rationale: this test
	// drives chdb-go's raw driver handle directly, never through chclient,
	// so a raw substring check against chdb-go's own exception text is what
	// proves the emitted SQL's throwIf fired, carrying the right message.
	if !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the classic-bucket merge budget guard's throwIf: %v", err)
	}
}

// TestClassicBucketMergeBudget_ChDB_Instant_WithinBudget seeds a small,
// legitimate merge (50 series, disjoint 20-wide layouts — cost = 50x20 x 20
// = 20,000, comfortably under budget) and asserts the query succeeds.
func TestClassicBucketMergeBudget_ChDB_Instant_WithinBudget(t *testing.T) {
	const seriesUnderBudget = 50
	const widthUnderBudget = 20

	var b strings.Builder
	b.WriteString(classicBucketMergeBoundSeedDDL)
	b.WriteString(seedClassicBucketMergeBoundRows(seriesUnderBudget, widthUnderBudget) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runClassicBucketMergeBoundInstantQuery(t, fixture); err != nil {
		t.Fatalf("a legitimate 50-series merge must not trip the budget guard: %v", err)
	}
}

// TestClassicBucketMergeBudget_ChDB_Range_Exceeded is
// TestClassicBucketMergeBudget_ChDB_Instant_Exceeded's range-mode
// counterpart, proving the SAME guard fires through
// lowerHistogramQuantileClassicAggRange (histogram_quantile_range.go) —
// the function issue #2408's own audit names as the range-mode caller of
// the shared, previously-unguarded merge.
func TestClassicBucketMergeBudget_ChDB_Range_Exceeded(t *testing.T) {
	const seriesOverBudget = 2000
	const widthOverBudget = 100

	var b strings.Builder
	b.WriteString(classicBucketMergeBoundSeedDDL)
	b.WriteString(seedClassicBucketMergeBoundRows(seriesOverBudget, widthOverBudget) + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runClassicBucketMergeBoundRangeQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the classic-bucket merge budget guard to abort the range query " +
			"(2,000 series x width 100, disjoint layouts, exceeds the cost budget), got no error")
	}
	if !strings.Contains(err.Error(), chplan.ClassicBucketMergeBudgetMessage) {
		t.Fatalf("range query failed, but not with the classic-bucket merge budget guard's throwIf: %v", err)
	}
}

// TestClassicBucketMergeBudget_ChDB_Range_WithinBudget is the range-mode
// negative control, mirroring TestClassicBucketMergeBudget_ChDB_Instant_WithinBudget.
func TestClassicBucketMergeBudget_ChDB_Range_WithinBudget(t *testing.T) {
	const seriesUnderBudget = 50
	const widthUnderBudget = 20

	var b strings.Builder
	b.WriteString(classicBucketMergeBoundSeedDDL)
	b.WriteString(seedClassicBucketMergeBoundRows(seriesUnderBudget, widthUnderBudget) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runClassicBucketMergeBoundRangeQuery(t, fixture); err != nil {
		t.Fatalf("a legitimate 50-series range merge must not trip the budget guard: %v", err)
	}
}
