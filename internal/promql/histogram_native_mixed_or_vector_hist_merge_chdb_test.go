//go:build chdb

// chDB-backed proof that `+`/`-` between two mixed float/histogram `or`
// operands, when BOTH operands resolve histogram-valued for a series,
// performs a genuine bucket-scale reconciliation rather than a naive
// same-index sum — the hard case
// histogram_native_mixed_or_vector_arithmetic.go's
// [mixedVVHistMergeInputProjections] / [mixedVVHistMergeOutputProjections]
// exist for (cerberus issue #2449). The companion test
// TestVectorVectorArithmeticOverMixedSetOpOr_ChDB (histogram_native_mixed_or_
// vector_arithmetic_chdb_test.go) already proves the four-combination keep/
// drop routing and a trivial same-layout "hh" merge; this file's fixture
// gives the two histogram operands DIFFERENT Scale AND different
// PositiveOffset, so the assertions below only pass if the merge actually
// downscales the finer operand's bucket indices onto the coarser one's
// scale before summing — a flat per-row JOIN projection with no
// reconciliation (the shape #2521/#2542 deliberately scoped this piece out
// of) would either error or silently misplace the finer side's counts.
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	mvvMergeLHSHistMetric = "mvv_merge_lhs_exp_hist"
	mvvMergeRHSHistMetric = "mvv_merge_rhs_exp_hist"
)

// mvvMergeSeed seeds exactly one series ("layout") on each side's
// histogram arm, deliberately at DIFFERENT Scale (0 vs 1 — RHS is one
// scale step FINER, base 2^(2^-1) vs LHS's base 2) and DIFFERENT
// PositiveOffset (1 vs 2), so the merged bucket ladder can only line up
// correctly if the lowering downscales RHS's absolute bucket indices onto
// LHS's coarser scale first. Neither side's float (histogram_quantile)
// arm has any row for "layout", so the mixed `or`'s shadow rule always
// resolves both operands histogram-valued for this series — the "hh"
// combination [mixedVVHistMergeInputProjections] / [mixedVVHistMergeOutputProjections]
// answer.
var mvvMergeSeed = "" +
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
	// LHS: Scale=0 (coarser), PositiveOffset=1, buckets=[10, 20] ->
	// absolute scale-0 bucket index 1 = 10, index 2 = 20.
	"    ('" + mvvMergeLHSHistMetric + "', map('series', 'layout'), toDateTime64('2026-01-01 00:00:00', 9), 30, 100.0, 0, 3, 1, [10, 20], 0, []),\n" +
	// RHS: Scale=1 (finer), PositiveOffset=2, buckets=[5, 7, 9] ->
	// absolute scale-1 bucket index 2 = 5, index 3 = 7, index 4 = 9.
	// Downscaled to scale 0 (index >> 1): index 2,3 -> 1 (5+7=12);
	// index 4 -> 2 (9).
	"    ('" + mvvMergeRHSHistMetric + "', map('series', 'layout'), toDateTime64('2026-01-01 00:00:00', 9), 21, 50.0, 1, 4, 2, [5, 7, 9], 0, []);\n"

var mvvMergeEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func mvvMergeLHSExpr() string {
	return `(` + mvvMergeLHSHistMetric + ` or histogram_quantile(0.5, mvv_merge_lhs_base_exp_hist))`
}

func mvvMergeRHSExpr() string {
	return `(` + mvvMergeRHSHistMetric + ` or histogram_quantile(0.5, mvv_merge_rhs_base_exp_hist))`
}

// mvvMergeRow is one decoded merged-histogram output row.
type mvvMergeRow struct {
	disc                   int
	cnt, sum               float64
	scale                  int64
	posOffset              int64
	posBucket1, posBucket2 float64
	negOffset              int64
	negBucketsLen          int64
}

func mvvMergeRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) mvvMergeRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvvMergeEvalTS, mvvMergeEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`_setop_is_histogram` AS disc, `HistogramCount` AS cnt, `HistogramSum` AS sum, " +
		"`HistogramScale` AS scale, " +
		"`HistogramPositiveOffset` AS pos_offset, " +
		"`HistogramPositiveBucketCounts`[1] AS pos_b1, `HistogramPositiveBucketCounts`[2] AS pos_b2, " +
		"`HistogramNegativeOffset` AS neg_offset, length(`HistogramNegativeBucketCounts`) AS neg_len"

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		t.Fatalf("query(%q): no rows, want exactly one merged row for series 'layout'", query)
	}
	var r mvvMergeRow
	if err := rows.Scan(&r.disc, &r.cnt, &r.sum, &r.scale, &r.posOffset, &r.posBucket1, &r.posBucket2, &r.negOffset, &r.negBucketsLen); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows.Next() {
		t.Fatalf("query(%q): got more than one row, want exactly one", query)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return r
}

// TestVectorVectorAdditiveArithmeticHistHistDifferentLayout_ChDB proves the
// histogram,histogram `+`/`-` merge over two mixed-`or` operands correctly
// reconciles two DIFFERENT bucket layouts (different Scale AND different
// PositiveOffset), not merely two operands that already happen to share
// one.
func TestVectorVectorAdditiveArithmeticHistHistDifferentLayout_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, mvvMergeSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	t.Run("+", func(t *testing.T) {
		r := mvvMergeRunQuery(t, fixture, s, p, mvvMergeLHSExpr()+" + "+mvvMergeRHSExpr())
		if r.disc != 1 {
			t.Fatalf("disc = %d, want 1 (histogram-shaped)", r.disc)
		}
		if got, want := r.cnt, 30.0+21.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramCount = %v, want %v", got, want)
		}
		if got, want := r.sum, 100.0+50.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramSum = %v, want %v", got, want)
		}
		if got, want := r.scale, int64(0); got != want {
			t.Errorf("HistogramScale = %d, want %d (min(0,1), the coarser operand's scale)", got, want)
		}
		if got, want := r.posOffset, int64(1); got != want {
			t.Errorf("HistogramPositiveOffset = %d, want %d", got, want)
		}
		// index1 = LHS[1]=10 + RHS downscaled (idx2,idx3 -> idx1)=5+7=12 -> 22.
		if got, want := r.posBucket1, 22.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}
		// index2 = LHS[2]=20 + RHS downscaled (idx4 -> idx2)=9 -> 29.
		if got, want := r.posBucket2, 29.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[2] = %v, want %v", got, want)
		}
		if got, want := r.negOffset, int64(0); got != want {
			t.Errorf("HistogramNegativeOffset = %d, want %d", got, want)
		}
		if got, want := r.negBucketsLen, int64(0); got != want {
			t.Errorf("len(HistogramNegativeBucketCounts) = %d, want %d", got, want)
		}
	})

	t.Run("-", func(t *testing.T) {
		r := mvvMergeRunQuery(t, fixture, s, p, mvvMergeLHSExpr()+" - "+mvvMergeRHSExpr())
		if got, want := r.cnt, 30.0-21.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramCount = %v, want %v", got, want)
		}
		if got, want := r.sum, 100.0-50.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramSum = %v, want %v", got, want)
		}
		if got, want := r.scale, int64(0); got != want {
			t.Errorf("HistogramScale = %d, want %d", got, want)
		}
		if got, want := r.posOffset, int64(1); got != want {
			t.Errorf("HistogramPositiveOffset = %d, want %d", got, want)
		}
		// index1 = LHS[1]=10 - RHS downscaled(idx2,idx3->idx1)=5+7=12 -> -2.
		if got, want := r.posBucket1, -2.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}
		// index2 = LHS[2]=20 - RHS downscaled(idx4->idx2)=9 -> 11.
		if got, want := r.posBucket2, 11.0; math.Abs(got-want) > 1e-9 {
			t.Errorf("HistogramPositiveBucketCounts[2] = %v, want %v", got, want)
		}
	})
}
