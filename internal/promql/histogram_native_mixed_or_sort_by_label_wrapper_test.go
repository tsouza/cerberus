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

// TestLower_ExpHistogram_WrapperOverMixedSortByLabel pins two pre-release
// audit findings, both rooted in the same gap: a wrapper composed OVER a
// FURTHER wrapper that resolves [chplan.MixedRowShape] — canonically
// `sort_by_label(<mixed float/histogram or>, label)` — is invisible to
// the direct-BinaryExpr mixed-or recognizers (mixedExpHistogramSetOp and
// its per-family siblings), because the mixed `or` sits one level deeper
// than a single BinaryExpr check reaches. That left two failure modes:
//
//   - A single-arg math function, round(v, to_nearest), clamp*, or
//     timestamp() reached instant_fns.go's/date_fns.go's generic lower()
//     fallback, got back a Mixed node, and PANICKED in
//     projectValueOverInner's assertValueShapedInput (finding B).
//   - scalar(...) reached scalar_args.go's generic lower() fallback and
//     silently ran count()/any(Value) over the Mixed union's fourteen
//     columns UNFILTERED — reading the histogram side's placeholder
//     Value or inflating the row count past 1 — instead of answering
//     NaN/the-single-float-sample the way scalar()'s own doc comment
//     promises (finding C).
//
// The fix (mixedRowsFloatOnly, histogram_shape_guard.go) narrows a
// Mixed-shaped generic-lower result to its float-only rows before ANY
// forwarder reads Value off it — wired into guardedValueProjection (every
// caller in finding B's list routes through it) and directly into
// lowerScalarVectorArg (finding C).
func TestLower_ExpHistogram_WrapperOverMixedSortByLabel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const mixedOr = `demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist)`

	cases := []struct {
		name  string
		query string
	}{
		{name: "abs", query: `abs(sort_by_label(` + mixedOr + `, "x"))`},
		{name: "sqrt over sort_by_label_desc", query: `sqrt(sort_by_label_desc(` + mixedOr + `, "x"))`},
		{name: "clamp_max", query: `clamp_max(sort_by_label(` + mixedOr + `, "x"), 5)`},
		{name: "round", query: `round(sort_by_label(` + mixedOr + `, "x"))`},
		{name: "round to_nearest", query: `round(sort_by_label(` + mixedOr + `, "x"), 0.5)`},
		{name: "timestamp", query: `timestamp(sort_by_label(` + mixedOr + `, "x"))`},
		{name: "scalar", query: `scalar(sort_by_label(` + mixedOr + `, "x"))`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}

			// This is the load-bearing assertion: before the fix, every
			// case but scalar() PANICKED inside Lower — a bare call, not
			// a recovered one, so a panic here fails the test with the
			// panic message intact.
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			if got := chplan.RowShapeOf(plan); got != chplan.SampleRowShape {
				t.Errorf("Lower(%q) RowShape = %s, want %s (histogram columns must not leak through)", tc.query, got, chplan.SampleRowShape)
			}

			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Errorf("Emit(%q): %v", tc.query, err)
			}
		})
	}
}
