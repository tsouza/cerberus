//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
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

const floatAggregateParityStep = 15 * time.Second

// A duplicate float h1 shares a histogram identity. LHS union precedence must
// resolve that collision before the aggregate discards histogram rows.
func floatAggregateParitySeed(t *testing.T) string {
	t.Helper()
	// Keep every population variance binary-exact under strict expected_rows
	// comparison. The duplicate remains above both b1 floats, so MAX also
	// distinguishes histogram-first shadowing from float-first precedence.
	const original = "map('series', 'f3', 'bucket', 'b3'), toDateTime64('2026-01-01 00:00:00', 9), 1.0)"
	const replacement = "map('series', 'f3', 'bucket', 'b3'), toDateTime64('2026-01-01 00:00:00', 9), 6.0)"
	if strings.Count(foSeed, original) != 1 {
		t.Fatal("shared float seed must contain exactly one expected f3 row")
	}
	seed := strings.Replace(foSeed, original, replacement, 1)
	return seed + "INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES ('" + foFloatMetric + "', map('series', 'h1', 'bucket', 'b1'), toDateTime64('2026-01-01 00:00:00', 9), 15);\n"
}

func floatAggregateExpectedGroups(histFirst bool, grouping string) map[string][]float64 {
	groups := map[string][]float64{"b1": {3, 9}, "b3": {6}}
	if !histFirst {
		groups["b1"] = append(groups["b1"], 15)
	}
	switch grouping {
	case "empty":
		return nil
	case "":
		return map[string][]float64{"": append(groups["b1"], groups["b3"]...)}
	default:
		return groups
	}
}

func floatAggregateExpectedValue(fn string, values []float64) float64 {
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	switch fn {
	case "min":
		return values[0]
	case "max":
		return values[len(values)-1]
	case "quantile":
		mid := (len(values) - 1) / 2
		if len(values)%2 == 0 {
			return (values[mid] + values[mid+1]) / 2
		}
		return values[mid]
	default:
		// The pairwise identity keeps the integer seed's numerator exact until
		// the final division, independently of the backend's accumulation order.
		var variance float64
		for i, value := range values {
			for _, other := range values[i+1:] {
				variance += (value - other) * (value - other)
			}
		}
		count := float64(len(values))
		variance /= count * count
		if fn == "stddev" {
			return math.Sqrt(variance)
		}
		return variance
	}
}

func runFloatAggregateParity(t *testing.T, query, fn string, groups map[string][]float64, step time.Duration, specialValue string) {
	t.Helper()
	const (
		fixturePermissions = 0o600
		gridIntervals      = 2
	)
	end := foEvalTS.Add(gridIntervals * step)
	expected := make([][]any, 0)
	for at := foEvalTS; !at.After(end); at = at.Add(step) {
		for group, values := range groups {
			attrs := map[string]string{}
			if group != "" {
				attrs["bucket"] = group
			}
			var value any = floatAggregateExpectedValue(fn, values)
			if specialValue != "" {
				value = specialValue
			}
			expected = append(expected, []any{"", attrs, at.Format(time.RFC3339), value})
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
		{Name: "seed", Data: []byte(floatAggregateParitySeed(t))},
		{Name: "expected_rows", Data: want},
	}}
	path := filepath.Join(t.TempDir(), "float_aggregate.txtar")
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
}

func TestFloatAggregateMixedParity_ChDB(t *testing.T) {
	for _, fn := range []string{"min", "max", "stddev", "stdvar", "quantile"} {
		for _, histFirst := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				for _, step := range []time.Duration{0, floatAggregateParityStep} {
					for _, grouping := range []string{"", " by(bucket)", " without(series)", "empty"} {
						t.Run(fmt.Sprintf("%s/hist_first_%v/nested_%v/%v/%s", fn, histFirst, nested, step, grouping), func(t *testing.T) {
							left, right := foFloatMetric, foHistMetric
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
							if fn == "quantile" {
								operand = "0.5," + operand
							}
							clause := grouping
							if grouping == "empty" {
								clause = ""
							}
							query := fn + clause + "(" + operand + ")"
							runFloatAggregateParity(t, query, fn, floatAggregateExpectedGroups(histFirst, grouping), step, "")
						})
					}
				}
			}
		}
	}
}

func TestFloatAggregateQuantileDomainParity_ChDB(t *testing.T) {
	for _, tc := range []struct{ phi, want string }{
		{"-1", "-Inf"},
		{"2", "+Inf"},
		{"NaN", "NaN"},
		{"scalar(vector(0.5))", ""},
		{"scalar(vector(-1))", "-Inf"},
		{"scalar(vector(2))", "+Inf"},
		{"scalar(vector(NaN))", "NaN"},
	} {
		for _, histFirst := range []bool{false, true} {
			for _, step := range []time.Duration{0, floatAggregateParityStep} {
				t.Run(fmt.Sprintf("%s/hist_first_%v/%v", tc.phi, histFirst, step), func(t *testing.T) {
					left, right := foFloatMetric, foHistMetric
					if histFirst {
						left, right = right, left
					}
					query := "quantile by(bucket)(" + tc.phi + ",sort_by_label(" + left + " or " + right + `,"series"))`
					runFloatAggregateParity(t, query, "quantile", floatAggregateExpectedGroups(histFirst, " by(bucket)"), step, tc.want)
				})
			}
		}
	}
}

func TestFloatAggregateOrdinaryParity_ChDB(t *testing.T) {
	for _, fn := range []string{"min", "max", "stddev", "stdvar", "quantile"} {
		for _, step := range []time.Duration{0, floatAggregateParityStep} {
			t.Run(fmt.Sprintf("%s/%v", fn, step), func(t *testing.T) {
				operand := foFloatMetric
				if fn == "quantile" {
					operand = "0.5," + operand
				}
				runFloatAggregateParity(t, fn+" by(bucket)("+operand+")", fn, floatAggregateExpectedGroups(false, " by(bucket)"), step, "")
			})
		}
	}
}
