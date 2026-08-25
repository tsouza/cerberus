//go:build chdb

// chDB-backed proof that `count()`/`group()` directly wrapping a mixed
// float/histogram `or` (cerberus issue #2595,
// histogram_native_mixed_or_aggregate_presence.go's
// [countOrGroupOverMixedExpHistogramSetOp] /
// [lowerCountOrGroupOverMixedExpHistogramSetOp]) actually counts EVERY
// row the mixed `or` produces — histogram-shaped and float-shaped alike,
// with no drop-on-type-mismatch the way sum()/avg() have — at real
// ClickHouse execution, not merely that the emitted plan's Go shape looks
// right.
package promql_test

import (
	"context"
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
	cgHistMetric  = "cg_wrapped_hist_side_exp_hist"
	cgFloatMetric = "cg_wrapped_float_side_gauge"
)

// cgSeed keys two histogram series ("h1" group="g1", "h2" group="g2") and
// two float series ("f1" group="g1" — SHARES g1 with h1, proving a group
// mixing both types survives count()/group() rather than being dropped
// the way sum()/avg() would drop it; "f2" group="g3" — a float-only
// group).
var cgSeed = "" +
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
	"    ('" + cgHistMetric + "', map('series', 'h1', 'group', 'g1'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + cgHistMetric + "', map('series', 'h2', 'group', 'g2'), toDateTime64('2026-01-01 00:00:00', 9), 3, 9.0, 0, 0, 0, [7], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + cgFloatMetric + "', map('series', 'f1', 'group', 'g1'), toDateTime64('2026-01-01 00:00:00', 9), 55.0),\n" +
	"    ('" + cgFloatMetric + "', map('series', 'f2', 'group', 'g3'), toDateTime64('2026-01-01 00:00:00', 9), 66.0);\n"

var cgEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func cgLower(t *testing.T, s schema.Metrics, p parser.Parser, query string) (string, []any) {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, cgEvalTS, cgEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s (count/group always reduce to a float)", query, shape, chplan.SampleRowShape)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr, args
}

// TestCountOverMixedSetOpOr_ChDB proves count() over a mixed `or` with no
// `by`/`without` clause counts every one of the mixed union's rows,
// histogram-shaped and float-shaped alike.
func TestCountOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, cgSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "count(" + cgHistMetric + " or " + cgFloatMetric + ")"
	sqlStr, args := cgLower(t, s, p, query)
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
	const wantCount = 4.0 // h1, h2, f1, f2 — every seeded series, no drop
	if val != wantCount {
		t.Fatalf("query %q returned Value=%v, want %v (count() must not drop the histogram-shaped rows)", query, val, wantCount)
	}
}

// TestGroupOverMixedSetOpOr_ChDB proves group() over a mixed `or` answers
// 1 for the single (no `by`/`without`) group, regardless of it mixing
// histogram and float samples.
func TestGroupOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, cgSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "group(" + cgHistMetric + " or " + cgFloatMetric + ")"
	sqlStr, args := cgLower(t, s, p, query)
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
	if val != 1.0 {
		t.Fatalf("query %q returned Value=%v, want 1", query, val)
	}
}

// TestCountByOverMixedSetOpOr_ChDB proves the reference-faithful part of
// this issue's fix: a `by(group)` group whose members straddle both
// value types ("g1": h1 + f1) is NOT dropped the way sum()/avg() would
// drop it — it survives with the combined member count, exactly as
// reference's type-agnostic `group.groupCount++` does.
func TestCountByOverMixedSetOpOr_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, cgSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "count by (group) (" + cgHistMetric + " or " + cgFloatMetric + ")"
	sqlStr, args := cgLower(t, s, p, query)
	rows := fixture.queryOverEmitted(t, "`Attributes`['group'] AS grp, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()

	seen := map[string]float64{}
	for rows.Next() {
		var grp string
		var val float64
		if err := rows.Scan(&grp, &val); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[grp] = val
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := map[string]float64{
		"g1": 2, // h1 (histogram) + f1 (float) — mixed, NOT dropped
		"g2": 1, // h2 only (histogram)
		"g3": 1, // f2 only (float)
	}
	for grp, wantVal := range want {
		gotVal, ok := seen[grp]
		if !ok {
			t.Errorf("query %q: no row for group %q, want Value=%v", query, grp, wantVal)
			continue
		}
		if gotVal != wantVal {
			t.Errorf("query %q: group %q Value=%v, want %v", query, grp, gotVal, wantVal)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("query %q: got %d groups %v, want %d groups %v", query, len(seen), seen, len(want), want)
	}
}
