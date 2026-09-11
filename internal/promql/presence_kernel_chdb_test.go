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

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestPresenceKernelParity_ChDB(t *testing.T) {
	const (
		fixturePermissions = 0o600
		gridStep           = 15 * time.Second
		gridIntervals      = 2
		seedSeriesCount    = 4
	)
	// The extra float shares h1's complete labelset. It must not increase the
	// row count regardless of which kind wins the union's LHS precedence.
	seed := cgSeed + "INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ('" + cgFloatMetric + "', map('series', 'h1', 'group', 'g1'), toDateTime64('2026-01-01 00:00:00', 9), 9999);\n"
	for _, fn := range []string{"count", "group"} {
		for _, histFirst := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				for _, step := range []time.Duration{0, gridStep} {
					for _, grouping := range []string{"", " by(group)", " without(series)", "empty"} {
						t.Run(fmt.Sprintf("%s/hist_first_%v/nested_%v/%v/%s", fn, histFirst, nested, step, grouping), func(t *testing.T) {
							left, right := cgFloatMetric, cgHistMetric
							if histFirst {
								left, right = right, left
							}
							if grouping == "empty" {
								left += `{series="missing"}`
								right += `{series="missing"}`
							}
							operand := left + " or " + right
							if nested {
								operand = `sort_by_label(` + operand + `,"series")`
							}
							groupClause := grouping
							if grouping == "empty" {
								groupClause = ""
							}
							query := fn + groupClause + "(" + operand + ")"
							end := foEvalTS.Add(gridIntervals * step)
							groups := map[string]float64{"": seedSeriesCount}
							if grouping != "" && grouping != "empty" {
								groups = map[string]float64{"g1": 2, "g2": 1, "g3": 1}
							}
							if grouping == "empty" {
								groups = nil
							}
							expected := make([][]any, 0)
							for at := foEvalTS; !at.After(end); at = at.Add(step) {
								for group, count := range groups {
									attrs := map[string]string{}
									if group != "" {
										attrs["group"] = group
									}
									if fn == "group" {
										count = 1
									}
									expected = append(expected, []any{"", attrs, at.Format(time.RFC3339), count})
								}
								if step == 0 {
									break
								}
							}
							want, err := json.Marshal(expected)
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
								{Name: "expected_rows", Data: want},
							}}
							path := filepath.Join(t.TempDir(), "presence.txtar")
							if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
								t.Fatal(err)
							}
							fixture, err := spec.Load(path)
							if err != nil {
								t.Fatal(err)
							}
							p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
							expr, err := p.ParseExpr(query)
							if err != nil {
								t.Fatal(err)
							}
							plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, end, step)
							if err != nil {
								t.Fatal(err)
							}
							optimized := spec.AssertScanTimeBoundAccepts(t, plan)
							sqlText, args, err := chsql.Emit(context.Background(), optimized)
							if err != nil {
								t.Fatal(err)
							}
							rows := spec.RunRoundTripSQL(t, fixture, sqlText, args)
							spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: end, Step: step}, rows)
						})
					}
				}
			}
		}
	}
}
