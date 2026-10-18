package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// mixedOrPairingInner is a mixed float/histogram `or`: a native histogram
// selector unioned with a float histogram_quantile() over it.
const mixedOrPairingInner = `(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`

// mixedOrPairingRangeStep is the step the range-mode half of
// TestVectorSetOrMixedOperandPairings lowers each pairing at.
const mixedOrPairingRangeStep = time.Minute

// mixedOrPairingRangeWidth is the width of that range query's window.
const mixedOrPairingRangeWidth = 5 * time.Minute

// TestVectorSetOrMixedOperandPairings pins every pairing of a Mixed `or`
// operand against each of the three row shapes an outer `or` can meet —
// histogram, float, and another Mixed — on both sides. Each one lowers to
// a Mixed VectorSetOr whose arms stay in source order and keep their own
// sample kinds, in instant and in range mode. The histogram pairings were
// the ones that failed: the mixed-`or` recognizer read the Mixed arm as the
// float side of a histogram/float pair and lowered it through the
// float-only operand path.
func TestVectorSetOrMixedOperandPairings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		query     string
		leftKind  chplan.SampleKind
		rightKind chplan.SampleKind
	}{
		{
			name:      "mixed_or_histogram",
			query:     mixedOrPairingInner + ` or latency_exp_hist`,
			leftKind:  chplan.SampleKindMixed,
			rightKind: chplan.SampleKindHistogram,
		},
		{
			name:      "histogram_or_mixed",
			query:     `latency_exp_hist or ` + mixedOrPairingInner,
			leftKind:  chplan.SampleKindHistogram,
			rightKind: chplan.SampleKindMixed,
		},
		{
			name:      "mixed_or_float",
			query:     mixedOrPairingInner + ` or up`,
			leftKind:  chplan.SampleKindMixed,
			rightKind: chplan.SampleKindFloat,
		},
		{
			name:      "float_or_mixed",
			query:     `up or ` + mixedOrPairingInner,
			leftKind:  chplan.SampleKindFloat,
			rightKind: chplan.SampleKindMixed,
		},
		{
			name:      "mixed_or_mixed",
			query:     mixedOrPairingInner + ` or ` + mixedOrPairingInner,
			leftKind:  chplan.SampleKindMixed,
			rightKind: chplan.SampleKindMixed,
		},
		{
			// The outer left operand is itself the Mixed-or-histogram
			// pairing, so the fix has to hold one nesting level down.
			name:      "nested_mixed_or_histogram_or_histogram",
			query:     `(` + mixedOrPairingInner + ` or latency_exp_hist) or latency_exp_hist`,
			leftKind:  chplan.SampleKindMixed,
			rightKind: chplan.SampleKindHistogram,
		},
	}

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	modes := []struct {
		name  string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{"instant", func(e parser.Expr) (chplan.Node, error) {
			return LowerAt(context.Background(), e, schema.DefaultOTelMetrics(), at, at)
		}},
		{"range", func(e parser.Expr) (chplan.Node, error) {
			return LowerAtRange(context.Background(), e, schema.DefaultOTelMetrics(),
				at.Add(-mixedOrPairingRangeWidth), at, mixedOrPairingRangeStep)
		}},
	}

	for _, tc := range cases {
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				plan, err := mode.lower(mustParseExperimental(t, tc.query))
				if err != nil {
					t.Fatalf("lower(%q): %v", tc.query, err)
				}
				op, ok := plan.(*chplan.VectorSetOp)
				if !ok {
					t.Fatalf("lower(%q): plan = %T, want *chplan.VectorSetOp", tc.query, plan)
				}
				if op.Op != chplan.VectorSetOr {
					t.Errorf("Op = %v, want VectorSetOr", op.Op)
				}
				if !op.Mixed {
					t.Errorf("Mixed = false, want true")
				}
				if op.Histogram {
					t.Errorf("Histogram = true, want false for a Mixed set op")
				}
				// MixedHistogramOnLeft only names which pure arm is the
				// histogram one when the set op builds Mixed from a float
				// arm and a histogram arm; with an already-Mixed arm the
				// emitter classifies each arm by its own sample kind.
				if op.MixedHistogramOnLeft {
					t.Errorf("MixedHistogramOnLeft = true, want false when an arm is already Mixed")
				}
				if got := chplan.LiveSampleKind(op); got != chplan.SampleKindMixed {
					t.Errorf("LiveSampleKind(plan) = %v, want %v", got, chplan.SampleKindMixed)
				}
				if got := chplan.LiveSampleKind(op.Left); got != tc.leftKind {
					t.Errorf("LiveSampleKind(Left) = %v, want %v (arms must stay in source order)", got, tc.leftKind)
				}
				if got := chplan.LiveSampleKind(op.Right); got != tc.rightKind {
					t.Errorf("LiveSampleKind(Right) = %v, want %v (arms must stay in source order)", got, tc.rightKind)
				}
			})
		}
	}
}

// TestMixedExpHistogramSetOpDeclinesMixedOtherSide pins the recognizer's
// classification on its own. A histogram-valued side paired with a plain
// float is the float/histogram pair [mixedExpHistogramSetOp] builds Mixed
// from; paired with an operand that is already a live mixed relation it is
// not, and the recognizer declines on either side. [isMixedRelationShape]
// reports every one of these shapes as mixed, because each lowers to a
// Mixed relation whichever path builds it.
func TestMixedExpHistogramSetOpDeclinesMixedOtherSide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		query     string
		wantMatch bool
	}{
		{"histogram_or_float", `latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)`, true},
		{"float_or_histogram", `histogram_quantile(0.5, latency_exp_hist) or latency_exp_hist`, true},
		{"mixed_or_histogram", mixedOrPairingInner + ` or latency_exp_hist`, false},
		{"histogram_or_mixed", `latency_exp_hist or ` + mixedOrPairingInner, false},
		{"label_wrapped_mixed_or_histogram", `label_replace(` + mixedOrPairingInner + `, "a", "$1", "b", "(.*)") or latency_exp_hist`, false},
	}

	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := mustParseExperimental(t, tc.query)
			if _, got := mixedExpHistogramSetOp(expr, s, ctx); got != tc.wantMatch {
				t.Errorf("mixedExpHistogramSetOp(%q) matched = %v, want %v", tc.query, got, tc.wantMatch)
			}
			if !isMixedRelationShape(expr, s, ctx) {
				t.Errorf("isMixedRelationShape(%q) = false, want true", tc.query)
			}
		})
	}
}
