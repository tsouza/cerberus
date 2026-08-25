//go:build chdb

// chDB-backed proof that unary `-`/`+` over a native (OTel exponential)
// histogram selector actually negates / passes through the row's nine
// Histogram*Column fields at real ClickHouse execution (cerberus issue
// #2583) — not merely that the emitted plan's Go shape looks right.
//
// Before this fix, `-demo_latency_exp_hist` hard-rejected via
// expHistogramSelectorRouting's catch-all: unary.go's lowerUnary called
// the generic lower() dispatcher on its operand with no histogram-aware
// opt-in, so a bare exp-histogram selector under a UnaryExpr fell
// straight through to lowerVectorSelector's rejection.
//
// The seed deliberately gives every one of the nine structural/count
// fields a DISTINCT, non-degenerate value (Count=5, Sum=12.5, Scale=3,
// ZeroCount=2, PositiveOffset=1, PositiveBucketCounts=[4,6],
// NegativeOffset=2, NegativeBucketCounts=[3]) so a lowering that negates
// the wrong subset — e.g. scaling Scale/offsets too (which reference's
// FloatHistogram.Mul leaves untouched — they describe where the buckets
// SIT, not how much fell into them) or forgetting ZeroCount (which
// reference's Mul DOES negate, confirmed by reading
// github.com/tsouza/prometheus's model/histogram/float_histogram.go
// directly rather than assumed) — is caught rather than passing by
// coincidence.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const unaryExpHistMetric = "unary_wrapped_exp_hist"

var unaryExpHistSeed = "" +
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
	"    ('" + unaryExpHistMetric + "', map('series', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 5, 12.5, 3, 2, 1, [4, 6], 2, [3]);\n"

var unaryExpHistEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestUnaryMinusExpHistogram_ChDB proves `-<exp-hist selector>` negates
// exactly the five COUNT-bearing fields — Count, Sum, ZeroCount, and both
// signed bucket ladders element-wise — and leaves Scale and both bucket
// offsets untouched, matching reference Prometheus's
// FloatHistogram.Mul(-1).
func TestUnaryMinusExpHistogram_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, unaryExpHistSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "-" + unaryExpHistMetric
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, unaryExpHistEvalTS, unaryExpHistEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`Attributes`['series'] AS series, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramZeroCount` AS zc, " +
		"`HistogramScale` AS scale, " +
		"`HistogramPositiveOffset` AS posoff, `HistogramPositiveBucketCounts`[1] AS pos1, `HistogramPositiveBucketCounts`[2] AS pos2, " +
		"`HistogramNegativeOffset` AS negoff, `HistogramNegativeBucketCounts`[1] AS neg1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	gotRows := 0
	for rows.Next() {
		gotRows++
		var series string
		var cnt, sum, zc, pos1, pos2, neg1 float64
		var scale, posoff, negoff int64
		if err := rows.Scan(&series, &cnt, &sum, &zc, &scale, &posoff, &pos1, &pos2, &negoff, &neg1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if series != "a" {
			t.Errorf("series = %q, want %q", series, "a")
		}
		// Negated: the five COUNT-bearing fields.
		if cnt != -5 {
			t.Errorf("HistogramCount = %v, want -5 (raw seeded value is 5)", cnt)
		}
		if sum != -12.5 {
			t.Errorf("HistogramSum = %v, want -12.5 (raw seeded value is 12.5)", sum)
		}
		if zc != -2 {
			t.Errorf("HistogramZeroCount = %v, want -2 (raw seeded value is 2 — reference's FloatHistogram.Mul negates ZeroCount too)", zc)
		}
		if pos1 != -4 || pos2 != -6 {
			t.Errorf("HistogramPositiveBucketCounts = [%v, %v], want [-4, -6] (raw seeded values are [4, 6])", pos1, pos2)
		}
		if neg1 != -3 {
			t.Errorf("HistogramNegativeBucketCounts[1] = %v, want -3 (raw seeded value is 3)", neg1)
		}
		// Untouched: the structural fields describing where the buckets
		// SIT on the value axis, not how much fell into them.
		if scale != 3 {
			t.Errorf("HistogramScale = %v, want 3 unchanged (unary minus must not rescale bucket boundaries)", scale)
		}
		if posoff != 1 {
			t.Errorf("HistogramPositiveOffset = %v, want 1 unchanged", posoff)
		}
		if negoff != 2 {
			t.Errorf("HistogramNegativeOffset = %v, want 2 unchanged", negoff)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if gotRows != 1 {
		t.Fatalf("%s: got %d rows, want 1", query, gotRows)
	}
}

// TestUnaryPlusExpHistogram_ChDB proves `+<exp-hist selector>` is the
// identity — reference Prometheus's UnaryExpr evaluator returns the
// operand's own histogram sample unchanged for `+` — over a real chDB
// execution.
func TestUnaryPlusExpHistogram_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, unaryExpHistSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "+" + unaryExpHistMetric
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, unaryExpHistEvalTS, unaryExpHistEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`Attributes`['series'] AS series, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramZeroCount` AS zc, " +
		"`HistogramScale` AS scale, " +
		"`HistogramPositiveOffset` AS posoff, `HistogramPositiveBucketCounts`[1] AS pos1, `HistogramPositiveBucketCounts`[2] AS pos2, " +
		"`HistogramNegativeOffset` AS negoff, `HistogramNegativeBucketCounts`[1] AS neg1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	gotRows := 0
	for rows.Next() {
		gotRows++
		var series string
		var cnt, sum, zc, pos1, pos2, neg1 float64
		var scale, posoff, negoff int64
		if err := rows.Scan(&series, &cnt, &sum, &zc, &scale, &posoff, &pos1, &pos2, &negoff, &neg1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if series != "a" {
			t.Errorf("series = %q, want %q", series, "a")
		}
		if cnt != 5 || sum != 12.5 || zc != 2 {
			t.Errorf("Count/Sum/ZeroCount = %v/%v/%v, want 5/12.5/2 unchanged (unary + is the identity)", cnt, sum, zc)
		}
		if pos1 != 4 || pos2 != 6 || neg1 != 3 {
			t.Errorf("bucket ladders = pos[%v,%v] neg[%v], want pos[4,6] neg[3] unchanged", pos1, pos2, neg1)
		}
		if scale != 3 || posoff != 1 || negoff != 2 {
			t.Errorf("Scale/PositiveOffset/NegativeOffset = %v/%v/%v, want 3/1/2 unchanged", scale, posoff, negoff)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if gotRows != 1 {
		t.Fatalf("%s: got %d rows, want 1", query, gotRows)
	}
}
