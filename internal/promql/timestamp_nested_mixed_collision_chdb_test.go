//go:build chdb

package promql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
	"github.com/tsouza/cerberus/test/spec"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

const nestedTimestampCollisionSeed = `
CREATE TABLE otel_metrics_histogram (
 MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Count UInt64, Sum Float64, BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
CREATE TABLE otel_metrics_exponential_histogram (
 MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Count UInt64, Sum Float64, Scale Int32, ZeroCount UInt64, PositiveOffset Int32, PositiveBucketCounts Array(UInt64), NegativeOffset Int32, NegativeBucketCounts Array(UInt64)
) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
INSERT INTO otel_metrics_exponential_histogram (MetricName,Attributes,TimeUnix,Count,Sum,Scale,ZeroCount,PositiveOffset,PositiveBucketCounts,NegativeOffset,NegativeBucketCounts) VALUES
 ('collision_exp_hist',map('series','hist'),toDateTime64('2026-01-01 00:00:00',9),2,4.,0,0,0,[6],0,[]);
CREATE TABLE otel_metrics_gauge (
 MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Value Float64
) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName,Attributes,TimeUnix,Value) VALUES
 ('collision_gauge',map('series','float'),toDateTime64('2025-12-31 23:59:59',9),42.),
 ('collision_other_gauge',map('series','float'),toDateTime64('2025-12-31 23:59:58',9),7.);
`

func TestTimestampNestedMixedDuplicateLabelsetParity_ChDB(t *testing.T) {
	const duplicateMessage = "vector cannot contain metrics with the same labelset"
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	standard := schema.DefaultOTelMetrics()
	custom := standard
	custom.MetricNameColumn, custom.AttributesColumn, custom.TimestampColumn, custom.ValueColumn = "metric_id", "label_map", "sample_time", "sample_value"
	for _, s := range []schema.Metrics{standard, custom} {
		for _, step := range []time.Duration{0, time.Second} {
			for _, histFirst := range []bool{false, true} {
				for _, sort := range []string{"sort_by_label", "sort_by_label_desc"} {
					t.Run(fmt.Sprintf("%s/%s/histFirst=%v/%s", s.ValueColumn, step, histFirst, sort), func(t *testing.T) {
						left, right := `{__name__=~"collision_gauge|collision_other_gauge"}`, "collision_exp_hist"
						if histFirst {
							left, right = right, left
						}
						query := "timestamp(" + sort + "(" + left + " or " + right + `, "series"))`
						seeded := []oracle.Series{
							{Labels: map[string]string{"__name__": "collision_exp_hist", "series": "hist"}, Points: []oracle.Point{{TMillis: at.Add(-time.Second).UnixMilli(), Histogram: &oracle.Histogram{Count: 2, Sum: 4, Scale: 0, PositiveBuckets: []float64{6}}}}},
							{Labels: map[string]string{"__name__": "collision_gauge", "series": "float"}, Points: []oracle.Point{{TMillis: at.Add(-2 * time.Second).UnixMilli(), Value: 42}}},
							{Labels: map[string]string{"__name__": "collision_other_gauge", "series": "float"}, Points: []oracle.Point{{TMillis: at.Add(-3 * time.Second).UnixMilli(), Value: 7}}},
						}
						_, err := oracle.Evaluate(t, seeded, oracle.Query{Expr: query, Start: at, End: at.Add(step), Step: step})
						if err == nil || !strings.Contains(err.Error(), duplicateMessage) {
							t.Fatalf("reference must reject duplicate label sets: %v", err)
						}
						expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
						if err != nil {
							t.Fatal(err)
						}
						plan, err := LowerAtRange(context.Background(), expr, s, at, at.Add(step), step)
						if err != nil {
							t.Fatal(err)
						}
						plan = spec.AssertScanTimeBoundAccepts(t, plan)
						sql, args, err := chsql.Emit(context.Background(), plan)
						if err != nil {
							t.Fatal(err)
						}
						sql = testsql.NestMapWhere(testsql.NestMapOrderBy(testsql.RewriteMapProjections(sql)))
						db := spec.OpenChDB(t)
						seed := strings.NewReplacer("MetricName", s.MetricNameColumn, "Attributes", s.AttributesColumn, "TimeUnix", s.TimestampColumn, "Value", s.ValueColumn).Replace(nestedTimestampCollisionSeed)
						// ResourceAttributes is not a configurable sample-label column.
						seed = strings.ReplaceAll(seed, "Resource"+s.AttributesColumn, "ResourceAttributes")
						for _, stmt := range testsql.SplitStatements(seed) {
							if _, err := db.Exec(stmt); err != nil {
								t.Fatal(err)
							}
						}
						rows, err := db.Query(sql, args...)
						if err == nil {
							for rows.Next() {
							}
							err = rows.Err()
							if closeErr := rows.Close(); err == nil {
								err = closeErr
							}
						}
						if err == nil || !strings.Contains(err.Error(), duplicateMessage) {
							t.Fatalf("timestamp accepted duplicate output labels: %v", err)
						}
					})
				}
			}
		}
	}
}
