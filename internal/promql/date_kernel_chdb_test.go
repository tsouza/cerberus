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

// Range execution checks both shadow precedence and nested float narrowing
// against independently computed rows and the live Prometheus oracle.
func TestDateKernelRangeParity_ChDB(t *testing.T) {
	const (
		fixturePermissions = 0o600
		gridStep           = 15 * time.Second
		gridIntervals      = 2
	)
	end := foEvalTS.Add(gridIntervals * gridStep)
	for _, fn := range []struct {
		name  string
		value func(time.Time) float64
	}{
		{"year", func(tm time.Time) float64 { return float64(tm.Year()) }},
		{"month", func(tm time.Time) float64 { return float64(tm.Month()) }},
		{"day_of_month", func(tm time.Time) float64 { return float64(tm.Day()) }},
		{"day_of_week", dfPromDayOfWeek},
		{"day_of_year", func(tm time.Time) float64 { return float64(tm.YearDay()) }},
		{"days_in_month", dfDaysInMonth},
		{"hour", func(tm time.Time) float64 { return float64(tm.Hour()) }},
		{"minute", func(tm time.Time) float64 { return float64(tm.Minute()) }},
	} {
		for _, histFirst := range []bool{true, false} {
			for _, nested := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/hist_first_%t/nested_%t", fn.name, histFirst, nested), func(t *testing.T) {
					left, right := tkShadowFloatMetric, tkShadowHistMetric
					if histFirst {
						left, right = right, left
					}
					operand := left + " or " + right
					if nested {
						operand = `sort_by_label(` + operand + `, "series")`
					}
					query := fn.name + "(" + operand + ")"
					values := map[string]int64{"solo": 7}
					if !histFirst {
						values["dup"] = 42
					}
					var expected [][]any
					for at := foEvalTS; !at.After(end); at = at.Add(gridStep) {
						for series, value := range values {
							expected = append(expected, []any{"", map[string]string{"series": series}, at.Format(time.RFC3339), fn.value(time.Unix(value, 0).UTC())})
						}
					}
					want, err := json.Marshal(expected)
					if err != nil {
						t.Fatal(err)
					}
					archive := &txtar.Archive{Files: []txtar.File{
						{Name: "query.promql", Data: []byte(query + "\n")},
						{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query_range\nscope: full\n")},
						{Name: "seed", Data: []byte(tkShadowSeed)},
						{Name: "expected_rows", Data: want},
					}}
					path := filepath.Join(t.TempDir(), "date_range.txtar")
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
					plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, end, gridStep)
					if err != nil {
						t.Fatal(err)
					}
					optimized := spec.AssertScanTimeBoundAccepts(t, plan)
					sqlText, args, err := chsql.Emit(context.Background(), optimized)
					if err != nil {
						t.Fatal(err)
					}
					rows := spec.RunRoundTripSQL(t, fixture, sqlText, args)
					spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: end, Step: gridStep}, rows)
				})
			}
		}
	}
}
