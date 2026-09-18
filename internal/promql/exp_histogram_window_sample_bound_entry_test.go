package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Every exported lowering entry point that can build an exponential-
// histogram window resolves its ResourceBounds through the same defaults,
// so the window pre-rejection is present on all of them — including the
// ones that never take a LowerOpts. A zero ceiling is never "omit the
// guard": the guard's ceiling on those paths is the derived one-GiB grant,
// exactly what an unset CERBERUS_CH_QUERY_MAX_MEMORY resolves to.
// LowerMetadataRange is not listed because a metadata lowering never
// takes the native exp-histogram path at all
// (histogram_native_availability.go), so it has no window to guard.
func TestExpHistogramWindowGuardEmittedByEveryEntryPoint(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(`rate(http_server_duration_exp_hist[5m])`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		lower func() (chplan.Node, error)
	}{
		{"Lower", func() (chplan.Node, error) { return promql.Lower(context.Background(), expr, s) }},
		{"LowerAt", func() (chplan.Node, error) { return promql.LowerAt(context.Background(), expr, s, at, at) }},
		{"LowerAtRange", func() (chplan.Node, error) {
			return promql.LowerAtRange(context.Background(), expr, s, at.Add(-time.Minute), at, 15*time.Second)
		}},
		{"LowerAtRangeOpts zero opts", func() (chplan.Node, error) {
			return promql.LowerAtRangeOpts(context.Background(), expr, s, at, at, 0, promql.LowerOpts{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := tc.lower()
			if err != nil {
				t.Fatal(err)
			}
			ceilings := expHistogramWindowGuardCeilings(plan)
			if len(ceilings) == 0 {
				t.Fatalf("%s lowered an exp-histogram window with no sample-cost guard", tc.name)
			}
			want := promql.ExpHistogramWindowCostUnitsForMemory(0)
			for _, got := range ceilings {
				if got != want {
					t.Fatalf("%s: window guard ceiling = %d, want the derived default %d", tc.name, got, want)
				}
			}
		})
	}
}

// expHistogramWindowGuardCeilings returns the ceiling literal of every
// exp-histogram window sample guard in plan: the right operand of the
// `cost > ceiling` comparison inside the guard's throwIf.
func expHistogramWindowGuardCeilings(plan chplan.Node) []int64 {
	var ceilings []int64
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		filter, ok := n.(*chplan.Filter)
		if !ok {
			return true
		}
		eq, ok := filter.Predicate.(*chplan.Binary)
		if !ok {
			return true
		}
		throwIf, ok := eq.Left.(*chplan.FuncCall)
		if !ok || throwIf.Fn != chplan.FnThrowIf || len(throwIf.Args) != 2 {
			return true
		}
		msg, ok := throwIf.Args[1].(*chplan.InlineString)
		if !ok || msg.V != chplan.ExpHistogramWindowSampleBudgetMessage {
			return true
		}
		over, ok := throwIf.Args[0].(*chplan.Binary)
		if !ok {
			return true
		}
		if lit, ok := over.Right.(*chplan.LitInt); ok {
			ceilings = append(ceilings, lit.V)
		}
		return true
	})
	return ceilings
}
