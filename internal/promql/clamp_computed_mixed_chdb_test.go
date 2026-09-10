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

// The shared seed contains a colliding histogram/float labelset (dup) and an
// unshadowed float (solo). Hist-first or must shadow the real dup float before
// clamp drops the histogram; reading its placeholder Value would invent dup=0.
func TestComputedClampOverNestedMixed_Parity_ChDB(t *testing.T) {
	const (
		fixturePermissions = 0o600
		onlySolo           = `[["",{"series":"solo"},"2026-01-01T00:00:00Z",7]]`
		bothFloats         = `[["",{"series":"dup"},"2026-01-01T00:00:00Z",10],["",{"series":"solo"},"2026-01-01T00:00:00Z",7]]`
	)
	for _, tc := range []struct {
		name      string
		histFirst bool
		inverted  bool
		wantRows  string
	}{
		{"hist_first_normal", true, false, onlySolo},
		{"float_first_normal", false, false, bothFloats},
		{"hist_first_inverted", true, true, "[]"},
		{"float_first_inverted", false, true, "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := tkShadowFloatMetric, tkShadowHistMetric
			if tc.histFirst {
				left, right = right, left
			}
			bounds := "scalar(vector(0)), scalar(vector(10))"
			if tc.inverted {
				bounds = "scalar(vector(2)), scalar(vector(1))"
			}
			query := fmt.Sprintf(`clamp(sort_by_label(%s or %s, "series"), %s)`, left, right, bounds)
			archive := &txtar.Archive{Files: []txtar.File{
				{Name: "query.promql", Data: []byte(query + "\n")},
				{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: /api/v1/query\nscope: full\n")},
				{Name: "seed", Data: []byte(tkShadowSeed)},
				{Name: "expected_rows", Data: []byte(tc.wantRows + "\n")},
			}}
			path := filepath.Join(t.TempDir(), "computed_clamp.txtar")
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
			sqlText, args, err := chsql.Emit(context.Background(), optimized)
			if err != nil {
				t.Fatal(err)
			}
			rows := spec.RunRoundTripSQL(t, fixture, sqlText, args)
			spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS}, rows)
		})
	}
}
