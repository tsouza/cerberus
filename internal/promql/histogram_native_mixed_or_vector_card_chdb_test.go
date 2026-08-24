//go:build chdb

// chDB-backed proof that group_left()/group_right() (many-to-one vector
// matching cardinality) over TWO independently mixed float/histogram `or`
// operands (cerberus issue #2449's ninth wrapper family,
// histogram_native_mixed_or_vector_arithmetic.go /
// histogram_native_mixed_or_vector_comparison.go's own Card support)
// broadcasts the "one" side's single collapsed row against each of the
// "many" side's own rows with the correct per-row VALUE (including a
// histogram-valued "many" row, exercising that the per-row four-
// combination fold composes unchanged under cardinality — see those
// files' own header docs) AND the correct output ATTRIBUTES (the "many"
// side's own full label set, overlaid with the "one" side's Include
// labels via group_left(<labels>)/group_right(<labels>)) — not merely
// that the emitted plan's Go shape looks right.
//
// The "many" side carries two `pod` values for a single `service`: "p1"
// resolves FLOAT (its histogram arm has no row, shadowed away by the
// mixed-or's own absence rule — the SAME [1,2,3,4]/Count=10/Sum=10.0
// layout histogram_native_mixed_or_vector_arithmetic_chdb_test.go's own
// mvvSeed uses, so "p1"'s float value is the SAME oracle-pinned
// 6.3496042078727974 quantile that file already establishes); "p2"
// resolves HISTOGRAM (Count=2, Sum=4.0, bucket=[6], the same layout that
// file's "hf"/"fh" series use). The "one" side carries a single row per
// `service`, labeled with `region` (a label the "many" side does NOT
// carry, so group_left(region)/group_right(region) is the only way it
// reaches the output), resolving float via the identical [1,2,3,4]
// layout — so its own quantile is the SAME q. group_left(region) and
// group_right(region) are each proven to broadcast that single one-side
// row against BOTH many-side pods with the identical result (MUL is
// symmetric under operand order for both the plain product and the
// histogram scale-fold), and to carry `region` on the output while `pod`
// (the many side's own label) survives unchanged.
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
	mvgManyHistMetric = "mvg_many_hist_exp_hist"
	mvgManyBaseMetric = "mvg_many_base_exp_hist"
	mvgOneHistMetric  = "mvg_one_hist_exp_hist" // declared, never seeded — always shadow-absent.
	mvgOneBaseMetric  = "mvg_one_base_exp_hist"
)

// mvgQuantile is the histogram_quantile(0.5, ...) answer for the
// [1,2,3,4]/Count=10/Sum=10.0 bucket layout every float arm below seeds —
// the same oracle-pinned value
// histogram_native_mixed_or_vector_arithmetic_chdb_test.go's own
// mvvQuantileBaseline establishes, reused rather than re-derived.
const mvgQuantile = mvvQuantileBaseline

var mvgSeed = "" +
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
	// Many side, pod "p1": no histogram row -> resolves float via the base
	// (quantile) metric.
	"    ('" + mvgManyBaseMetric + "', map('service', 'svc', 'pod', 'p1'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []),\n" +
	// Many side, pod "p2": histogram row present -> resolves histogram,
	// shadowing its own (unseeded, for this pod) base arm.
	"    ('" + mvgManyHistMetric + "', map('service', 'svc', 'pod', 'p2'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	// One side: a single row per service, labeled with `region` (a label
	// the many side never carries) instead of `pod`.
	"    ('" + mvgOneBaseMetric + "', map('service', 'svc', 'region', 'us'), toDateTime64('2026-01-01 00:00:00', 9), 10, 10.0, 0, 0, 0, [1, 2, 3, 4], 0, []);\n"

var mvgEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

func mvgManyExpr() string {
	return `(` + mvgManyHistMetric + ` or histogram_quantile(0.5, ` + mvgManyBaseMetric + `))`
}

func mvgOneExpr() string {
	return `(` + mvgOneHistMetric + ` or histogram_quantile(0.5, ` + mvgOneBaseMetric + `))`
}

// mvgRow is one decoded output row: which pod, the overlaid region label,
// whether it is histogram-shaped, and its float Value / three histogram
// probe fields.
type mvgRow struct {
	pod, region string
	disc        int
	val         float64
	cnt, sum    float64
	bucket1     float64
}

func mvgRunQuery(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) map[string]mvgRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, mvgEvalTS, mvgEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	// The `bool`-comparison shape ([lowerMixedVVCompareBool]) projects a
	// plain 4-column Sample ([chplan.SampleRowShape]) with no histogram
	// payload at all, unlike the arithmetic/non-bool-comparison Mixed
	// shape's fourteen columns — mirrors mvvRunQuery/mvcRunQuery's own
	// shape-conditional projection.
	mixed := chplan.RowShapeOf(plan) == chplan.MixedRowShape
	projection := "`Attributes`['pod'] AS pod, `Attributes`['region'] AS region, `Value` AS val"
	if mixed {
		projection = "`Attributes`['pod'] AS pod, `Attributes`['region'] AS region, " +
			"`_setop_is_histogram` AS disc, `Value` AS val, `HistogramCount` AS cnt, " +
			"`HistogramSum` AS sum, `HistogramPositiveBucketCounts`[1] AS bucket1"
	}

	rows := fixture.queryOverEmitted(t, projection, sqlStr, args)
	defer func() { _ = rows.Close() }()

	out := map[string]mvgRow{}
	for rows.Next() {
		var r mvgRow
		if mixed {
			if err := rows.Scan(&r.pod, &r.region, &r.disc, &r.val, &r.cnt, &r.sum, &r.bucket1); err != nil {
				t.Fatalf("scan: %v", err)
			}
		} else {
			if err := rows.Scan(&r.pod, &r.region, &r.val); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}
		out[r.pod] = r
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestVectorVectorArithmeticOverMixedSetOpOr_ChDB_GroupLeftRight proves
// group_left(region) and group_right(region), both over `*`, broadcast
// the one-side's single row against each of the many-side's two pods —
// float,float (p1) and histogram,float (p2) — with the correct scaled
// value and Attributes overlay, for both cardinality directions.
func TestVectorVectorArithmeticOverMixedSetOpOr_ChDB_GroupLeftRight(t *testing.T) {
	fixture := newChDBFixture(t, mvgSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	q := mvgQuantile

	checkRows := func(t *testing.T, rows map[string]mvgRow) {
		t.Helper()
		if len(rows) != 2 {
			t.Fatalf("got %d rows, want 2 (one per many-side pod): %+v", len(rows), rows)
		}
		p1, ok := rows["p1"]
		if !ok {
			t.Fatalf("no row for pod=p1")
		}
		if p1.region != "us" {
			t.Errorf("p1: region = %q, want %q (group_left/right Include overlay)", p1.region, "us")
		}
		if p1.disc != 0 {
			t.Errorf("p1: disc = %d, want 0 (float,float)", p1.disc)
		}
		if got, want := p1.val, q*q; math.Abs(got-want) > 1e-9 {
			t.Errorf("p1: Value = %v, want %v", got, want)
		}

		p2, ok := rows["p2"]
		if !ok {
			t.Fatalf("no row for pod=p2")
		}
		if p2.region != "us" {
			t.Errorf("p2: region = %q, want %q (group_left/right Include overlay)", p2.region, "us")
		}
		if p2.disc != 1 {
			t.Errorf("p2: disc = %d, want 1 (histogram,float keeps for *, scaled by the one side's float)", p2.disc)
		}
		if got, want := p2.cnt, 2*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("p2: HistogramCount = %v, want %v", got, want)
		}
		if got, want := p2.sum, 4*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("p2: HistogramSum = %v, want %v", got, want)
		}
		if got, want := p2.bucket1, 6*q; math.Abs(got-want) > 1e-6 {
			t.Errorf("p2: HistogramPositiveBucketCounts[1] = %v, want %v", got, want)
		}
	}

	t.Run("group_left", func(t *testing.T) {
		query := mvgManyExpr() + ` * on(service) group_left(region) ` + mvgOneExpr()
		checkRows(t, mvgRunQuery(t, fixture, s, p, query))
	})

	t.Run("group_right", func(t *testing.T) {
		// Mirror of group_left: the one side (carrying `region`) now
		// syntactically on the LEFT, the many side (carrying `pod`) on
		// the RIGHT — group_right() names the RIGHT operand as the
		// "many" side.
		query := mvgOneExpr() + ` * on(service) group_right(region) ` + mvgManyExpr()
		checkRows(t, mvgRunQuery(t, fixture, s, p, query))
	})
}

// TestVectorVectorCompareOverMixedSetOpOr_ChDB_GroupLeftRight proves
// group_left(region)/group_right(region) also broadcast correctly for a
// comparison: `==` `bool` keeps the float,float pod (p1, equal values ->
// 1.0) and drops the float,histogram pod (p2, an incompatible-type pair
// per histogram_native_mixed_or_vector_comparison.go's own header) —
// proving the per-row keep/drop decision is unaffected by cardinality
// while the surviving row still carries the correct Include overlay.
func TestVectorVectorCompareOverMixedSetOpOr_ChDB_GroupLeftRight(t *testing.T) {
	fixture := newChDBFixture(t, mvgSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	checkRows := func(t *testing.T, rows map[string]mvgRow) {
		t.Helper()
		if _, ok := rows["p2"]; ok {
			t.Errorf("p2: got a row, want none (float,histogram is an incompatible-type pair, dropped even with bool)")
		}
		p1, ok := rows["p1"]
		if !ok {
			t.Fatalf("no row for pod=p1")
		}
		if p1.region != "us" {
			t.Errorf("p1: region = %q, want %q (group_left/right Include overlay)", p1.region, "us")
		}
		if got, want := p1.val, 1.0; got != want {
			t.Errorf("p1: Value = %v, want %v (equal quantiles)", got, want)
		}
	}

	t.Run("group_left", func(t *testing.T) {
		query := mvgManyExpr() + ` == bool on(service) group_left(region) ` + mvgOneExpr()
		checkRows(t, mvgRunQuery(t, fixture, s, p, query))
	})

	t.Run("group_right", func(t *testing.T) {
		query := mvgOneExpr() + ` == bool on(service) group_right(region) ` + mvgManyExpr()
		checkRows(t, mvgRunQuery(t, fixture, s, p, query))
	})
}
