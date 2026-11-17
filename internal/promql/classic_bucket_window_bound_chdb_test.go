//go:build chdb

// chDB-backed proof that the instant classic-histogram per-series window
// guard (classic_bucket_window_bound.go) fires at real ClickHouse execution
// at its exact boundary: the per-series entry count against
// ClassicBucketWindowSeriesMaxEntries.
//
// Every seeded series carries a DISJOINT ExplicitBounds layout (series i's
// bounds are `[i*width+1 .. i*width+width]`).
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

// classicBucketSeedDDL is the OTel-CH classic-histogram table shape the
// default schema reads, matching hqNativeSeedDDL
// (histogram_quantile_classic_native_chdb_test.go, internal/chsql) — this
// package needs its own copy since Go test binaries don't share consts
// across packages.
const classicBucketSeedDDL = `
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

// classicBucketSeedMetric is the metric name every case below queries —
// distinct from other fixtures sharing this package's chDB session (see
// fixture_chdb_test.go) so this test's own predicate can never read a
// sibling fixture's rows.
const classicBucketSeedMetric = "classic_bucket_window_bound_test_metric"

// classicBucketSeedSampleTS is the newest sample timestamp every seeded
// series carries; classicBucketSeedEvalTS reads it inside its window.
var classicBucketSeedSampleTS = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// classicBucketSeedEvalTS is the instant eval time — one second after the
// newest sample, well inside the sum_over_time(...[5m]) window.
var classicBucketSeedEvalTS = classicBucketSeedSampleTS.Add(time.Second)

// classicBucketSeedSampleSpacingSeconds separates one series' consecutive
// samples. With at most classicBucketSeedMaxSamples samples a series spans
// under two minutes, so every sample sits inside the [5m] window of the
// instant eval time.
const classicBucketSeedSampleSpacingSeconds = 10

// classicBucketSeedMaxSamples is the most samples per series a seed may
// carry and keep every sample inside the evaluated window.
const classicBucketSeedMaxSamples = 10

// classicBucketSeedQuery is the aggregated classic-histogram idiom that
// reaches the per-series window stage: `sum by(le)(sum_over_time(...))`,
// matched against histogramAggShapeLowerable (sum_over_time's 1-sample
// floor, unlike rate's 2, lets a single seeded sample per series exercise
// it).
const classicBucketSeedQuery = "histogram_quantile(0.5, sum by(le)(sum_over_time(" +
	classicBucketSeedMetric + "_bucket[5m])))"

// classicBucketSeedShape is one seed: series series, each with its own
// disjoint width-wide layout, each carrying samples in-window samples.
type classicBucketSeedShape struct {
	series, width, samples int
}

// seedClassicBucketRows renders an INSERT of shape: series i's bounds are
// `[i*width+1 .. i*width+width]` (see this file's header doc), one
// observation per bucket per sample.
func seedClassicBucketRows(t *testing.T, shape classicBucketSeedShape) string {
	t.Helper()
	if shape.samples < 1 || shape.samples > classicBucketSeedMaxSamples {
		t.Fatalf("seed %+v: samples must lie in [1, %d]", shape, classicBucketSeedMaxSamples)
	}
	return fmt.Sprintf(`INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds)
SELECT '%s', map('svc', toString(intDiv(number, %d))),
  toDateTime64('%s', 9) - toIntervalSecond((number %% %d) * %d),
  arrayWithConstant(%d, toUInt64(1)),
  arrayMap(j -> toFloat64(intDiv(number, %d) * %d + j), range(1, %d))
FROM numbers(%d)`,
		classicBucketSeedMetric, shape.samples,
		classicBucketSeedSampleTS.Format("2006-01-02 15:04:05"), shape.samples, classicBucketSeedSampleSpacingSeconds,
		shape.width+1,
		shape.samples, shape.width, shape.width+1,
		shape.series*shape.samples)
}

// newClassicBucketSeedFixture seeds shape into a fresh fixture.
func newClassicBucketSeedFixture(t *testing.T, shape classicBucketSeedShape) *chdbFixture {
	t.Helper()
	return newChDBFixture(t, classicBucketSeedDDL+seedClassicBucketRows(t, shape)+";\n")
}

// runClassicBucketSeedInstantQuery lowers + emits classicBucketSeedQuery in
// instant mode under opts and runs it against fixture, returning the query
// error (nil on success). It wraps the statement in `SELECT count() FROM
// (...)`, mirroring runHistogramMergeBoundQuery's identical rationale: the
// merged output carries Map/Array histogram columns chdb-go's parquet driver
// cannot decode, and a throwIf inside a column expression still fires even
// though the outer SELECT never reads that column's value.
func runClassicBucketSeedInstantQuery(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	expr, err := promparser.NewParser(promparser.Options{}).ParseExpr(classicBucketSeedQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", classicBucketSeedQuery, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, classicBucketSeedEvalTS, classicBucketSeedEvalTS, 0, opts)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", classicBucketSeedQuery, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", classicBucketSeedQuery, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	rows, qerr := fixture.Query(context.Background(), "SELECT count() FROM ("+sqlStr+")", args...)
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

// windowSeriesEntries is the bucket entries one series of shape collects in
// the window: every sample's finite bounds plus its +Inf rung.
func windowSeriesEntries(sh classicBucketSeedShape) int64 {
	return int64(sh.samples) * int64(sh.width+1)
}

// windowBounds is the resource bounds with the per-series window ceiling
// set.
func windowBounds(maxSeriesEntries int64) promql.LowerOpts {
	return promql.LowerOpts{ResourceBounds: promql.ResourceBounds{
		ClassicBucketWindowSeriesMaxEntries: maxSeriesEntries,
	}}
}

// classicBucketWindowBoundaryShapes cover a single-sample seed, a
// multi-sample one, a one-bound-per-series one and a few wide rows.
var classicBucketWindowBoundaryShapes = []struct {
	name  string
	shape classicBucketSeedShape
}{
	{"single-sample", classicBucketSeedShape{series: 40, width: 20, samples: 1}},
	{"multi-sample", classicBucketSeedShape{series: 40, width: 20, samples: 6}},
	{"one-bound-rows", classicBucketSeedShape{series: 200, width: 1, samples: 3}},
	{"wide-rows", classicBucketSeedShape{series: 4, width: 300, samples: 2}},
}

// TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries pins
// the per-series check at its exact boundary: a ceiling equal to one
// series' window entries admits the query, one lower refuses it with the
// per-series message. The admitted runs go first: chDB's Parquet output
// after a throwIf error is malformed (cerberus #3761).
func TestClassicBucketWindowBudget_ChDB_SeriesBoundaryIsTheSeriesEntries(t *testing.T) {
	for _, tc := range classicBucketWindowBoundaryShapes {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newClassicBucketSeedFixture(t, tc.shape)
			entries := windowSeriesEntries(tc.shape)
			if err := runClassicBucketSeedInstantQuery(t, fixture, windowBounds(entries)); err != nil {
				t.Fatalf("%+v: a ceiling of exactly %d entries per series must admit the query: %v", tc.shape, entries, err)
			}
			err := runClassicBucketSeedInstantQuery(t, fixture, windowBounds(entries-1))
			if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
				t.Fatalf("%+v: a ceiling one below %d entries per series must be refused by the per-series check: %v", tc.shape, entries, err)
			}
		})
	}
}

// TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries seeds one
// series of a single sample past the default per-series ceiling and asserts
// the default bounds refuse it with the per-series message: the ceiling
// counts entries, so one sample that wide is refused like a long series of
// as many entries.
func TestClassicBucketWindowBudget_ChDB_DefaultRefusesOneWideSeries(t *testing.T) {
	ceiling := promql.ClassicBucketWindowSeriesEntriesForMemory(0)
	shape := classicBucketSeedShape{series: 1, width: int(ceiling), samples: 1}
	if got := windowSeriesEntries(shape); got <= ceiling {
		t.Fatalf("seed %+v carries %d entries; want past the default per-series ceiling %d", shape, got, ceiling)
	}
	fixture := newClassicBucketSeedFixture(t, shape)
	err := runClassicBucketSeedInstantQuery(t, fixture, promql.LowerOpts{})
	if err == nil || !strings.Contains(err.Error(), chplan.ClassicBucketWindowSeriesBudgetMessage) {
		t.Fatalf("seed %+v: the default bounds must refuse it with the per-series message: %v", shape, err)
	}
}
