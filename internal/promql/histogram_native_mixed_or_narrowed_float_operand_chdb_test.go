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

// nfNarrowed is a mixed relation nested under a label-sort wrapper, the
// operand every query in [TestMixedOrNarrowedFloatArmParity_ChDB]
// float-narrows. Over [woSeed] it answers the histograms b="h" and b="d"
// and the float b="f" (7).
const nfNarrowed = `sort_by_label(` + woMixedOr + `, "b")`

// TestMixedOrNarrowedFloatArmParity_ChDB pins cerberus issue #3853's shapes
// against the Prometheus reference engine: a float-narrowing consumer (a
// comparison, sort, or empty clamp) over a nested mixed relation answers,
// as the float arm of a further mixed `or` and as the plain operand of a
// mixed vector join, the rows Prometheus answers, in instant and range mode.
func TestMixedOrNarrowedFloatArmParity_ChDB(t *testing.T) {
	const fixturePermissions = 0o600
	var (
		hH = woHist(woHistMetric, map[string]string{"b": "h"}, 3, 3)
		hD = woHist(woHistMetric, map[string]string{"b": "d"}, 5, 5)
		fF = woSelected(woFloatMetric, map[string]string{"b": "f"}, 7)
	)
	cases := []struct {
		name  string
		query string
		rows  []woRow
	}{
		// The three shapes cerberus issue #3853 lists.
		{"histogram_or_sort", woHistMetric + ` or sort(` + nfNarrowed + `)`, []woRow{hH, hD, fF}},
		{"comparison_or_histogram", `(` + nfNarrowed + ` > 1) or ` + woHistMetric, []woRow{fF, hH, hD}},
		{"empty_clamp_or_histogram", `clamp(` + nfNarrowed + `, 2, 1) or ` + woHistMetric, []woRow{hH, hD}},
		// Siblings: the other row-filtering float narrowings, a float arm
		// under the aggregate recognizer, and the plain operand of a mixed
		// vector join. Prometheus refuses histogram + float, so only the
		// float pair joins.
		{"histogram_or_sort_desc", woHistMetric + ` or sort_desc(` + nfNarrowed + `)`, []woRow{hH, hD, fF}},
		{"inverted_comparison_or_histogram", `(1 < ` + nfNarrowed + `) or ` + woHistMetric, []woRow{fF, hH, hD}},
		{"sum_by_comparison_or_histogram", `sum by (b) ((` + nfNarrowed + ` > 1) or ` + woHistMetric + `)`, []woRow{
			woFloat("", map[string]string{"b": "f"}, 7),
			woHist("", map[string]string{"b": "h"}, 3, 3),
			woHist("", map[string]string{"b": "d"}, 5, 5),
		}},
		{"mixed_plus_sort", woMixedOr + ` + on(b) sort(` + nfNarrowed + `)`, []woRow{
			woSelected("", map[string]string{"b": "f"}, 14),
		}},
		{"comparison_plus_mixed", `(` + nfNarrowed + ` > 1) + on(b) ` + woMixedOr, []woRow{
			woSelected("", map[string]string{"b": "f"}, 14),
		}},
	}
	for _, step := range []time.Duration{0, time.Second} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/step=%s", tc.name, step), func(t *testing.T) {
				times := []time.Time{foEvalTS}
				endpoint := "/api/v1/query"
				if step > 0 {
					times = append(times, foEvalTS.Add(step))
					endpoint = "/api/v1/query_range"
				}
				cells := [][]any{}
				for _, at := range times {
					for _, r := range tc.rows {
						cells = append(cells, woCell(r, at, step == 0, true))
					}
				}
				expected, err := json.Marshal(cells)
				if err != nil {
					t.Fatal(err)
				}
				archive := &txtar.Archive{Files: []txtar.File{
					{Name: "query.promql", Data: []byte(tc.query + "\n")},
					{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
					{Name: "seed", Data: []byte(woSeed)},
					{Name: "expected_rows", Data: expected},
				}}
				path := filepath.Join(t.TempDir(), "mixed_or_narrowed_float_arm.txtar")
				if err := os.WriteFile(path, txtar.Format(archive), fixturePermissions); err != nil {
					t.Fatal(err)
				}
				fixture, err := spec.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(tc.query)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS.Add(step), step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
				}
				plan = spec.AssertScanTimeBoundAccepts(t, plan)
				sql, args, err := chsql.Emit(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				actual := spec.RunRoundTripSQL(t, fixture, sql, args)
				spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS.Add(step), Step: step}, actual)
			})
		}
	}
}
