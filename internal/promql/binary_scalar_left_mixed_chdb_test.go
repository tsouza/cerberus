//go:build chdb

package promql_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

func TestMixedScalarLeftComparisonBaseline_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, foSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	type sample struct {
		Name  string
		Value float64
	}
	cases := []struct {
		name string
		op   string
		want map[string]sample
	}{
		{"less_filter", "<", map[string]sample{"f2": {foFloatMetric, 9}}},
		{"greater_filter", ">", map[string]sample{"f1": {foFloatMetric, 3}, "f3": {foFloatMetric, 1}}},
		{"less_bool", "< bool", map[string]sample{"f1": {"", 0}, "f2": {"", 1}, "f3": {"", 0}}},
		{"greater_bool", "> bool", map[string]sample{"f1": {"", 1}, "f2": {"", 0}, "f3": {"", 1}}},
	}
	for _, orientation := range []struct{ name, expr string }{
		{"hist_first", foHistMetric + " or " + foFloatMetric},
		{"float_first", foFloatMetric + " or " + foHistMetric},
	} {
		for _, tc := range cases {
			t.Run(orientation.name+"/"+tc.name, func(t *testing.T) {
				query := "5 " + tc.op + " (" + orientation.expr + ")"
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
				if err != nil {
					t.Fatalf("LowerAt(%s): %v", query, err)
				}
				sqlText, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				rows := fixture.queryOverEmitted(t, "`Attributes`['series'], `MetricName`, `Value`", sqlText, args)
				defer func() { _ = rows.Close() }()
				got := map[string]sample{}
				for rows.Next() {
					var series string
					var value sample
					if err := rows.Scan(&series, &value.Name, &value.Value); err != nil {
						t.Fatal(err)
					}
					if _, exists := got[series]; exists {
						t.Fatalf("duplicate series %q", series)
					}
					got[series] = value
				}
				if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("query %s: got %#v; want %#v", query, got, tc.want)
				}
			})
		}
	}
}

func TestMixedScalarLeftComparisonShadowBaseline_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, tkShadowSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	type sample struct {
		Name  string
		Value float64
	}
	cases := []struct {
		name, op, expr string
		want           map[string]sample
	}{
		{
			"hist_first_filter", "<", tkShadowHistMetric + " or " + tkShadowFloatMetric,
			map[string]sample{"solo": {tkShadowFloatMetric, 7}},
		},
		{
			"float_first_filter", "<", tkShadowFloatMetric + " or " + tkShadowHistMetric,
			map[string]sample{"solo": {tkShadowFloatMetric, 7}, "dup": {tkShadowFloatMetric, 42}},
		},
		{
			"hist_first_bool", "< bool", tkShadowHistMetric + " or " + tkShadowFloatMetric,
			map[string]sample{"solo": {"", 1}},
		},
		{
			"float_first_bool", "< bool", tkShadowFloatMetric + " or " + tkShadowHistMetric,
			map[string]sample{"solo": {"", 1}, "dup": {"", 1}},
		},
		{
			"hist_first_false_bool", "> bool", tkShadowHistMetric + " or " + tkShadowFloatMetric,
			map[string]sample{"solo": {"", 0}},
		},
		{
			"float_first_false_bool", "> bool", tkShadowFloatMetric + " or " + tkShadowHistMetric,
			map[string]sample{"solo": {"", 0}, "dup": {"", 0}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := "5 " + tc.op + " (" + tc.expr + ")"
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%s): %v", query, err)
			}
			sqlText, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			rows := fixture.queryOverEmitted(t, "`Attributes`['series'], `MetricName`, `Value`", sqlText, args)
			defer func() { _ = rows.Close() }()
			got := map[string]sample{}
			for rows.Next() {
				var series string
				var value sample
				if err := rows.Scan(&series, &value.Name, &value.Value); err != nil {
					t.Fatal(err)
				}
				if _, exists := got[series]; exists {
					t.Fatalf("duplicate series %q", series)
				}
				got[series] = value
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("query %s: got %#v; want %#v", query, got, tc.want)
			}
		})
	}
}
