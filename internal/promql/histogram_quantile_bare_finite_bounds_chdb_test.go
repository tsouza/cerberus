//go:build chdb

// chDB-backed proof that lowerHistogramQuantileClassicBare
// (histogram_quantile.go, the `histogram_quantile(phi, <bare classic-
// histogram selector>)` lowering, no `le` matcher) filters a literal
// non-finite storage bound the same way the aggregated/merge path
// already does (#2495) — a pre-release audit finding: before the fix,
// this path had NO finite-filtering at all, so a malformed row's own
// non-finite ExplicitBounds entry reached HistogramQuantile's own
// bounds[1]<=0 special case verbatim and could answer -Inf directly as
// "the quantile", rather than being excluded from the interpolation the
// way #2495 already excludes it from the aggregated path.
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

// hqBareFiniteBoundsMetric is the BARE metric name storage keys under —
// see hqFloatPairedBoundsMetric's identical note.
const hqBareFiniteBoundsMetric = "hq_bare_finite_bounds_probe"

// hqBareFiniteBoundsSeed seeds ONE classic-histogram row whose storage
// ExplicitBounds carries a malformed leading "-inf" entry ahead of the
// three genuine finite bounds [1, 5, 10], with per-bucket (NON-
// cumulative — this path's storage format) BucketCounts = [100, 2, 3, 4, 1]:
//
//	bound -inf:  100 observations (malformed — huge, so a leak is obvious)
//	bound    1:    2 observations
//	bound    5:    3 observations
//	bound   10:    4 observations
//	+Inf overflow: 1 observation
var hqBareFiniteBoundsSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_histogram (" +
	"MetricName String, Attributes Map(String, String), " +
	"ResourceAttributes Map(String, String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', " +
	"TimeUnix DateTime64(9), BucketCounts Array(UInt64), ExplicitBounds Array(Float64), " +
	"AggregationTemporality Int32 DEFAULT 2" +
	") ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);\n" +
	"INSERT INTO otel_metrics_histogram (MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES\n" +
	"    ('" + hqBareFiniteBoundsMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), " +
	"[100, 2, 3, 4, 1], [-inf, 1.0, 5.0, 10.0]);\n"

var hqBareFiniteBoundsEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestHistogramQuantileClassicBare_ChDB_FiniteBoundsFiltered pins the
// finding. Post-fix, ExplicitBounds narrows to [1,5,10] and BucketCounts
// to the paired [2,3,4,1] (cumulative [2,5,9,10], total 10); at phi=0.35
// (rank=3.5) the target bucket is (1,5] with a cumulative delta of 3
// starting from 2, so the answer interpolates to
// 1 + (5-1)*((3.5-2)/3) = 1 + 4*(1.5/3) = 3.0.
//
// Pre-fix (verified: reverting the fix reproduces it), the raw storage
// bounds [-inf,1,5,10] / cumulative [100,102,105,109,110] (total 110,
// rank=38.5) land the target bucket at index 1 (cum=100 already >=
// 38.5), and HistogramQuantile's own `bounds[1] <= 0` special case
// answers bounds[1] = -Inf directly — the malformed bound propagating
// straight through as "the quantile".
func TestHistogramQuantileClassicBare_ChDB_FiniteBoundsFiltered(t *testing.T) {
	fixture := newChDBFixture(t, hqBareFiniteBoundsSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	const query = "histogram_quantile(0.35, " + hqBareFiniteBoundsMetric + "_bucket)"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, hqBareFiniteBoundsEvalTS, hqBareFiniteBoundsEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := fixture.queryOverEmitted(t, "Value AS v", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		t.Fatal("query returned no rows, want exactly one interpolated quantile")
	}
	var got float64
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("scan Value: %v", err)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if math.IsInf(got, -1) {
		t.Fatal("query answered -Inf — the malformed storage bound leaked straight through as the quantile (the pre-fix bug this test pins)")
	}

	const want = 3.0 // see doc comment above for the derivation
	const tolerance = 1e-6
	if math.Abs(got-want) > tolerance {
		t.Fatalf("Value = %v, want %v (±%v)", got, want, tolerance)
	}
}
