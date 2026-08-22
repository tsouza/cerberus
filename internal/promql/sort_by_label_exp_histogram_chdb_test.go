//go:build chdb

// chDB-backed proof that PromQL `sort_by_label`/`sort_by_label_desc` over an
// exp-histogram-valued vector (cerberus issue #2462) actually PRESERVES the
// histogram sample at real ClickHouse execution, reordered by the named
// label — not merely that the lowered plan's Go shape looks right.
//
// Unlike `sort`/`sort_desc` (#2456, which genuinely drop every histogram
// sample — reference Prometheus's funcSort/funcSortDesc start from
// filterFloats), funcSortByLabel/funcSortByLabelDesc sort the vector by
// label value alone and never touch `.H`/`.F`, so a histogram sample
// survives the sort unchanged. This test seeds real Count/Sum/bucket data
// across several series, sorts by a label, and asserts both the resulting
// ROW ORDER and that the histogram payload (Count) rode through untouched —
// proving lowerSortByLabel's chplan.OrderBy-over-HistogramProjection wiring
// (and chplan.RowShapeOf's OrderBy forwarding arm) actually emit SQL
// ClickHouse accepts and answers correctly, not just SQL that type-checks
// in Go.
package promql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// sortByLabelExpHistogramMetric is the metric name every case below
// queries — distinct from histogramMergeBoundMetric (histogram_merge_
// bound_chdb_test.go) so the two fixtures' seeds never collide on the
// same MetricName within the shared chDB session.
const sortByLabelExpHistogramMetric = "sort_by_label_target_exp_hist"

// sortByLabelExpHistogramSeed declares the exp-histogram table (same shape
// as histogramMergeBoundSeedDDL — CREATE OR REPLACE keeps re-application
// idempotent across the shared in-process chDB session) and seeds three
// series in an order that matches neither ascending nor descending label
// sort, so a passing assertion can only come from the emitted ORDER BY.
// Each series carries a distinct Count so the row-order assertion and the
// payload-survival assertion are the same read.
var sortByLabelExpHistogramSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + sortByLabelExpHistogramMetric + "', map('job', 'c'), toDateTime64('2026-01-01 00:00:00', 9), 30, 3.0, 0, 0, 0, [30], 0, []),\n" +
	"    ('" + sortByLabelExpHistogramMetric + "', map('job', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 10, 1.0, 0, 0, 0, [10], 0, []),\n" +
	"    ('" + sortByLabelExpHistogramMetric + "', map('job', 'b'), toDateTime64('2026-01-01 00:00:00', 9), 20, 2.0, 0, 0, 0, [20], 0, []);\n"

// sortByLabelExpHistogramEvalTS sits one second after every seeded row —
// well inside the bare-selector staleness lookback.
var sortByLabelExpHistogramEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func TestSortByLabel_ExpHistogram_ChDB_PreservesAndOrders(t *testing.T) {
	fixture := newChDBFixture(t, sortByLabelExpHistogramSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name      string
		query     string
		wantJobs  []string
		wantCount []float64
	}{
		{
			name:      "asc",
			query:     fmt.Sprintf(`sort_by_label(%s, "job")`, sortByLabelExpHistogramMetric),
			wantJobs:  []string{"a", "b", "c"},
			wantCount: []float64{10, 20, 30},
		},
		{
			name:      "desc",
			query:     fmt.Sprintf(`sort_by_label_desc(%s, "job")`, sortByLabelExpHistogramMetric),
			wantJobs:  []string{"c", "b", "a"},
			wantCount: []float64{30, 20, 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, sortByLabelExpHistogramEvalTS, sortByLabelExpHistogramEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			// Project the label value plus HistogramCount (the wire alias
			// [chplan.HistogramProjection] publishes for the Count column,
			// see chplan.HistogramCountColumn) as plain scalar columns —
			// the same Map-scan sidestep sort_by_label_chdb_test.go uses —
			// preserving the ORDER BY-produced row order.
			projection := "`Attributes`['job'] AS job, `HistogramCount` AS cnt"
			rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
			defer func() { _ = rows.Close() }()

			var gotJobs []string
			var gotCounts []float64
			for rows.Next() {
				var job string
				var cnt float64
				if err := rows.Scan(&job, &cnt); err != nil {
					t.Fatalf("scan: %v", err)
				}
				gotJobs = append(gotJobs, job)
				gotCounts = append(gotCounts, cnt)
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}

			if len(gotJobs) != len(tc.wantJobs) {
				t.Fatalf("%s: got %d rows %v, want %d %v — the histogram sample(s) must survive the sort, not be dropped",
					tc.query, len(gotJobs), gotJobs, len(tc.wantJobs), tc.wantJobs)
			}
			for i := range tc.wantJobs {
				if gotJobs[i] != tc.wantJobs[i] {
					t.Errorf("%s: job order mismatch at %d: got %v, want %v", tc.query, i, gotJobs, tc.wantJobs)
					break
				}
			}
			for i := range tc.wantCount {
				if gotCounts[i] != tc.wantCount[i] {
					t.Errorf("%s: Count mismatch at row %d (job=%s): got %v, want %v — the histogram payload did not ride through the sort unchanged",
						tc.query, i, gotJobs[i], gotCounts[i], tc.wantCount[i])
				}
			}
		})
	}
}
