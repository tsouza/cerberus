//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestScalarOperandOptimizedParity_ChDB(t *testing.T) {
	for _, histFirst := range []bool{false, true} {
		for _, wrapper := range []string{"", "sort_by_label", "sort_by_label_desc"} {
			for _, selection := range []string{"", `{series="solo"}`, `{series="missing"}`} {
				operand := tkShadowHistMetric + " or " + tkShadowFloatMetric + selection
				if !histFirst {
					operand = tkShadowFloatMetric + selection + " or " + tkShadowHistMetric
				}
				if wrapper != "" {
					operand = wrapper + "(" + operand + `, "series")`
				}
				value := any("NaN")
				if selection == `{series="solo"}` || (selection == "" && histFirst) {
					value = float64(7)
				}
				t.Run(fmt.Sprintf("instant/%v/%s/%s", histFirst, wrapper, selection), func(t *testing.T) {
					scalar := "scalar(" + operand + ")"
					for index, query := range []string{scalar, "vector(" + scalar + ")", "clamp_min(" + tkShadowFloatMetric + `{series="solo"}, ` + scalar + ")"} {
						t.Run(fmt.Sprint(index), func(t *testing.T) {
							labels := map[string]string{}
							timestamp := foEvalTS.Format(time.RFC3339)
							if strings.HasPrefix(query, "clamp_min(") {
								labels["series"] = "solo"
								timestamp = "2026-01-01T00:00:00Z"
							}
							scalarOperandParity(t, query, tkShadowSeed, foEvalTS, foEvalTS, 0,
								[][]any{{"", labels, timestamp, value}})
						})
					}
				})
			}
			t.Run(fmt.Sprintf("range/%v/%s", histFirst, wrapper), func(t *testing.T) {
				// Histograms exist first; then the duplicate float arrives, then
				// the solo float. Finally histogram staleness exposes both floats.
				seed := strings.Replace(tkShadowSeed, "toDateTime64('2026-01-01 00:00:00', 9), 42.0)", "toDateTime64('2026-01-01 00:02:00', 9), 42.0)", 1)
				seed = strings.Replace(seed, "toDateTime64('2026-01-01 00:00:00', 9), 7.0)", "toDateTime64('2026-01-01 00:03:00', 9), 7.0)", 1)
				operand := tkShadowHistMetric + " or " + tkShadowFloatMetric
				if !histFirst {
					operand = tkShadowFloatMetric + " or " + tkShadowHistMetric
				}
				if wrapper != "" {
					operand = wrapper + "(" + operand + `, "series")`
				}
				const step = 2 * time.Minute
				const finalOffset = 6 * time.Minute
				start, end := foEvalTS.Add(-step), foEvalTS.Add(finalOffset)
				values := []any{"NaN", "NaN", float64(42), "NaN", "NaN"}
				if histFirst {
					values = []any{"NaN", "NaN", "NaN", float64(7), "NaN"}
				}
				rows := make([][]any, 0, len(values))
				for i, value := range values {
					rows = append(rows, []any{"", map[string]string{}, start.Add(time.Duration(i) * step).Format(time.RFC3339), value})
				}
				for index, query := range []string{"scalar(" + operand + ")", "vector(scalar(" + operand + "))"} {
					t.Run(fmt.Sprint(index), func(t *testing.T) {
						scalarOperandParity(t, query, seed, start, end, step, rows)
					})
				}
			})
		}
	}
	for index, operand := range []string{tkShadowHistMetric, tkShadowHistMetric + " + 0", tkShadowHistMetric + `{series="missing"}`, tkShadowFloatMetric + `{series="missing"}`} {
		t.Run(fmt.Sprintf("empty/%d", index), func(t *testing.T) {
			scalarOperandParity(t, "scalar("+operand+")", tkShadowSeed, foEvalTS, foEvalTS, 0,
				[][]any{{"", map[string]string{}, foEvalTS.Format(time.RFC3339), "NaN"}})
		})
	}
}

func scalarOperandParity(t *testing.T, query, seed string, start, end time.Time, step time.Duration, rows [][]any) {
	t.Helper()
	const permissions = 0o600
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
	path := filepath.Join(t.TempDir(), "scalar.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), permissions); err != nil {
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
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatal(err)
	}
	optimized := spec.AssertScanTimeBoundAccepts(t, plan)
	sql, args, err := chsql.Emit(context.Background(), optimized)
	if err != nil {
		t.Fatal(err)
	}
	result := spec.RunRoundTripSQL(t, fixture, sql, args)
	spec.RunParity(t, fixture, spec.ParityEval{Start: start, End: end, Step: step}, result)
}
