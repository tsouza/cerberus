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

func TestTimestampNestedMixedRowsAndShadowParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	for _, shadow := range []bool{false, true} {
		for _, histFirst := range []bool{false, true} {
			for _, sort := range []string{"", "sort_by_label", "sort_by_label_desc"} {
				for _, modifier := range []string{"", " offset 1s", " @ 1767225600"} {
					for _, step := range []time.Duration{0, time.Second} {
						t.Run(fmt.Sprintf("shadow=%v/histFirst=%v/sort=%s/modifier=%s/step=%s", shadow, histFirst, sort, modifier, step), func(t *testing.T) {
							hist, float, seed := foHistMetric, foFloatMetric, foSeed
							labels := []map[string]string{{"series": "f1", "bucket": "b1"}, {"series": "f2", "bucket": "b1"}, {"series": "f3", "bucket": "b3"}, {"series": "h1", "bucket": "b1"}, {"series": "h2", "bucket": "b2"}}
							if shadow {
								hist, float, seed = tkShadowHistMetric, tkShadowFloatMetric, tkShadowSeed
								labels = []map[string]string{{"series": "dup"}, {"series": "solo"}}
							}
							left, right := float+modifier, hist+modifier
							if histFirst {
								left, right = right, left
							}
							operand := left + " or " + right
							if sort != "" {
								operand = sort + "(" + operand + `, "series")`
							}
							query := "timestamp(" + operand + ")"
							rows := [][]any{}
							times := []time.Time{foEvalTS}
							if step > 0 {
								times = append(times, foEvalTS.Add(step))
							}
							for _, at := range times {
								for _, label := range labels {
									rows = append(rows, []any{"", label, at.Format(time.RFC3339), at.Unix()})
								}
							}
							expected, err := json.Marshal(rows)
							if err != nil {
								t.Fatal(err)
							}
							endpoint := "/api/v1/query"
							if step > 0 {
								endpoint = "/api/v1/query_range"
							}
							archive := &txtar.Archive{Files: []txtar.File{
								{Name: "query.promql", Data: []byte(query + "\n")},
								{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
								{Name: "seed", Data: []byte(seed)},
								{Name: "expected_rows", Data: expected},
							}}
							path := filepath.Join(t.TempDir(), "timestamp_mixed.txtar")
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
							if chplan.RowShapeOf(plan) != chplan.SampleRowShape {
								t.Fatal("timestamp must return floats, not retain histogram output payload")
							}
							plan = spec.AssertScanTimeBoundAccepts(t, plan)
							sql, args, err := chsql.Emit(context.Background(), plan)
							if err != nil {
								t.Fatal(err)
							}
							actual := spec.RunRoundTripSQL(t, fixture, sql, args)
							spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS.Add(step), Step: step}, actual)
						})
					}
				}
			}
		}
	}
}
