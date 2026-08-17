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

// TestLower_ExpHistogram_MixedSetOpOr pins cerberus issue #2330: `or`
// between a float-valued operand (histogram_quantile's output) and a
// raw histogram-valued selector lowers successfully — in BOTH source
// orders — to a *chplan.VectorSetOp whose Mixed flag is set and whose
// MixedHistogramOnLeft field names which side is which.
func TestLower_ExpHistogram_MixedSetOpOr(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		query         string
		histogramLeft bool
		leftIsFloat   bool
	}{
		{
			name:          "float left",
			query:         `histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist`,
			histogramLeft: false,
			leftIsFloat:   true,
		},
		{
			name:          "histogram left",
			query:         `latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)`,
			histogramLeft: true,
			leftIsFloat:   false,
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
			if setOp.Histogram {
				t.Fatalf("lower(%q): VectorSetOp.Histogram = true, want false (Mixed and Histogram are mutually exclusive)", tc.query)
			}
			if setOp.MixedHistogramOnLeft != tc.histogramLeft {
				t.Fatalf("lower(%q): MixedHistogramOnLeft = %v, want %v", tc.query, setOp.MixedHistogramOnLeft, tc.histogramLeft)
			}
			leftIsHistogramProjection := false
			if _, ok := setOp.Left.(*chplan.HistogramProjection); ok {
				leftIsHistogramProjection = true
			}
			if leftIsHistogramProjection == tc.leftIsFloat {
				t.Fatalf("lower(%q): Left is %T, histogramLeft=%v mismatch", tc.query, setOp.Left, tc.histogramLeft)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_WindowedFloatSideRejects pins the
// deliberate scope line lowerMixedExpHistogramSetOp draws: a float side
// that is itself a windowed / derived shape (here, a range-vector
// `rate()` over a float metric) is not yet reconciled against the
// histogram side's row shape, so the query is rejected with a clear
// error rather than silently mis-projected. This is not a regression —
// the histogram side of this exact query was ALREADY rejected before
// this file existed (expHistogramSelectorRouting's catch-all).
func TestLower_ExpHistogram_MixedSetOpOr_WindowedFloatSideRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `rate(up[5m]) or latency_exp_hist`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}
