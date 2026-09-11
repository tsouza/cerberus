//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestScalarComparisonAllOperatorsShadowParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	const threshold = 7.0
	for _, tc := range []struct {
		op      string
		compare func(float64, float64) bool
	}{
		{"==", func(a, b float64) bool { return a == b }},
		{"!=", func(a, b float64) bool { return a != b }},
		{"<", func(a, b float64) bool { return a < b }},
		{"<=", func(a, b float64) bool { return a <= b }},
		{">", func(a, b float64) bool { return a > b }},
		{">=", func(a, b float64) bool { return a >= b }},
	} {
		for _, histFirst := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				for _, left := range []bool{false, true} {
					for _, boolean := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/histFirst=%v/nested=%v/left=%v/bool=%v", tc.op, histFirst, nested, left, boolean), func(t *testing.T) {
							operand := "(tk_shadow_float_side_gauge or tk_shadow_hist_side_exp_hist)"
							if histFirst {
								operand = "(tk_shadow_hist_side_exp_hist or tk_shadow_float_side_gauge)"
							}
							if nested {
								operand = "sort_by_label(" + operand + `, "series")`
							}
							op := tc.op
							if boolean {
								op += " bool"
							}
							query := operand + " " + op + " 7"
							if left {
								query = "7 " + op + " " + operand
							}
							rows := [][]any{}
							for _, sample := range []struct {
								series string
								value  float64
							}{{"dup", 42}, {"solo", 7}} {
								if histFirst && sample.series == "dup" {
									continue
								}
								a, b := sample.value, threshold
								if left {
									a, b = b, a
								}
								matches := tc.compare(a, b)
								if !boolean && !matches {
									continue
								}
								name, value := "tk_shadow_float_side_gauge", sample.value
								if boolean {
									name = ""
									value = 0
									if matches {
										value = 1
									}
								}
								row := []any{name, map[string]string{"series": sample.series}, "2026-01-01T00:00:00Z", value}
								if nested && !boolean {
									// A filtering comparison preserves the existing mixed
									// physical envelope; histogram payloads are zero-filled.
									row = append(row, 0, 0, 0, 0, 0, 0, []any{}, 0, []any{}, 0)
								}
								rows = append(rows, row)
							}
							expected, err := json.Marshal(rows)
							if err != nil {
								t.Fatal(err)
							}
							archive := &txtar.Archive{Files: []txtar.File{
								{Name: "query.promql", Data: []byte(query + "\n")},
								{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query\nscope: full\n")},
								{Name: "seed", Data: []byte(tkShadowSeed)},
								{Name: "expected_rows", Data: expected},
							}}
							path := filepath.Join(t.TempDir(), "comparison.txtar")
							if err = os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
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
							plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS)
							if err != nil {
								t.Fatal(err)
							}
							plan = spec.AssertScanTimeBoundAccepts(t, plan)
							sql, args, err := chsql.Emit(context.Background(), plan)
							if err != nil {
								t.Fatal(err)
							}
							actual := spec.RunRoundTripSQL(t, fixture, sql, args)
							spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS}, actual)
						})
					}
				}
			}
		}
	}
}
