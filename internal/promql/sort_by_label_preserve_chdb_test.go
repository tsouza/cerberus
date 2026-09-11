//go:build chdb

package promql_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

func TestSortByLabelPreserveFixtureParity_ChDB(t *testing.T) {
	for _, name := range []string{
		"mixed_wrapper_sort_by_label", "mixed_wrapper_sort_by_label_mirror",
		"sort_by_label_mixed_or_mirror", "sort_by_label_desc_mixed_or_mirror",
	} {
		t.Run(name, func(t *testing.T) {
			fixture, err := spec.Load(filepath.Join("..", "..", "test", "spec", "promql", name+".txtar"))
			if err != nil {
				t.Fatal(err)
			}
			query, ok := fixture.Section("query.promql")
			if !ok {
				t.Fatal("missing PromQL query")
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
			sqlText, args, err := chsql.Emit(context.Background(), optimized)
			if err != nil {
				t.Fatal(err)
			}
			rows := spec.RunRoundTripSQL(t, fixture, sqlText, args)
			spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS}, rows)
		})
	}
}
