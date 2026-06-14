package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Info covers PromQL's `info(v[, {matchers}])` label-enrichment
// join. `info` is experimental, so the parser must enable experimental
// functions. The lowering builds a chplan.InfoJoin keyed on
// {instance, job}; the chsql emitter renders a LEFT JOIN whose output
// keeps the base side's value/timestamp and grows its Attributes with the
// info series' data labels via mapConcat.
func TestLower_Info(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name      string
		query     string
		wantInSQL []string
	}{
		{
			name:  "default target_info enrichment",
			query: `info(up)`,
			wantInSQL: []string{
				"LEFT JOIN",
				"L.`Attributes`[?] = R.`Attributes`[?]",
				"mapConcat(",
				"mapFilter((k, v) -> NOT (k IN (?, ?, ?))",
				"L.`Value` AS `Value`",
				"L.`MetricName` AS `MetricName`",
			},
		},
		{
			name:  "second-arg name matcher selects info metric",
			query: `info(up, {__name__="build_info"})`,
			wantInSQL: []string{
				"LEFT JOIN",
				"mapConcat(",
			},
		},
		{
			name:  "second-arg data-label matcher restricts copied labels",
			query: `info(up, {version=~".+"})`,
			wantInSQL: []string{
				"LEFT JOIN",
				"mapFilter((k, v) -> k IN (?)",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for _, want := range tc.wantInSQL {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
		})
	}
}
