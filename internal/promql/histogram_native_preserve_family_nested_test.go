package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_PreserveFamilyNestedUnderFloatOnlyWrapper pins a
// pre-release audit finding that did NOT reproduce against this branch:
// the claim was that lowerAggregate retries the "drop" family
// (lowerExpHistogramDroppingShape) and count/group
// (lowerExpHistogramCountFamily) for a nested wrapper but has no
// equivalent retry for the "preserve" family (sum()/avg()), so
// `abs(sum(rate(m_exp_hist[5m])))` would fall through to
// expHistogramSelectorRouting's rejection instead of composing the way
// `sum(rate(m_exp_hist[5m]))` does at the query root.
//
// Verified directly: it already composes. lowerExpHistogramArgAsCanonicalFloat
// (the shared opt-in every float-only wrapper — abs() included — threads
// before its own generic lower() fallback) calls lowerExpHistogramValuedShape
// on its argument BEFORE ever reaching lowerAggregate's fallback path;
// lowerExpHistogramValuedShape's own mergeableExpHistogramAggregate branch
// (histogram_native_float_fn.go) recognises ANY mergeable SUM/AVG
// aggregation regardless of nesting depth and recurses into its OWN
// operand via the same function — so `sum(...)` nested under `abs()`,
// under a further `sum()`/`sum by(...)`, under `topk()`/`min()`/`count()`/
// `sort()`, all compose without ever needing lowerAggregate's own generic
// `lower(a.Expr, ...)` fallback to understand histograms at all. This test
// pins that composition so a future regression here is caught.
func TestLower_ExpHistogram_PreserveFamilyNestedUnderFloatOnlyWrapper(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{name: "abs over sum", query: `abs(sum(rate(latency_exp_hist[5m])))`},
		{name: "abs over avg", query: `abs(avg(rate(latency_exp_hist[5m])))`},
		{name: "sqrt over sum by", query: `sqrt(sum by (service) (rate(latency_exp_hist[5m])))`},
		{name: "topk over sum", query: `topk(1, sum(rate(latency_exp_hist[5m])))`},
		{name: "min over sum", query: `min(sum(rate(latency_exp_hist[5m])))`},
		{name: "count over sum", query: `count(sum(rate(latency_exp_hist[5m])))`},
		{name: "sort over sum", query: `sort(sum(rate(latency_exp_hist[5m])))`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("Lower(%q): %v (expected this nested preserve-family shape to compose)", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Errorf("Lower(%q) RowShape = %s, want %s", tc.query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Errorf("Emit(%q): %v", tc.query, err)
			}
		})
	}
}
