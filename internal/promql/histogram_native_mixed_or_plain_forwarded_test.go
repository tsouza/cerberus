package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_PlainSideForwardedThroughSetOpRejectsCleanly pins
// a pre-release audit finding: vectorPlainArithmeticOverMixedExpHistogramSetOp
// / comparisonVectorPlainOverMixedExpHistogramSetOp guarded their "plain
// side" argument with the narrower isExpHistogramValuedShape instead of
// the wider isExpHistogramValuedOrForwarded (cerberus issue #2571) that
// mixedExpHistogramSetOp/lowerMixedExpHistogramOperands use precisely so
// they can see an and/unless-forwarded histogram-valued operand.
//
// `(a or b) * (hist_x and other_selector)` where hist_x is exp-histogram-
// valued has its plain side ((hist_x and other_selector), forwarding
// hist_x's histogram value through the `and`) silently mis-classified as
// an ordinary float vector at RECOGNITION time — this recognizer would
// then accept the shape and hand it to lowerPlainOperandForMixedJoin,
// which only catches the mistake LATE via its own row-shape guard,
// erroring on the resulting Histogram-shaped node with a DIFFERENT
// message than the one binary.go's ordinary lowerVectorSetOp rejection
// raises for every other unattempted histogram/mixed combination.
//
// This test pins that the error message now comes from the EARLY,
// shared rejection path (binary.go's lowerVectorSetOp) rather than the
// late, recognizer-specific one — proving the recognizer itself now
// correctly excludes this shape instead of accepting it and failing
// downstream.
func TestLower_ExpHistogram_PlainSideForwardedThroughSetOpRejectsCleanly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const mixedOr = `demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)`
	const forwardedPlain = `demo_latency_exp_hist and some_float_metric`

	cases := []struct {
		name  string
		query string
	}{
		{name: "arithmetic", query: "(" + mixedOr + ") * (" + forwardedPlain + ")"},
		{name: "comparison", query: "(" + mixedOr + ") < (" + forwardedPlain + ")"},
	}

	// This exact message is binary.go's lowerVectorSetOp's own generic
	// rejection for a shape none of the mixed-or wrapper families
	// attempt — the signal that the recognizer excluded this shape up
	// front rather than accepting it and failing inside
	// lowerPlainOperandForMixedJoin's late row-shape guard (whose message
	// reads "a mixed float/histogram 'or' operand paired with a
	// %s-shaped operand is not supported" instead).
	const wantErrSubstring = "'or' between a float-valued and a histogram-valued operand is not supported"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)
			_, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err == nil {
				t.Fatalf("Lower(%q): expected an error (this shape is not yet attempted), got success", tc.query)
			}
			if !strings.Contains(err.Error(), wantErrSubstring) {
				t.Fatalf("Lower(%q) error = %q, want it to contain %q (the early, shared rejection — a different message here means the late lowerPlainOperandForMixedJoin guard caught it instead, i.e. the recognizer regressed)", tc.query, err.Error(), wantErrSubstring)
			}
		})
	}
}
