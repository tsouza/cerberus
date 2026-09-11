//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// Histograms have unequal source timestamps. Every grouping mode must reduce
// them at the evaluation instant rather than creating one group per scrape.
const mixedSumAvgSeed = `
CREATE TABLE otel_metrics_histogram (MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Count UInt64, Sum Float64, BucketCounts Array(UInt64), ExplicitBounds Array(Float64)) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
CREATE TABLE otel_metrics_exponential_histogram (MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Count UInt64, Sum Float64, Scale Int32, ZeroCount UInt64, PositiveOffset Int32, PositiveBucketCounts Array(UInt64), NegativeOffset Int32, NegativeBucketCounts Array(UInt64)) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
INSERT INTO otel_metrics_exponential_histogram (MetricName,Attributes,TimeUnix,Count,Sum,Scale,ZeroCount,PositiveOffset,PositiveBucketCounts,NegativeOffset,NegativeBucketCounts) VALUES
 ('group_exp_hist',map('series','dup','bucket','mixed'),toDateTime64('2025-12-31 23:59:57',9),2,4,0,0,0,[2],0,[]),
 ('group_exp_hist',map('series','h1','bucket','hist'),toDateTime64('2025-12-31 23:59:58',9),2,4,0,0,0,[2],0,[]),
 ('group_exp_hist',map('series','h2','bucket','hist'),toDateTime64('2025-12-31 23:59:59',9),4,8,0,0,0,[4],0,[]);
CREATE TABLE otel_metrics_gauge (MetricName String, Attributes Map(String,String), ResourceAttributes Map(String,String) DEFAULT map(), ServiceName LowCardinality(String) DEFAULT '', TimeUnix DateTime64(9), Value Float64) ENGINE=MergeTree ORDER BY (MetricName,Attributes,TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName,Attributes,TimeUnix,Value) VALUES
 ('group_gauge',map('series','dup','bucket','mixed'),toDateTime64('2026-01-01 00:00:00',9),99),
 ('group_gauge',map('series','f0','bucket','mixed'),toDateTime64('2026-01-01 00:00:00',9),10),
 ('group_gauge',map('series','f1','bucket','float'),toDateTime64('2026-01-01 00:00:00',9),6),
 ('group_gauge',map('series','f2','bucket','float'),toDateTime64('2026-01-01 00:00:00',9),10);
`

func TestSumAvgNestedMixedGroupParity_ChDB(t *testing.T) {
	const (
		fixturePermissions    = 0o600
		histogramCountSum     = 6.0
		histogramValueSum     = 12.0
		floatGroupSum         = 16.0
		shadowedFloatGroupSum = 109.0
		pureGroupSize         = 2
	)
	for _, op := range []string{"sum", "avg"} {
		for _, histFirst := range []bool{false, true} {
			for _, wrapper := range []string{"sort_by_label", "sort_by_label_desc"} {
				for _, step := range []time.Duration{0, time.Second} {
					for _, grouping := range []string{"", " by(bucket)", " without(series)", "empty"} {
						t.Run(fmt.Sprintf("%s/%v/%s/%s/%s", op, histFirst, wrapper, step, grouping), func(t *testing.T) {
							hist, floats := "group_exp_hist", "group_gauge"
							clause := grouping
							if grouping == "empty" {
								hist += `{series="missing"}`
								floats += `{series="missing"}`
								clause = ""
							}
							left, right := floats, hist
							if histFirst {
								left, right = right, left
							}
							query := op + clause + "(" + wrapper + "(" + left + " or " + right + `,"series"))`
							rows := [][]any{}
							for at := foEvalTS; !at.After(foEvalTS.Add(step)); at = at.Add(step) {
								if grouping != "" && grouping != "empty" {
									hCount, hSum, floatValue, mixedValue := histogramCountSum, histogramValueSum, floatGroupSum, shadowedFloatGroupSum
									if op == "avg" {
										hCount /= pureGroupSize
										hSum /= pureGroupSize
										floatValue /= pureGroupSize
										mixedValue /= pureGroupSize
									}
									rows = append(rows, []any{"", map[string]string{"bucket": "hist"}, at.Format(time.RFC3339), 0, hCount, hSum, 0, 0, 0, 0, []float64{hCount}, 0, []any{}, 1})
									floatRow := func(bucket string, value float64) []any {
										return []any{"", map[string]string{"bucket": bucket}, at.Format(time.RFC3339), value, 0, 0, 0, 0, 0, 0, []any{}, 0, []any{}, 0}
									}
									rows = append(rows, floatRow("float", floatValue))
									if !histFirst {
										rows = append(rows, floatRow("mixed", mixedValue))
									}
								}
								if step == 0 {
									break
								}
							}
							want, err := json.Marshal(rows)
							if err != nil {
								t.Fatal(err)
							}
							endpoint := "/api/v1/query"
							if step > 0 {
								endpoint = "/api/v1/query_range"
							}
							archive := &txtar.Archive{Files: []txtar.File{
								{Name: "query.promql", Data: []byte(query + "\n")},
								{Name: "seed", Data: []byte(mixedSumAvgSeed)},
								{Name: "expected_rows", Data: want},
								{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
							}}
							path := filepath.Join(t.TempDir(), "sum_avg.txtar")
							if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
								t.Fatal(err)
							}
							fixture, err := spec.Load(path)
							if err != nil {
								t.Fatal(err)
							}
							expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
							if err != nil {
								t.Fatal(err)
							}
							plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS.Add(step), step)
							if err != nil {
								t.Fatal(err)
							}
							for _, optimized := range []bool{false, true} {
								t.Run(fmt.Sprintf("optimized=%v", optimized), func(t *testing.T) {
									n := chplan.CloneNode(plan)
									if optimized {
										n = spec.AssertScanTimeBoundAccepts(t, n)
									}
									sql, args, err := chsql.Emit(context.Background(), n)
									if err != nil {
										t.Fatal(err)
									}
									actual := spec.RunRoundTripSQL(t, fixture, sql, args)
									spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS.Add(step), Step: step}, actual)
								})
							}
						})
					}
				}
			}
		}
	}
}
