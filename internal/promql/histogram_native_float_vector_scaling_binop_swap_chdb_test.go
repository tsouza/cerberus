//go:build chdb

// chDB-backed proof for cerberus issue #2537: the two shapes
// expHistogramFloatVectorScalingBinop used to reject because the
// histogram operand could not play the join's Left role now execute
// correctly against real ClickHouse data, not merely lower without
// erroring.
//
//   - CardOneToOne with an on() reduced key and the histogram operand on
//     the syntactic RHS: `histogram_quantile(0.5, hist) * on(job) hist`
//     — the exact shape TestLower_ExpHistogram_
//     FloatVectorScalingBinopStillRejected used to pin as a permanent
//     rejection. [chplan.HistogramFloatVectorJoin]'s own header doc
//     argues this needs no Left/Right role bit because the join's
//     ON-clause equality already forces both sides' reduced Attributes
//     to be identical — this test confirms that argument holds at real
//     execution, not just on paper.
//   - group_right() with the histogram operand on the syntactic LHS: the
//     histogram then plays the "one" role, broadcasting a SINGLE matched
//     histogram row across every one of several matching float rows —
//     chplan.CardOneToMany, a genuinely new join shape #2537 adds (the
//     pre-#2537 emitter only supported the histogram as CardOneToOne or
//     CardManyToOne "many"). The seed deliberately matches the single
//     histogram row against TWO float rows so a broken broadcast (e.g.
//     one that silently drops to one output row, or joins the wrong
//     histogram row) is visible as a missing/wrong row rather than a
//     coincidentally-correct single-row answer.
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

// swapHistMetric is the bare exp-histogram selector both scenarios below
// scale; swapFloatMetric is an UNRELATED plain gauge metric standing in
// for the genuine per-series float-vector operand, mirroring the
// hist/gauge pairing test/spec/promql's own exp_histogram_float_vector_
// scaling_*.txtar fixtures use.
const (
	swapHistMetric  = "swap_hist_side_exp_hist"
	swapFloatMetric = "swap_float_side_gauge"
)

var swapEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// swapOneToOneSeed backs the CardOneToOne/on()/histogram-on-RHS
// scenario: one histogram row and one float row sharing job="api",
// distinguished by an extra label ("region"/"zone") outside the on(job)
// match key so a reduction bug that leaked the WRONG side's extra label
// into the output would be visible rather than accidentally passing.
var swapOneToOneSeed = "" +
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
	"    ('" + swapHistMetric + "', map('job', 'api', 'region', 'hist-side'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + swapFloatMetric + "', map('job', 'api', 'zone', 'float-side'), toDateTime64('2026-01-01 00:00:00', 9), 2.0);\n"

// swapGaugeSeedDDL declares otel_metrics_gauge (this file's own float
// operand table) AND the empty otel_metrics_sum sibling the read path's
// `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')`
// fan-out scans regardless of which one a query actually seeds —
// mirrors limit_ratio_chdb_test.go's identical pairing.
const swapGaugeSeedDDL = "" +
	"CREATE OR REPLACE TABLE otel_metrics_gauge (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"CREATE OR REPLACE TABLE otel_metrics_sum (`MetricName` String, `Attributes` Map(String, String), `ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', `TimeUnix` DateTime64(9), `Value` Float64) ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// TestFloatVectorScalingBinop_HistOnRHS_ChDB proves
// `histogram_quantile(0.5, hist) * on(job) hist` — hist on the syntactic
// RHS, CardOneToOne, reduced on() key — scales the histogram correctly
// and reduces the output Attributes to exactly the on() key.
func TestFloatVectorScalingBinop_HistOnRHS_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, swapOneToOneSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// Hist on the syntactic RHS, float on the LHS — the exact operand
	// order the pre-#2537 recognizer rejected for a reduced-key
	// CardOneToOne match.
	query := swapFloatMetric + " * on(job) " + swapHistMetric

	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, swapEvalTS, swapEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	join := findHistogramFloatVectorJoin(plan)
	if join == nil {
		t.Fatalf("LowerAt(%q): plan does not root in a *chplan.HistogramFloatVectorJoin", query)
	}
	if join.Card != chplan.CardOneToOne {
		t.Fatalf("LowerAt(%q): join.Card = %v, want CardOneToOne", query, join.Card)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`Attributes`['job'] AS job, `Attributes`['region'] AS region, `Attributes`['zone'] AS zone, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		n++
		var job, region, zone string
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&job, &region, &zone, &cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if job != "api" {
			t.Errorf("job = %q, want %q", job, "api")
		}
		// on(job) keeps ONLY the on() labels: both the hist-side
		// ("region") and float-side ("zone") extra labels must be
		// dropped from the output regardless of which side the
		// reduction actually ran over — this is the exact claim
		// [chplan.HistogramFloatVectorJoin]'s own doc proves.
		if region != "" {
			t.Errorf("region = %q, want empty (on(job) must drop it)", region)
		}
		if zone != "" {
			t.Errorf("zone = %q, want empty (on(job) must drop it)", zone)
		}
		const wantCount, wantSum, wantBucket1 = 6, 12.0, 18 // hist(Count=3,Sum=6,bucket=[9]) * float(Value=2)
		if cnt != wantCount {
			t.Errorf("HistogramCount = %v, want %v", cnt, wantCount)
		}
		if sum != wantSum {
			t.Errorf("HistogramSum = %v, want %v", sum, wantSum)
		}
		if bucket1 != wantBucket1 {
			t.Errorf("HistogramPositiveBucketCounts[1] = %v, want %v", bucket1, wantBucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want exactly 1", n)
	}
}

// swapBroadcastHistMetric/swapBroadcastFloatMetric back the
// group_right()/histogram-as-"one" broadcast scenario, kept distinct
// from swapHistMetric/swapFloatMetric above so the two scenarios' seeds
// never collide in the process-shared chDB session (see fixture_chdb_
// test.go's own package doc).
const (
	swapBroadcastHistMetric  = "swap_broadcast_hist_exp_hist"
	swapBroadcastFloatMetric = "swap_broadcast_float_gauge"
)

// swapBroadcastSeed backs the group_right()/CardOneToMany scenario: ONE
// histogram row (job="api") and TWO float rows sharing job="api" but
// distinguished by "zone" — Include(region) then copies the histogram
// row's own "region" label onto BOTH output rows, proving the single
// matched histogram genuinely broadcasts rather than the join silently
// collapsing to one row (a broken broadcast drops one of "z1"/"z2", or
// answers the wrong Value multiplier for one of them).
var swapBroadcastSeed = "" +
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
	"    ('" + swapBroadcastHistMetric + "', map('job', 'api', 'region', 'eu'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [9], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + swapBroadcastFloatMetric + "', map('job', 'api', 'zone', 'z1'), toDateTime64('2026-01-01 00:00:00', 9), 2.0),\n" +
	"    ('" + swapBroadcastFloatMetric + "', map('job', 'api', 'zone', 'z2'), toDateTime64('2026-01-01 00:00:00', 9), 5.0);\n"

// TestFloatVectorScalingBinop_GroupRightHistOne_ChDB proves
// `hist * on(job) group_right(region) float` — histogram on the
// syntactic LHS, group_right() keeping the RHS (float) "many" and the
// histogram "one" — broadcasts the single matched histogram row's SAME
// bucket layout across every matching float row, scaled by that row's
// own Value, with Include(region) carrying the histogram's own "region"
// label onto every broadcast output row.
func TestFloatVectorScalingBinop_GroupRightHistOne_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, swapBroadcastSeed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := swapBroadcastHistMetric + " * on(job) group_right(region) " + swapBroadcastFloatMetric
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, swapEvalTS, swapEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	join := findHistogramFloatVectorJoin(plan)
	if join == nil {
		t.Fatalf("LowerAt(%q): plan does not root in a *chplan.HistogramFloatVectorJoin", query)
	}
	if join.Card != chplan.CardOneToMany {
		t.Fatalf("LowerAt(%q): join.Card = %v, want CardOneToMany", query, join.Card)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	projection := "`Attributes`['job'] AS job, `Attributes`['zone'] AS zone, `Attributes`['region'] AS region, " +
		"`HistogramCount` AS cnt, `HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	wantByZone := map[string]struct{ cnt, sum, bucket1 float64 }{
		"z1": {6, 12.0, 18},  // hist(3,6.0,[9]) * float(Value=2)
		"z2": {15, 30.0, 45}, // hist(3,6.0,[9]) * float(Value=5)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var job, zone, region string
		var cnt, sum, bucket1 float64
		if err := rows.Scan(&job, &zone, &region, &cnt, &sum, &bucket1); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if job != "api" {
			t.Errorf("job = %q, want %q", job, "api")
		}
		if region != "eu" {
			t.Errorf("zone %s: region = %q, want %q (Include must carry the histogram's own label onto the broadcast row)", zone, region, "eu")
		}
		want, ok := wantByZone[zone]
		if !ok {
			t.Fatalf("unexpected zone %q", zone)
		}
		seen[zone] = true
		if math.Abs(cnt-want.cnt) > 1e-9 {
			t.Errorf("zone %s: HistogramCount = %v, want %v", zone, cnt, want.cnt)
		}
		if math.Abs(sum-want.sum) > 1e-9 {
			t.Errorf("zone %s: HistogramSum = %v, want %v", zone, sum, want.sum)
		}
		if math.Abs(bucket1-want.bucket1) > 1e-9 {
			t.Errorf("zone %s: HistogramPositiveBucketCounts[1] = %v, want %v", zone, bucket1, want.bucket1)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(seen) != len(wantByZone) {
		t.Fatalf("got rows for zones %v, want both %v present (a broken broadcast drops one matching float row)", seen, wantByZone)
	}
}
