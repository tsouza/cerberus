//go:build chdb

// chDB-backed differential proof for cerberus issue #2757's two-pass
// sumMap-based exponential-histogram cross-series merge
// (exp_histogram_merge_summap.go): the SAME query and seed, lowered once
// through the default groupArray + picker fold
// (expHistogramGroupMergeFanout) and once through
// NativeExpHistogramMergeLowerer (chopt exp_histogram_merge_summap),
// executed against real ClickHouse (chDB) and compared field-for-field.
//
//   - TestExpHistogramMergeSumMapDifferential_Homogeneous pins identical
//     output for several series sharing one Scale/Offset layout — the
//     realistic OTel-SDK-default shape this issue's own measured win is
//     calibrated on.
//   - TestExpHistogramMergeSumMapDifferential_ScaleNegotiation pins
//     identical output across series with genuinely different Scale,
//     exercising the downscale COLLAPSE case (several of a wide row's own
//     buckets folding onto one merged bucket) — mirrors
//     histogram_merge_scatter_chdb_test.go's own scatter fixture.
//   - TestExpHistogramMergeSumMapDifferential_ZeroBucket pins identical
//     output for a single row whose interior bucket count is exactly zero
//     — the sumMap zero-summed-key drop this file's header documents and
//     expHistogramSumMapLadderExpr's indexOf-based reconstruction works
//     around, mirroring classic_bucket_merge_summap's identical concern.
//   - TestExpHistogramMergeSumMapDifferential_SignBuckets pins identical
//     output when both the positive AND negative ladders carry real data.
//   - TestExpHistogramMergeSumMapDifferential_SingleSeries pins identical
//     output for the trivial one-row case (no real cross-series merge),
//     the "must not regress the simple case" check — see this issue's own
//     measured numbers: at realistic OTel-default width this stays roughly
//     PARITY with the existing fold despite the two-pass double-scan (a
//     real, measured memory regression only shows up for a single series
//     with an unusually WIDE individual layout, not exercised by this
//     fixture).
//
// The five `TestExpHistogramMergeSumMapDifferential_Avg_*` cases below
// mirror the five above, seed-for-seed, over `avg()` instead of `sum()` —
// cerberus issue #2866's own differential proof that
// [expHistogramGroupMergeSumMap]'s series-count collection and division
// (added for avg() by that issue) reconciles bucket ladders identically to
// the fold path's own [expHistogramAvgScaleProjections], not just that the
// SUM-only shape still works. Each Avg case reuses its Sum sibling's exact
// seed builder — same rows, same fixture — so a divergence in either
// aggregation's arithmetic shows up against a shared baseline.
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// expHistSumMapDiffMetric is the metric name every case below queries —
// distinct from other fixtures sharing this package's chDB session
// (fixture_chdb_test.go).
const expHistSumMapDiffMetric = "exp_histogram_merge_summap_diff_exp_hist"

var expHistSumMapDiffEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// expHistSumMapDiffLowerers is the two strategies under differential test:
// fanout (the default groupArray + picker merge) and native (this issue's
// two-pass sumMap merge).
func expHistSumMapDiffLowerers(native bool) promql.RangeLowerers {
	if !native {
		return promql.RangeLowerers{}
	}
	return promql.RangeLowerers{
		ExpHistogramMerge: promql.NativeExpHistogramMergeLowerer{},
	}
}

// expHistSumMapDiffRow is one merged-histogram output row, projected as
// strings/ints for stable equality comparison across the two strategies.
type expHistSumMapDiffRow struct {
	scale                  int64
	zeroCount              float64
	posOffset, negOffset   int64
	posBuckets, negBuckets string
	count, sum             float64
}

// runExpHistSumMapDiffQuery lowers `sum(<metric>)` under the given
// strategy and returns the single resulting merged-histogram row.
func runExpHistSumMapDiffQuery(t *testing.T, fixture *chdbFixture, native bool) expHistSumMapDiffRow {
	t.Helper()
	return runExpHistSumMapDiffQueryAgg(t, fixture, native, "sum")
}

// runExpHistSumMapDiffQueryAvg is [runExpHistSumMapDiffQuery]'s `avg()`
// twin — cerberus issue #2866's own eligibility widening.
func runExpHistSumMapDiffQueryAvg(t *testing.T, fixture *chdbFixture, native bool) expHistSumMapDiffRow {
	t.Helper()
	return runExpHistSumMapDiffQueryAgg(t, fixture, native, "avg")
}

// runExpHistSumMapDiffQueryAgg lowers `<aggFn>(<metric>)` under the given
// strategy and returns the single resulting merged-histogram row. aggFn is
// "sum" or "avg" — the two shapes [NativeExpHistogramMergeLowerer] routes
// onto [expHistogramGroupMergeSumMap].
func runExpHistSumMapDiffQueryAgg(t *testing.T, fixture *chdbFixture, native bool, aggFn string) expHistSumMapDiffRow {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf("%s(%s)", aggFn, expHistSumMapDiffMetric)
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, expHistSumMapDiffEvalTS, expHistSumMapDiffEvalTS, 0,
		promql.LowerOpts{Lowerers: expHistSumMapDiffLowerers(native)})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(native=%v): %v", native, err)
	}
	// toString(...) throughout — chdb-go's parquet driver cannot decode
	// Map/Array cells directly into a Go destination (see
	// chdbFixture.queryOverEmitted's own doc).
	rows := fixture.queryOverEmitted(t,
		"HistogramScale, toString(HistogramZeroCount), HistogramPositiveOffset, toString(HistogramPositiveBucketCounts), "+
			"HistogramNegativeOffset, toString(HistogramNegativeBucketCounts), toString(HistogramCount), toString(HistogramSum)",
		sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("query error (native=%v): %v", native, err)
		}
		t.Fatalf("query (native=%v) returned no rows", native)
	}
	var row expHistSumMapDiffRow
	var zeroCountStr, countStr, sumStr string
	if err := rows.Scan(&row.scale, &zeroCountStr, &row.posOffset, &row.posBuckets, &row.negOffset, &row.negBuckets, &countStr, &sumStr); err != nil {
		t.Fatalf("scan (native=%v): %v", native, err)
	}
	if rows.Next() {
		t.Fatalf("query (native=%v) returned more than one row", native)
	}
	row.zeroCount = mustParseFloat(t, zeroCountStr)
	row.count = mustParseFloat(t, countStr)
	row.sum = mustParseFloat(t, sumStr)
	return row
}

func mustParseFloat(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		t.Fatalf("parse float %q: %v", s, err)
	}
	return f
}

func assertExpHistSumMapDiffEqual(t *testing.T, fanout, native expHistSumMapDiffRow) {
	t.Helper()
	if fanout != native {
		t.Fatalf("fanout and native (sumMap) merges disagree:\n  fanout = %+v\n  native = %+v", fanout, native)
	}
}

// expHistSumMapDiffSeedDDL mirrors histogramMergeBoundSeedDDL — this
// file's own copy since Go test binaries don't share consts across
// packages.
const expHistSumMapDiffSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// newExpHistSumMapDiffFixtureHomogeneous seeds three series sharing one
// Scale(0)/Offset(0) layout, each a plain 1-bucket-wide positive-only
// histogram — the realistic OTel-SDK-default shape this issue's own
// measured 13-43x win (at hundreds to thousands of rows) is calibrated on.
// Shared by the Sum and Avg differential tests below.
func newExpHistSumMapDiffFixtureHomogeneous(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 6, 12.0, 0, 0, 0, [1,2,3], 0, [])", expHistSumMapDiffMetric),
		fmt.Sprintf("('%s', map('series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 9, 20.0, 0, 0, 0, [4,5,0], 0, [])", expHistSumMapDiffMetric),
		fmt.Sprintf("('%s', map('series', 's3'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [1,1,1], 0, [])", expHistSumMapDiffMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapDifferential_Homogeneous is the `sum()` case
// over [newExpHistSumMapDiffFixtureHomogeneous].
func TestExpHistogramMergeSumMapDifferential_Homogeneous(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureHomogeneous(t)
	fanout := runExpHistSumMapDiffQuery(t, fixture, false)
	native := runExpHistSumMapDiffQuery(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapDifferential_Avg_Homogeneous is
// [TestExpHistogramMergeSumMapDifferential_Homogeneous]'s `avg()` twin
// (cerberus issue #2866): the SAME seed, both strategies now dividing by
// the group's 3-series member count.
func TestExpHistogramMergeSumMapDifferential_Avg_Homogeneous(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureHomogeneous(t)
	fanout := runExpHistSumMapDiffQueryAvg(t, fixture, false)
	native := runExpHistSumMapDiffQueryAvg(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// newExpHistSumMapDiffFixtureScaleNegotiation seeds three series with
// genuinely different Scale (0, 2, 1) and different PositiveOffset —
// mirrors histogram_merge_scatter_chdb_test.go's own scatter fixture,
// exercising both the no-collapse (ratio=1, s1) and collapse (ratio>1, s2
// and s3) downscale paths in the SAME merge. Shared by the Sum and Avg
// differential tests below.
func newExpHistSumMapDiffFixtureScaleNegotiation(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 10, 1.0, 0, 0, 5, [1,2,3,4], 0, [])", expHistSumMapDiffMetric),
		fmt.Sprintf("('%s', map('series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 283, 1.0, 2, 0, 17, [10,20,30,40,50,60,70], 0, [])", expHistSumMapDiffMetric),
		fmt.Sprintf("('%s', map('series', 's3'), toDateTime64('2026-01-01 00:00:00', 9), 630, 1.0, 1, 0, 9, [100,200,300], 0, [])", expHistSumMapDiffMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapDifferential_ScaleNegotiation is the `sum()`
// case over [newExpHistSumMapDiffFixtureScaleNegotiation].
func TestExpHistogramMergeSumMapDifferential_ScaleNegotiation(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureScaleNegotiation(t)
	fanout := runExpHistSumMapDiffQuery(t, fixture, false)
	native := runExpHistSumMapDiffQuery(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapDifferential_Avg_ScaleNegotiation is
// [TestExpHistogramMergeSumMapDifferential_ScaleNegotiation]'s `avg()`
// twin (cerberus issue #2866): the downscale-collapse case must still
// merge identically to the fold BEFORE the division, then divide
// identically.
func TestExpHistogramMergeSumMapDifferential_Avg_ScaleNegotiation(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureScaleNegotiation(t)
	fanout := runExpHistSumMapDiffQueryAvg(t, fixture, false)
	native := runExpHistSumMapDiffQueryAvg(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// newExpHistSumMapDiffFixtureZeroBucket seeds a SINGLE series whose middle
// bucket count is exactly zero — sumMap([idx0,idx1,idx2], [5,0,3]) drops
// the zero-valued key entirely (the same quirk
// classic_bucket_merge_summap.go's header documents for the classic
// path). Shared by the Sum and Avg differential tests below.
func newExpHistSumMapDiffFixtureZeroBucket(t *testing.T) *chdbFixture {
	t.Helper()
	return newChDBFixture(t, expHistSumMapDiffSeedDDL+
		"INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n"+
		fmt.Sprintf("    ('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 8, 4.0, 0, 0, 0, [5,0,3], 0, []);\n", expHistSumMapDiffMetric))
}

// TestExpHistogramMergeSumMapDifferential_ZeroBucket is the `sum()` case
// over [newExpHistSumMapDiffFixtureZeroBucket]. Both strategies must
// answer identically: expHistogramSumMapLadderExpr's indexOf-based
// reconstruction restores the dropped zero explicitly.
func TestExpHistogramMergeSumMapDifferential_ZeroBucket(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureZeroBucket(t)
	fanout := runExpHistSumMapDiffQuery(t, fixture, false)
	native := runExpHistSumMapDiffQuery(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapDifferential_Avg_ZeroBucket is
// [TestExpHistogramMergeSumMapDifferential_ZeroBucket]'s `avg()` twin
// (cerberus issue #2866): the dropped-zero-key reconstruction must survive
// the division on top unchanged.
func TestExpHistogramMergeSumMapDifferential_Avg_ZeroBucket(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureZeroBucket(t)
	fanout := runExpHistSumMapDiffQueryAvg(t, fixture, false)
	native := runExpHistSumMapDiffQueryAvg(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// newExpHistSumMapDiffFixtureSignBuckets seeds two series each carrying
// BOTH real positive and real negative bucket data — exercising
// expHistogramGroupMergeAggsSumMap's independent pos/neg sumMap pair
// together. Shared by the Sum and Avg differential tests below.
func newExpHistSumMapDiffFixtureSignBuckets(t *testing.T) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(expHistSumMapDiffSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 21, -3.0, 0, 2, 0, [1,2,3], -4, [4,5,6])", expHistSumMapDiffMetric),
		fmt.Sprintf("('%s', map('series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 15, -1.0, 0, 1, 0, [3,2,1], -3, [1,1,1])", expHistSumMapDiffMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapDifferential_SignBuckets is the `sum()` case
// over [newExpHistSumMapDiffFixtureSignBuckets].
func TestExpHistogramMergeSumMapDifferential_SignBuckets(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureSignBuckets(t)
	fanout := runExpHistSumMapDiffQuery(t, fixture, false)
	native := runExpHistSumMapDiffQuery(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapDifferential_Avg_SignBuckets is
// [TestExpHistogramMergeSumMapDifferential_SignBuckets]'s `avg()` twin
// (cerberus issue #2866): both ladders' merge, ZeroCount included, must
// divide identically to the fold path.
func TestExpHistogramMergeSumMapDifferential_Avg_SignBuckets(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureSignBuckets(t)
	fanout := runExpHistSumMapDiffQueryAvg(t, fixture, false)
	native := runExpHistSumMapDiffQueryAvg(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// newExpHistSumMapDiffFixtureSingleSeries seeds exactly ONE series — no
// real cross-series merge — the "must not regress the simple case" check:
// at this issue's own measured realistic OTel-default width (~160 buckets
// and below), the two-pass design stays roughly PARITY with the fold even
// here despite paying its own double-scan-of-perSeries overhead; see this
// file's header and cerberus issue #2757 for the full measured table (the
// documented memory regression only appears for a SINGLE series with an
// unusually WIDE individual layout, not exercised by this fixture). Shared
// by the Sum and Avg differential tests below.
func newExpHistSumMapDiffFixtureSingleSeries(t *testing.T) *chdbFixture {
	t.Helper()
	return newChDBFixture(t, expHistSumMapDiffSeedDDL+
		"INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n"+
		fmt.Sprintf("    ('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 6, 3.0, 3, 0, -2, [1,2,3], 0, []);\n", expHistSumMapDiffMetric))
}

// TestExpHistogramMergeSumMapDifferential_SingleSeries is the `sum()` case
// over [newExpHistSumMapDiffFixtureSingleSeries].
func TestExpHistogramMergeSumMapDifferential_SingleSeries(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureSingleSeries(t)
	fanout := runExpHistSumMapDiffQuery(t, fixture, false)
	native := runExpHistSumMapDiffQuery(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}

// TestExpHistogramMergeSumMapDifferential_Avg_SingleSeries is
// [TestExpHistogramMergeSumMapDifferential_SingleSeries]'s `avg()` twin
// (cerberus issue #2866): a single-member group's avg() divides by 1 —
// the degenerate case a divide-by-member-count implementation is likeliest
// to get wrong (e.g. dividing by row count from the wrong stage).
func TestExpHistogramMergeSumMapDifferential_Avg_SingleSeries(t *testing.T) {
	fixture := newExpHistSumMapDiffFixtureSingleSeries(t)
	fanout := runExpHistSumMapDiffQueryAvg(t, fixture, false)
	native := runExpHistSumMapDiffQueryAvg(t, fixture, true)
	assertExpHistSumMapDiffEqual(t, fanout, native)
}
