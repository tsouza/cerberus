//go:build chdb

// chDB-backed proof that `min()`/`max()`/`stddev()`/`stdvar()` directly
// wrapping a mixed float/histogram `or` (cerberus issue #2595,
// histogram_native_mixed_or_aggregate_float_only.go's
// [floatOnlyAggOverMixedExpHistogramSetOp] /
// [lowerFloatOnlyAggOverMixedExpHistogramSetOp]) actually IGNORE every
// histogram-shaped row and reduce over the float-shaped rows alone at
// real ClickHouse execution — including the reference-faithful "no output
// row at all" answer for a `by(...)` group whose only member is
// histogram-shaped.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	foHistMetric  = "fo_wrapped_hist_side_exp_hist"
	foFloatMetric = "fo_wrapped_float_side_gauge"
)

// foSeed keys two histogram series ("h1" bucket="b1", "h2" bucket="b2" —
// the ONLY member of its bucket) and three float series ("f1"=3
// bucket="b1", "f2"=9 bucket="b1", "f3"=1 bucket="b3" — the ONLY member
// of its bucket).
var foSeed = "" +
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
	"    ('" + foHistMetric + "', map('series', 'h1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + foHistMetric + "', map('series', 'h2', 'bucket', 'b2'), toDateTime64('2026-01-01 00:00:00', 9), 3, 9.0, 0, 0, 0, [7], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + foFloatMetric + "', map('series', 'f1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),\n" +
	"    ('" + foFloatMetric + "', map('series', 'f2', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 9.0),\n" +
	"    ('" + foFloatMetric + "', map('series', 'f3', 'bucket', 'b3'), toDateTime64('2026-01-01 00:00:00', 9), 1.0);\n"

var foEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func foLower(t *testing.T, s schema.Metrics, p parser.Parser, query string) (string, []any) {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (min/max/stddev/stdvar always reduce to a float)", query, shape, chplan.SampleRowShape)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr, args
}

// foSingleValue runs query (expected to produce exactly one output row,
// no `by`/`without`) and returns its Value.
func foSingleValue(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) float64 {
	t.Helper()
	sqlStr, args := foLower(t, s, p, query)
	rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("query %q returned no rows", query)
	}
	var val float64
	if err := rows.Scan(&val); err != nil {
		t.Fatalf("scan Value: %v", err)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return val
}

// TestMinMaxStddevStdvarOverMixedSetOpOr_ChDB proves all four ops, with
// no `by`/`without` clause, ignore the two histogram-shaped rows (h1, h2)
// entirely and reduce over the three float-shaped rows (3, 9, 1) alone.
func TestMinMaxStddevStdvarOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	orExpr := "(" + foHistMetric + " or " + foFloatMetric + ")"

	cases := []struct {
		op   string
		want float64
	}{
		{"min", 1},
		{"max", 9},
		// mean(3, 9, 1) = 13/3; population variance/stddev over those
		// three floats alone.
		{"stdvar", 11.555555555555557},
		{"stddev", 3.39934634239519},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			query := tc.op + "(" + orExpr + ")"
			got := foSingleValue(t, fixture, s, p, query)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("%s: Value = %v, want %v (a histogram-blind bug would include h1/h2's placeholder 0.0 in the reduction)", query, got, tc.want)
			}
		})
	}
}

// TestMinByOverMixedSetOpOr_AllHistogramGroupIsAbsent_ChDB proves the
// reference-faithful edge case: a `by(bucket)` group whose ONLY member is
// histogram-shaped ("b2": h2 alone) never becomes "seen" for these four
// ops and so produces NO output row at all — not a zero, not the
// histogram's own placeholder Value.
func TestMinByOverMixedSetOpOr_AllHistogramGroupIsAbsent_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "min by (bucket) (" + foHistMetric + " or " + foFloatMetric + ")"
	sqlStr, args := foLower(t, s, p, query)
	rows := fixture.queryOverEmitted(t, "`Attributes`['bucket'] AS bucket, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	seen := map[string]float64{}
	for rows.Next() {
		var bucket string
		var val float64
		if err := rows.Scan(&bucket, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[bucket] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := map[string]float64{
		"b1": 3, // h1 (ignored) + f1(3) + f2(9) -> min(3,9) = 3
		"b3": 1, // f3 alone
	}
	for bucket, wantVal := range want {
		gotVal, ok := seen[bucket]
		if !ok {
			t.Errorf("query %q: no row for bucket %q, want Value=%v", query, bucket, wantVal)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: bucket %q Value=%v, want %v", query, bucket, gotVal, wantVal)
		}
	}
	if val, ok := seen["b2"]; ok {
		t.Errorf("query %q: got a row for bucket %q (Value = %v), want none (its only member is histogram-shaped, so reference never marks the group seen)", query, "b2", val)
	}
	if len(seen) != len(want) {
		t.Errorf("query %q: got %d buckets %v, want %d buckets %v", query, len(seen), seen, len(want), want)
	}
}
