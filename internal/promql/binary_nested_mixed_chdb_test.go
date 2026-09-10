//go:build chdb

package promql_test

import (
	"context"
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

// Hist-first union shadows dup=42 before comparison removes the histogram.
// Non-bool comparisons retain the vector's name/value, even with a scalar LHS.
func TestNestedMixedComparisonParity_ChDB(t *testing.T) {
	const (
		fixturePermissions = 0o600
		solo               = `["tk_shadow_float_side_gauge",{"series":"solo"},"2026-01-01T00:00:00Z",7,0,0,0,0,0,0,[],0,[],0]`
		dup                = `["tk_shadow_float_side_gauge",{"series":"dup"},"2026-01-01T00:00:00Z",42,0,0,0,0,0,0,[],0,[],0]`
		boolSolo           = `["",{"series":"solo"},"2026-01-01T00:00:00Z",0]`
		boolDup            = `["",{"series":"dup"},"2026-01-01T00:00:00Z",0]`
	)
	for _, histFirst := range []bool{true, false} {
		left, right := tkShadowFloatMetric, tkShadowHistMetric
		if histFirst {
			left, right = right, left
		}
		vector := fmt.Sprintf(`sort_by_label(%s or %s, "series")`, left, right)
		nonempty, boolRows := "["+solo+"]", "["+boolSolo+"]"
		if !histFirst {
			nonempty, boolRows = "["+dup+","+solo+"]", "["+boolDup+","+boolSolo+"]"
		}
		for _, tc := range []struct{ name, query, want string }{
			{"literal_right_empty", vector + " == 0", "[]"},
			{"literal_left_empty", "0 == " + vector, "[]"},
			{"computed_right_empty", vector + " == scalar(vector(0))", "[]"},
			{"computed_left_empty", "scalar(vector(0)) == " + vector, "[]"},
			{"literal_right_values", vector + " < 50", nonempty},
			{"literal_left_values", "50 > " + vector, nonempty},
			{"computed_right_values", vector + " < scalar(vector(50))", nonempty},
			{"computed_left_values", "scalar(vector(50)) > " + vector, nonempty},
			{"literal_bool", vector + " == bool 0", boolRows},
			{"computed_bool", "scalar(vector(0)) == bool " + vector, boolRows},
		} {
			t.Run(fmt.Sprintf("hist_first_%t/%s", histFirst, tc.name), func(t *testing.T) {
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query\nscope: full\n")},
					{Name: "seed", Data: []byte(tkShadowSeed)},
					{Name: "expected_rows", Data: []byte(tc.want + "\n")},
				}}
				path := filepath.Join(t.TempDir(), "nested_comparison.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
				expr, err := p.ParseExpr(tc.query)
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
}
