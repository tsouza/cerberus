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

// TestLower_ExpHistogram_MixedOrSubqueryFoldFn_OmittedStepDefaults kills
// the CONDITIONALS_BOUNDARY mutant on
// lowerSumOrAvgOverMixedOrSubqueryFoldFn's
//
//	if sub.Step < 0 {
//		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", sub.Step)
//	}
//	step := sub.Step
//	if step == 0 {
//		step = defaultSubqueryStep
//	}
//
// with `<` rewritten to `<=`. Every existing untagged case of this shape
// (TestLower_ExpHistogram_MixedOrSubqueryFoldFn_SumWrapped, in the
// sibling _test.go file) writes an EXPLICIT subquery step
// (`[5m:1m]`), so none of them ever reaches this function with
// `sub.Step == 0` — the one value the boundary mutation actually moves.
//
// A bare `[5m:]` subquery — step omitted, the standard PromQL spelling
// for "use the default step" — parses with `sub.Step == 0`. Under the
// original `<`, zero is not negative, so the explicit-step rejection is
// skipped and the immediately following `if step == 0` substitutes
// [defaultSubqueryStep]: the query lowers successfully. Under the
// mutated `<=`, zero satisfies the (now inclusive) bound and the query
// is wrongly rejected as if a NEGATIVE step had been written, even
// though omitting the step is legal PromQL syntax with a well-defined
// default.
func TestLower_ExpHistogram_MixedOrSubqueryFoldFn_OmittedStepDefaults(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	at := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	const query = `sum(rate((latency_exp_hist or latency_float)[5m:]))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error (an omitted subquery step must default, not reject): %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.MixedRowShape)
	}
}
