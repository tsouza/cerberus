//go:build chdb

package promql_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func arithmeticWireValue(value float64) any {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	default:
		return value
	}
}

func TestScalarArithmeticShadowAndValuesParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	for _, tc := range []struct {
		op, scalar string
		left       bool
		negative   bool
		value      func(float64) float64
	}{
		{"+", "-3", false, false, func(v float64) float64 { return v - 3 }},
		{"+", "-3", true, false, func(v float64) float64 { return -3 + v }},
		{"-", "3", false, false, func(v float64) float64 { return v - 3 }},
		{"-", "3", true, false, func(v float64) float64 { return 3 - v }},
		{"%", "3", false, false, func(v float64) float64 { return math.Mod(v, 3) }},
		{"%", "3", true, false, func(v float64) float64 { return math.Mod(3, v) }},
		{"^", "2", false, false, func(v float64) float64 { return v * v }},
		{"^", "-2", true, false, func(v float64) float64 { return math.Pow(-2, v) }},
		// Axis/quadrant probes have exact shared Float64 answers. General
		// angles can differ by one libm rounding bit from Go's math.Atan2;
		// nonzero operands are pinned separately by SQL/IR equivalence.
		{"atan2", "0", false, true, func(v float64) float64 { return math.Atan2(v, 0) }},
		{"atan2", "0", true, true, func(v float64) float64 { return math.Atan2(0, v) }},
		{"atan2", "-0", true, true, func(v float64) float64 { return math.Atan2(math.Copysign(0, -1), v) }},
		{"/", "3", true, false, func(v float64) float64 { return 3 / v }},
		{"%", "0", false, false, func(float64) float64 { return math.NaN() }},
		{"+", "NaN", false, false, func(float64) float64 { return math.NaN() }},
		{"+", "+Inf", true, false, func(float64) float64 { return math.Inf(1) }},
		{"-", "-Inf", true, false, func(float64) float64 { return math.Inf(-1) }},
		{"^", "0.5", false, true, func(v float64) float64 { return math.Sqrt(v) }},
	} {
		for _, histFirst := range []bool{false, true} {
			for _, nested := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/left=%v/histFirst=%v/nested=%v", tc.op, tc.scalar, tc.left, histFirst, nested), func(t *testing.T) {
					left, right := tkShadowFloatMetric, tkShadowHistMetric
					if histFirst {
						left, right = right, left
					}
					operand := "(" + left + " or " + right + ")"
					if nested {
						operand = "sort_by_label(" + operand + `, "series")`
					}
					// Parentheses make a negative literal the actual scalar base,
					// rather than unary minus applied outside exponentiation.
					scalar := "(" + tc.scalar + ")"
					query := operand + " " + tc.op + " " + scalar
					if tc.left {
						query = scalar + " " + tc.op + " " + operand
					}
					seed := tkShadowSeed
					solo := 7.0
					if tc.negative {
						seed = strings.ReplaceAll(seed, ", 7.0)", ", -7.0)")
						solo = -solo
					}
					rows := [][]any{}
					if !histFirst {
						rows = append(rows, []any{"", map[string]string{"series": "dup"}, "2026-01-01T00:00:00Z", arithmeticWireValue(tc.value(42))})
					}
					rows = append(rows, []any{"", map[string]string{"series": "solo"}, "2026-01-01T00:00:00Z", arithmeticWireValue(tc.value(solo))})
					expected, err := json.Marshal(rows)
					if err != nil {
						t.Fatal(err)
					}
					archive := &txtar.Archive{Files: []txtar.File{
						{Name: "query.promql", Data: []byte(query + "\n")},
						{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query\nscope: full\n")},
						{Name: "seed", Data: []byte(seed)},
						{Name: "expected_rows", Data: expected},
					}}
					path := filepath.Join(t.TempDir(), "arithmetic.txtar")
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
					plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS)
					if err != nil {
						t.Fatal(err)
					}
					optimized := spec.AssertScanTimeBoundAccepts(t, plan)
					sql, params, err := chsql.Emit(context.Background(), optimized)
					if err != nil {
						t.Fatal(err)
					}
					result := spec.RunRoundTripSQL(t, fixture, sql, params)
					spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS}, result)
				})
			}
		}
	}
}
