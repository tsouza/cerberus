//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
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
	"github.com/tsouza/cerberus/test/spec"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

func TestCountValuesNestedMixedSerialization_ChDB(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn = "metric_id", "label_map"
	custom.TimestampColumn, custom.ValueColumn = "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, shadow := range []bool{false, true} {
			for _, histFirst := range []bool{false, true} {
				for _, step := range []time.Duration{0, time.Second} {
					for _, grouping := range []string{"", " by (series)"} {
						t.Run(fmt.Sprintf("%s/shadow=%v/histFirst=%v/%s/%s", s.ValueColumn, shadow, histFirst, step, grouping), func(t *testing.T) {
							seed, series, histMetric, floatMetric := countValuesMixedSeed(shadow)
							left, right := floatMetric, histMetric
							if histFirst {
								left, right = right, left
							}
							operand := left + " or " + right
							query := `count_values("v", sort_by_label(` + operand + `, "series"))` + grouping
							assertCountValuesMixedParity(t, query, seed, series, s, step)
							if s.ValueColumn == standard.ValueColumn && !shadow && step == 0 && grouping == "" {
								assertCountValuesMixedParity(t, `count_values("v", `+operand+`)`, seed, series, s, step)
								assertCountValuesMixedParity(t, `abs(`+query+`)`, seed, series, s, step)
								emptyOperand := left + `{series="missing"} or ` + right + `{series="missing"}`
								assertCountValuesMixedParity(t, `count_values("v", sort_by_label(`+emptyOperand+`, "series"))`, seed, series, s, step)
								if !histFirst {
									assertCountValuesMixedParity(t, query+" without (series)", seed, series, s, step)
								}
							}
						})
					}
				}
			}
		}
	}
}

func countValuesMixedSeed(shadow bool) (string, []oracle.Series, string, string) {
	at := cvMixedEvalTS.Add(-time.Second).UnixMilli()
	floatSeries := func(metric, name string, value float64) oracle.Series {
		return oracle.Series{Labels: map[string]string{"__name__": metric, "series": name}, Points: []oracle.Point{{TMillis: at, Value: value}}}
	}
	histSeries := func(metric, name string, count, sum, bucketCount float64) oracle.Series {
		return oracle.Series{Labels: map[string]string{"__name__": metric, "series": name}, Points: []oracle.Point{{TMillis: at, Histogram: &oracle.Histogram{Count: count, Sum: sum, PositiveBuckets: []float64{bucketCount}}}}}
	}
	if shadow {
		return tkShadowSeed, []oracle.Series{
			histSeries(tkShadowHistMetric, "dup", 2, 4, 6),
			floatSeries(tkShadowFloatMetric, "dup", 42),
			floatSeries(tkShadowFloatMetric, "solo", 7),
		}, tkShadowHistMetric, tkShadowFloatMetric
	}
	// A real zero must remain distinct from the histogram's placeholder Value.
	seed := strings.Replace(cvMixedSeed, "9.0);", "0.0);", 1)
	return seed, []oracle.Series{
		histSeries(cvMixedHistMetric, "h1", 5, 10, 5),
		floatSeries(cvMixedFloatMetric, "f1", 3),
		floatSeries(cvMixedFloatMetric, "f2", 3),
		floatSeries(cvMixedFloatMetric, "f3", 0),
	}, cvMixedHistMetric, cvMixedFloatMetric
}

func assertCountValuesMixedParity(t *testing.T, query, seed string, series []oracle.Series, s schema.Metrics, step time.Duration) {
	t.Helper()
	reference, err := oracle.Evaluate(t, series, oracle.Query{Expr: query, Start: cvMixedEvalTS, End: cvMixedEvalTS.Add(step), Step: step})
	if err != nil {
		t.Fatal(err)
	}
	record := func(labels map[string]string, stamp int64, value float64) string {
		encoded, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%d/%g", encoded, stamp, value)
	}
	want := []string{}
	for _, sample := range reference {
		if sample.Histogram != nil {
			t.Fatal("count_values reference returned a histogram")
		}
		want = append(want, record(sample.Labels, sample.TMillis, sample.Value))
	}
	slices.Sort(want)
	seed = strings.NewReplacer("MetricName", s.MetricNameColumn, "Attributes", s.AttributesColumn, "TimeUnix", s.TimestampColumn, "Value", s.ValueColumn).Replace(seed)
	seed = strings.ReplaceAll(seed, "Resource"+s.AttributesColumn, "ResourceAttributes")
	for _, optimized := range []bool{false, true} {
		t.Run(fmt.Sprintf("optimized=%v", optimized), func(t *testing.T) {
			expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, cvMixedEvalTS, cvMixedEvalTS.Add(step), step)
			if err != nil {
				t.Fatal(err)
			}
			if optimized {
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
			}
			col := func(name string) chplan.Expr { return &chplan.ColumnRef{Name: name} }
			projection := &chplan.Project{Input: plan, Projections: []chplan.Projection{
				{Expr: &chplan.FuncCall{Fn: chplan.FnToJSONString, Args: []chplan.Expr{col(s.AttributesColumn)}}, Alias: "labels"},
				{Expr: &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{col(s.TimestampColumn)}}, Alias: "stamp"},
				{Expr: col(s.ValueColumn), Alias: "count"},
			}}
			sql, args, err := chsql.Emit(context.Background(), projection)
			if err != nil {
				t.Fatal(err)
			}
			db := spec.OpenChDB(t)
			spec.ApplySeed(t, db, seed)
			rows, err := db.Query(sql, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			got := []string{}
			for rows.Next() {
				var labelsJSON, stamp string
				var value float64
				if err := rows.Scan(&labelsJSON, &stamp, &value); err != nil {
					t.Fatal(err)
				}
				labels := map[string]string{}
				if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
					t.Fatal(err)
				}
				at := cvMixedEvalTS
				if step > 0 {
					at, err = time.Parse("2006-01-02 15:04:05.999999999", stamp)
					if err != nil {
						t.Fatal(err)
					}
				}
				got = append(got, record(labels, at.UnixMilli(), value))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			slices.Sort(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: got %v, reference %v", query, got, want)
			}
		})
	}
}
