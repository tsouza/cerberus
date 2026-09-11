//go:build chdb

package promql_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
	"github.com/tsouza/cerberus/test/spec"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// K exceeds every group, making limitk's otherwise arbitrary selection exact.
const mixedSelectorAllK = "10"

func TestComputedSelectorCustomValueColumn_ChDB(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.ValueColumn = "sample_value"
	for _, op := range []string{"topk", "bottomk", "limitk"} {
		for _, step := range []time.Duration{0, time.Second} {
			t.Run(fmt.Sprintf("%s/%s", op, step), func(t *testing.T) {
				assertMixedSelectorParity(t, op+"(scalar(vector("+mixedSelectorAllK+")), "+foFloatMetric+")", false, s, step)
			})
		}
	}
}

func TestMixedSelectorsRetainSampleKinds_ChDB(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "sample_time", "sample_value"
	for _, tc := range []struct {
		name      string
		op        string
		parameter string
		schema    schema.Metrics
		step      time.Duration
		shadow    bool
		histFirst bool
	}{
		{"topk/literal/instant/float_first", "topk", mixedSelectorAllK, standard, 0, false, false},
		{"topk/computed/range/hist_first", "topk", "scalar(vector(" + mixedSelectorAllK + "))", custom, time.Second, false, true},
		{"bottomk/literal/range/hist_first", "bottomk", mixedSelectorAllK, custom, time.Second, false, true},
		{"bottomk/computed/instant/shadow_float_first", "bottomk", "scalar(vector(" + mixedSelectorAllK + "))", standard, 0, true, false},
		{"limitk/literal/range/shadow_hist_first", "limitk", mixedSelectorAllK, standard, time.Second, true, true},
		{"limitk/computed/instant/float_first", "limitk", "scalar(vector(" + mixedSelectorAllK + "))", custom, 0, false, false},
		{"limit_ratio/literal/instant/hist_first", "limit_ratio", "1", custom, 0, false, true},
		{"limit_ratio/computed/range/float_first", "limit_ratio", "scalar(vector(1))", standard, time.Second, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operand := mixedSelectorOperand(tc.shadow, tc.histFirst)
			query := tc.op + "(" + tc.parameter + ", sort_by_label(" + operand + `, "series")) by (bucket)`
			assertMixedSelectorParity(t, query, tc.shadow, tc.schema, tc.step)
		})
	}
}

func mixedSelectorOperand(shadow, histFirst bool) string {
	hist, float := foHistMetric, foFloatMetric
	if shadow {
		hist, float = tkShadowHistMetric, tkShadowFloatMetric
	}
	if histFirst {
		return hist + " or " + float
	}
	return float + " or " + hist
}

func mixedSelectorSeed(shadow bool, s schema.Metrics) (string, []oracle.Series) {
	hist, float, seed := foHistMetric, foFloatMetric, foSeed
	if shadow {
		hist, float, seed = tkShadowHistMetric, tkShadowFloatMetric, tkShadowSeed
	}
	at := foEvalTS.Add(-time.Second).UnixMilli()
	histSeries := func(series, bucket string, count, sum, bucketCount float64) oracle.Series {
		labels := map[string]string{"__name__": hist, "series": series}
		if bucket != "" {
			labels["bucket"] = bucket
		}
		return oracle.Series{Labels: labels, Points: []oracle.Point{{TMillis: at, Histogram: &oracle.Histogram{Count: count, Sum: sum, PositiveBuckets: []float64{bucketCount}}}}}
	}
	floatSeries := func(series, bucket string, value float64) oracle.Series {
		labels := map[string]string{"__name__": float, "series": series}
		if bucket != "" {
			labels["bucket"] = bucket
		}
		return oracle.Series{Labels: labels, Points: []oracle.Point{{TMillis: at, Value: value}}}
	}
	seeded := []oracle.Series{histSeries("h1", "b1", 2, 4, 6), histSeries("h2", "b2", 3, 9, 7), floatSeries("f1", "b1", 3), floatSeries("f2", "b1", 9), floatSeries("f3", "b3", 1)}
	if shadow {
		seeded = []oracle.Series{histSeries("dup", "", 2, 4, 6), floatSeries("dup", "", 42), floatSeries("solo", "", 7)}
	}
	seed = strings.NewReplacer("MetricName", s.MetricNameColumn, "Attributes", s.AttributesColumn, "TimeUnix", s.TimestampColumn, "Value", s.ValueColumn).Replace(seed)
	seed = strings.ReplaceAll(seed, "Resource"+s.AttributesColumn, "ResourceAttributes")
	return seed, seeded
}

func assertMixedSelectorParity(t *testing.T, query string, shadow bool, s schema.Metrics, step time.Duration) {
	t.Helper()
	seed, seeded := mixedSelectorSeed(shadow, s)
	reference, err := oracle.Evaluate(t, seeded, oracle.Query{Expr: query, Start: foEvalTS, End: foEvalTS.Add(step), Step: step})
	if err != nil {
		t.Fatalf("reference %s: %v", query, err)
	}
	want := []string{}
	for _, sample := range reference {
		key := fmt.Sprintf("%s/%s/%s/%d", sample.Labels["__name__"], sample.Labels["series"], sample.Labels["bucket"], sample.TMillis)
		if sample.Histogram != nil {
			h := sample.Histogram
			want = append(want, fmt.Sprintf("%s/hist/%g/%g/%v", key, h.Count, h.Sum, h.PositiveBuckets))
		} else {
			want = append(want, fmt.Sprintf("%s/float/%g", key, sample.Value))
		}
	}
	slices.Sort(want)
	for _, optimized := range []bool{false, true} {
		t.Run(fmt.Sprintf("optimized=%v", optimized), func(t *testing.T) {
			t.Logf("query: %s", query)
			expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, foEvalTS, foEvalTS.Add(step), step)
			if err != nil {
				t.Fatal(err)
			}
			if optimized {
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
			}
			got := mixedSelectorRows(t, plan, s, seed, step)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("query %s: got %v, reference %v", query, got, want)
			}
		})
	}
}

func mixedSelectorRows(t *testing.T, plan chplan.Node, s schema.Metrics, seed string, step time.Duration) []string {
	t.Helper()
	col := func(name string) chplan.Expr { return &chplan.ColumnRef{Name: name} }
	str := func(expr chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{expr}}
	}
	var kind, count, sum, buckets chplan.Expr = &chplan.LitInt{}, &chplan.LitFloat{}, &chplan.LitFloat{}, &chplan.LitString{V: "[]"}
	if plan.RowType().HasHistogramPayload() {
		kind = &chplan.LitInt{V: 1}
		if plan.RowType().Has(chplan.RoleDiscriminator) {
			kind = col(chplan.MixedDiscriminatorColumn)
		}
		count, sum, buckets = col(chplan.HistogramCountColumn), col(chplan.HistogramSumColumn), str(col(chplan.HistogramPositiveBucketCountsColumn))
	}
	projection := &chplan.Project{Input: plan, Projections: []chplan.Projection{
		{Expr: col(s.MetricNameColumn), Alias: "metric"},
		{Expr: &chplan.MapAccess{Map: col(s.AttributesColumn), Key: &chplan.LitString{V: "series"}}, Alias: "series"},
		{Expr: &chplan.MapAccess{Map: col(s.AttributesColumn), Key: &chplan.LitString{V: "bucket"}}, Alias: "bucket"},
		{Expr: str(col(s.TimestampColumn)), Alias: "stamp"},
		{Expr: col(s.ValueColumn), Alias: "value"},
		{Expr: kind, Alias: "kind"},
		{Expr: count, Alias: "count"},
		{Expr: sum, Alias: "sum"},
		{Expr: buckets, Alias: "buckets"},
	}}
	sql, args, err := chsql.Emit(context.Background(), projection)
	if err != nil {
		t.Fatal(err)
	}
	db := spec.OpenChDB(t)
	spec.ApplySeed(t, db, seed)
	sql = testsql.ExpandStarProjection(sql, testsql.SeedTableColumns(seed))
	sql = testsql.NestMapWhere(testsql.NestMapOrderBy(testsql.RewriteMapProjections(sql)))
	rows, err := db.Query(sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := []string{}
	for rows.Next() {
		var metric, series, bucket, stamp, positiveBuckets string
		var value, count, sum float64
		var kind int
		if err := rows.Scan(&metric, &series, &bucket, &stamp, &value, &kind, &count, &sum, &positiveBuckets); err != nil {
			t.Fatal(err)
		}
		at := foEvalTS
		if step > 0 {
			at, err = time.Parse("2006-01-02 15:04:05.999999999", stamp)
			if err != nil {
				t.Fatal(err)
			}
		}
		key := fmt.Sprintf("%s/%s/%s/%d", metric, series, bucket, at.UnixMilli())
		if kind == 1 {
			got = append(got, fmt.Sprintf("%s/hist/%g/%g/%s", key, count, sum, positiveBuckets))
		} else {
			got = append(got, fmt.Sprintf("%s/float/%g", key, value))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}
