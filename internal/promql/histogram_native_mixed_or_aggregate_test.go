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

// TestLower_ExpHistogram_MixedSetOpOr_SumWrapped pins cerberus issue
// #2346: `sum`/`avg` [by/without] directly wrapping a mixed float/
// histogram `or` (histogram_native_mixed_or.go's own #2330/#2335 shape)
// now lowers successfully instead of falling through to
// internal/promql/binary.go's lowerVectorSetOp rejection
// ("'or' between a float-valued and a histogram-valued operand is not
// supported"). The representative query from the issue is included
// verbatim.
func TestLower_ExpHistogram_MixedSetOpOr_SumWrapped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "issue representative query",
			query: `sum(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`,
		},
		{
			name:  "avg, float on left",
			query: `avg(histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist)`,
		},
		{
			name:  "sum by (...)",
			query: `sum by (service) (latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`,
		},
		{
			name:  "sum without (...)",
			query: `sum without (instance) (latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`,
		},
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
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			// The plan root must still answer MixedRowShape — the API
			// layer's wrapWithSampleProjection (internal/api/prom/handler.go)
			// relies on RowShapeOf, not on the root being literally a
			// *chplan.VectorSetOp, to decide the wire projection is
			// already complete.
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.MixedRowShape)
			}
			setOp, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", tc.query, plan)
			}
			if !setOp.Mixed {
				t.Fatalf("lower(%q): VectorSetOp.Mixed = false, want true", tc.query)
			}
			if setOp.Op != chplan.VectorSetOr {
				t.Fatalf("lower(%q): VectorSetOp.Op = %v, want %v", tc.query, setOp.Op, chplan.VectorSetOr)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_WindowedFloatSide pins
// cerberus issue #2453: wrapping the mixed `or` in `sum`/`avg(...)` now
// composes with a windowed/derived float arm too — the intersection of
// #2333 (windowed float side, root-only leaf recognizer) and #2346
// (sum/avg-wrapped composition, plain float side only) that neither of
// those issues covered on its own. Both orderings of the `or` operands
// are covered because histogram_native_mixed_or_aggregate.go's own
// shadow-resolution step canonicalises each side through a DIFFERENT
// mechanism depending on whether it is LHS (kept unconditionally) or RHS
// (shadow-filtered) in the source AST — see canonicalizeFloatArmForAgg's
// doc comment.
func TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_WindowedFloatSide(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "issue representative query, windowed float on left",
			query: `sum(rate(up[5m]) or demo_latency_exp_hist)`,
		},
		{
			name:  "windowed float on right",
			query: `avg(demo_latency_exp_hist or rate(up[5m]))`,
		},
		{
			name:  "range mode, windowed float on left",
			query: `sum(rate(up[5m]) or latency_exp_hist)`,
		},
		{
			name:  "range mode, windowed float on right, by(...)",
			query: `sum by (service) (latency_exp_hist or rate(up[5m]))`,
		},
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
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.MixedRowShape)
			}
			setOp, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", tc.query, plan)
			}
			if !setOp.Mixed {
				t.Fatalf("lower(%q): VectorSetOp.Mixed = false, want true", tc.query)
			}
			if setOp.Op != chplan.VectorSetOr {
				t.Fatalf("lower(%q): VectorSetOp.Op = %v, want %v", tc.query, setOp.Op, chplan.VectorSetOr)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_RangeMode pins that
// TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_WindowedFloatSide's
// range-mode cases (ctx.step > 0) also lower successfully — the
// windowed float arm's canonicalisation differs by mode
// (canonicalizeFloatArmForAgg passes the matrix RangeWindow's own
// Timestamp column through directly in range mode instead of
// synthesising an instant anchor), so range mode needs its own coverage
// rather than trusting the instant-mode case above to stand in for it.
func TestLower_ExpHistogram_MixedSetOpOr_SumWrapped_RangeMode(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	const step = time.Minute

	query := `sum(rate(up[5m]) or demo_latency_exp_hist)`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): unexpected error: %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.MixedRowShape)
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_AbsWrappedStillRejects pins that
// this file's new recognizer is scoped to `sum`/`avg` only: any OTHER
// wrapper around a mixed `or` (cerberus issue #2346 names `abs(a or b)`
// explicitly as staying out of scope) keeps falling through to the
// pre-existing rejection, unchanged.
func TestLower_ExpHistogram_MixedSetOpOr_AbsWrappedStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `abs(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}
