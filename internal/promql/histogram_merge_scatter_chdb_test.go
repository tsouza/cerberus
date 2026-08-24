//go:build chdb

// chDB-backed correctness proof for cerberus issue #2500: the across-series
// exponential-histogram merge (expHistogramMergeBucketsExpr) now computes
// each row's contribution to a merged target bucket via a directly-computed
// arraySlice instead of rescanning that row's entire bucket array
// (expHistogramBucketPositionPickerExpr, internal/promql/histogram_quantile.go).
//
// The seed below deliberately mixes THREE series with heterogeneous bucket
// layouts — different Scale (0, 2, 1), different PositiveOffset/
// NegativeOffset, one row whose scale forces a downscale COLLAPSE (several
// of its own buckets folding onto one merged bucket) — so both the
// no-collapse (ratio=1) and collapse (ratio>1) code paths in the rewritten
// picker are exercised in the same query. Expected PositiveBucketCounts /
// NegativeBucketCounts below were hand-computed AND independently verified
// against the pre-rewrite picker formula run as raw SQL (not through
// chplan) on the identical inputs — see the issue for the derivation.
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

const histogramMergeScatterMetric = "scatter_merge_latency_exp_hist"

var histogramMergeScatterEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestHistogramMergeScatter_ChDB_HeterogeneousLayoutsMatchHandComputed seeds
// three series whose Scale/Offset genuinely differ (mergedScale = min = 0,
// so the Scale-2 and Scale-1 rows both downscale) and asserts the merged
// PositiveBucketCounts / PositiveOffset and NegativeBucketCounts /
// NegativeOffset the rewritten picker produces match values computed by
// hand (and cross-checked against the pre-rewrite rescan formula run as raw
// SQL — see the file header).
//
// Row layout (all Scale values share ONE mergedScale = 0):
//
//	series  Scale  PosOffset  PosBuckets        NegOffset  NegBuckets
//	s1      0      5          [1,2,3,4]         -8         [5,6,7]
//	s2      2      17         [10,20,30,40,     -3         [1,2]
//	                           50,60,70]
//	s3      1      9          [100,200,300]     -5         [9,10,11]
//
// Positive side (mergedStart=4, mergedLength=5): s2's Scale-2 row
// downscales by ratio 4, folding its first 3 buckets (10+20+30) onto
// target 4 and its last 4 (40+50+60+70) onto target 5 — the COLLAPSE case;
// s3's Scale-1 row downscales by ratio 2 similarly. s1 needs no
// downscaling (ratio 1). Expected merged PositiveBucketCounts, absolute
// targets 4..8:
//
//	target 4: s2's first chunk (60) + s3's first chunk (100)      = 160
//	target 5: s1[5]=1 + s2's second chunk (220) + s3's second (500) = 721
//	target 6: s1[6]=2                                              = 2
//	target 7: s1[7]=3                                              = 3
//	target 8: s1[8]=4                                              = 4
//
// Negative side (mergedStart=-8, mergedLength=8), same three rows'
// NEGATIVE ladders (s2's ratio-4 downscale collapses its own 2 buckets
// onto ONE target; s3's ratio-2 downscale splits 3 buckets across two
// targets):
//
//	target -8: s1[1]=5   target -7: s1[2]=6   target -6: s1[3]=7
//	target -5: 0         target -4: 0
//	target -3: s3's first chunk (9)
//	target -2: s3's second chunk (10+11=21)
//	target -1: s2's only chunk (1+2=3)
func TestHistogramMergeScatter_ChDB_HeterogeneousLayoutsMatchHandComputed(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	rows := []string{
		fmt.Sprintf("('%s', map('series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 22, 1.0, 0, 0, 5, [1,2,3,4], -8, [5,6,7])", histogramMergeScatterMetric),
		fmt.Sprintf("('%s', map('series', 's2'), toDateTime64('2026-01-01 00:00:00', 9), 283, 1.0, 2, 0, 17, [10,20,30,40,50,60,70], -3, [1,2])", histogramMergeScatterMetric),
		fmt.Sprintf("('%s', map('series', 's3'), toDateTime64('2026-01-01 00:00:00', 9), 630, 1.0, 1, 0, 9, [100,200,300], -5, [9,10,11])", histogramMergeScatterMetric),
	}
	b.WriteString("    " + strings.Join(rows, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf("sum(%s)", histogramMergeScatterMetric)
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, histogramMergeScatterEvalTS, histogramMergeScatterEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows2 := fixture.queryOverEmitted(t, "toString(HistogramPositiveBucketCounts), HistogramPositiveOffset, toString(HistogramNegativeBucketCounts), HistogramNegativeOffset", sqlStr, args)
	defer func() { _ = rows2.Close() }()

	if !rows2.Next() {
		if err := rows2.Err(); err != nil {
			t.Fatalf("query error: %v", err)
		}
		t.Fatal("expected exactly one merged row, got none")
	}
	var posBuckets, negBuckets string
	var posOffset, negOffset int64
	if err := rows2.Scan(&posBuckets, &posOffset, &negBuckets, &negOffset); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows2.Next() {
		t.Fatal("expected exactly one merged row, got more than one")
	}

	const wantPos = "[160,721,2,3,4]"
	const wantPosOffset = int64(4)
	const wantNeg = "[5,6,7,0,0,9,21,3]"
	const wantNegOffset = int64(-8)

	if posBuckets != wantPos || posOffset != wantPosOffset {
		t.Errorf("PositiveBucketCounts=%s PositiveOffset=%d, want %s offset %d", posBuckets, posOffset, wantPos, wantPosOffset)
	}
	if negBuckets != wantNeg || negOffset != wantNegOffset {
		t.Errorf("NegativeBucketCounts=%s NegativeOffset=%d, want %s offset %d", negBuckets, negOffset, wantNeg, wantNegOffset)
	}
}
