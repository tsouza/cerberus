package promql

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func mathFamilyContractLower(t *testing.T, query string) (chplan.Node, error) {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	ts := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	return LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), ts, ts)
}

func TestMathFamilyFirstError(t *testing.T) {
	const operand = `count_values("", up)`
	const minVector = `topk(NaN, up)`
	const maxVector = `topk(Inf, up)`
	const minBound = `scalar(` + minVector + `)`
	const maxBound = `scalar(` + maxVector + `)`
	const mixed = `latency_exp_hist or ` + operand
	want := map[string]string{}
	for name, query := range map[string]string{"operand": operand, "min": minVector, "max": maxVector} {
		_, err := mathFamilyContractLower(t, query)
		if err == nil {
			t.Fatalf("sentinel %s must fail", name)
		}
		want[name] = err.Error()
	}
	if want["operand"] == want["min"] || want["min"] == want["max"] || want["operand"] == want["max"] {
		t.Fatal("sentinel errors must distinguish ordering")
	}
	for _, tc := range []struct{ name, query, first string }{
		{"ordinary_round_bound_first", `round(` + operand + `, ` + minBound + `)`, "min"},
		{"mixed_round_operand_first", `round(` + mixed + `, ` + minBound + `)`, "operand"},
		{"ordinary_clamp_min_bound_first", `clamp_min(` + operand + `, ` + minBound + `)`, "min"},
		{"mixed_clamp_min_operand_first", `clamp_min(` + mixed + `, ` + minBound + `)`, "operand"},
		{"ordinary_clamp_max_bound_first", `clamp_max(` + operand + `, ` + maxBound + `)`, "max"},
		{"mixed_clamp_max_operand_first", `clamp_max(` + mixed + `, ` + maxBound + `)`, "operand"},
		{"ordinary_clamp_min_before_max_and_operand", `clamp(` + operand + `, ` + minBound + `, ` + maxBound + `)`, "min"},
		{"ordinary_clamp_max_before_operand", `clamp(` + operand + `, scalar(vector(0)), ` + maxBound + `)`, "max"},
		{"mixed_clamp_operand_before_both_bounds", `clamp(` + mixed + `, ` + minBound + `, ` + maxBound + `)`, "operand"},
		{"mixed_clamp_min_before_max", `clamp(latency_exp_hist or up, ` + minBound + `, ` + maxBound + `)`, "min"},
		{"mixed_clamp_max_after_valid_min", `clamp(latency_exp_hist or up, scalar(vector(0)), ` + maxBound + `)`, "max"},
		{"ordinary_inverted_literal_still_lowers_operand", `clamp(` + operand + `, 10, 5)`, "operand"},
		{"mixed_inverted_literal_still_lowers_operand", `clamp(` + mixed + `, 10, 5)`, "operand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mathFamilyContractLower(t, tc.query)
			if err == nil || err.Error() != want[tc.first] {
				t.Fatalf("query %s: first error=%v; want %s sentinel %q", tc.query, err, tc.first, want[tc.first])
			}
		})
	}
}

func TestMathFamilyInvertedLiteralTopology(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, tc := range []struct {
		name, vector string
		mixed        bool
	}{
		{"ordinary", "up", false},
		{"hist_first", "latency_exp_hist or up", true},
		{"float_first", "up or latency_exp_hist", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := "clamp(" + tc.vector + ", 10, 5)"
			plan, err := mathFamilyContractLower(t, query)
			if err != nil {
				t.Fatal(err)
			}
			inner := plan
			if tc.mixed {
				project, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("mixed inverted root=%T, want canonical Project", plan)
				}
				if len(project.Projections) != len(metricRoles(s)) || !project.RowType().Equal(chplan.Schema{Columns: metricRoles(s)}) {
					t.Fatalf("mixed inverted output=%#v, want exactly canonical float quartet", project.RowType())
				}
				name, ok := project.Projections[0].Expr.(*chplan.LitString)
				if !ok || name.V != "" || project.Projections[0].Alias != s.MetricNameColumn {
					t.Fatalf("name projection=%#v", project.Projections[0])
				}
				for index, column := range []string{s.AttributesColumn, s.TimestampColumn, s.ValueColumn} {
					projection := project.Projections[index+1]
					ref, ok := projection.Expr.(*chplan.ColumnRef)
					if !ok || ref.Name != column || ref.Qualifier != "" || chplan.ProjectionOutputName(projection) != column {
						t.Fatalf("projection %d=%#v, want passthrough %s", index+1, projection, column)
					}
				}
				inner = project.Input
			}
			empty, ok := inner.(*chplan.Filter)
			if !ok {
				t.Fatalf("empty node=%T, want Filter(false)", inner)
			}
			predicate, ok := empty.Predicate.(*chplan.LitBool)
			if !ok || predicate.V {
				t.Fatalf("empty predicate=%#v, want false", empty.Predicate)
			}
			if tc.mixed {
				narrow, ok := empty.Input.(*chplan.Filter)
				if !ok || !chplan.IsMixedFloatNarrowing(narrow) {
					t.Fatalf("empty input=%T, want strict float discriminator Filter", empty.Input)
				}
				union, ok := narrow.Input.(*chplan.VectorSetOp)
				if !ok || !union.Mixed {
					t.Fatalf("narrow input=%T, want existing mixed union (no shadow-arm replacement/extra guard)", narrow.Input)
				}
			}
		})
	}
}

func TestMathFamilyNestedInvertedLiteralPreservesOperand(t *testing.T) {
	for _, union := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist"} {
		for _, sort := range []string{"sort_by_label", "sort_by_label_desc"} {
			operand := sort + "(" + union + ", \"job\")"
			t.Run(operand, func(t *testing.T) {
				original, err := mathFamilyContractLower(t, operand)
				if err != nil || chplan.RowShapeOf(original) != chplan.MixedRowShape {
					t.Fatalf("operand must independently lower to Mixed: %T, %v", original, err)
				}
				plan, err := mathFamilyContractLower(t, "clamp("+operand+", 2, 1)")
				if err != nil {
					t.Fatal(err)
				}
				empty, ok := plan.(*chplan.Filter)
				if !ok {
					t.Fatalf("terminal empty root=%T, want original Filter(false)", plan)
				}
				predicate, ok := empty.Predicate.(*chplan.LitBool)
				if !ok || predicate.V || !reflect.DeepEqual(empty.Input, original) {
					t.Fatalf("terminal empty must preserve original Mixed operand without payload narrowing: %#v", empty)
				}
			})
		}
	}
}
