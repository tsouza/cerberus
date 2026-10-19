package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// wrapperOperandMixedOr is the mixed float/histogram `or` every case in
// [TestLower_MixedOrWrapper_AsSetOpOperand] wraps.
const wrapperOperandMixedOr = `(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist))`

// wrapperOperandLabelReplace is cerberus issue #3823's own wrapper.
const wrapperOperandLabelReplace = `label_replace(` + wrapperOperandMixedOr + `, "a", "$1", "b", "(.*)")`

// TestLower_MixedOrWrapper_AsSetOpOperand pins cerberus issue #3823: a
// wrapper composed directly over a mixed float/histogram `or` lowers as an
// operand of a vector set operator — and as the float arm of a further
// mixed `or` — through the same recognizers the query root runs, instead
// of reaching the wrapper's inner `or` through a non-root lowerVectorSetOp
// that rejects the float/histogram pair. Each case also holds the lowered
// plan's live mixed-ness to [isMixedRelationShape], the static predicate
// the binary-operator recognizers consult, so the two cannot disagree for
// these shapes.
func TestLower_MixedOrWrapper_AsSetOpOperand(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		mixed bool
	}{
		// The five shapes cerberus issue #3823 lists.
		{"label_replace or histogram", wrapperOperandLabelReplace + ` or latency_exp_hist`, true},
		{"histogram or label_replace", `latency_exp_hist or ` + wrapperOperandLabelReplace, true},
		{"label_replace or float", wrapperOperandLabelReplace + ` or up`, true},
		{"label_replace and float", wrapperOperandLabelReplace + ` and up`, true},
		{"sum or float", `sum(` + wrapperOperandMixedOr + `) or up`, true},
		// The remaining recognizer families as a set-op operand.
		{"float unless label_join", `up unless label_join(` + wrapperOperandMixedOr + `, "a", "-", "b")`, false},
		{"scale or float", wrapperOperandMixedOr + ` * 2 or up`, true},
		{"vector comparison or float", `(` + wrapperOperandMixedOr + ` == ` + wrapperOperandMixedOr + `) or up`, true},
		{"vector arithmetic and float", `(` + wrapperOperandMixedOr + ` + up) and up`, true},
		{"topk or float", `topk(2, ` + wrapperOperandMixedOr + `) or up`, false},
		{"count_values or float", `count_values("v", ` + wrapperOperandMixedOr + `) or up`, false},
		{"absent of label_replace", `absent(` + wrapperOperandLabelReplace + `)`, false},
		// A float-narrowing wrapper as the float arm of a further mixed `or`.
		{"count or histogram", `count(` + wrapperOperandMixedOr + `) or latency_exp_hist`, true},
		{"histogram or abs", `latency_exp_hist or abs(` + wrapperOperandMixedOr + `)`, true},
		{"scalar comparison or histogram", `(` + wrapperOperandMixedOr + ` > 1) or latency_exp_hist`, true},
		{"sum of min or histogram", `sum(min(` + wrapperOperandMixedOr + `) or latency_exp_hist)`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			if got := mixedRowsNeedPreparation(plan); got != tc.mixed {
				t.Errorf("%q lowered to a live mixed relation: %v, want %v (plan kind %v)",
					tc.query, got, tc.mixed, chplan.LiveSampleKind(plan))
			}
			if got := isMixedRelationShape(expr, s, lowerCtx{start: at, end: at}); got != tc.mixed {
				t.Errorf("isMixedRelationShape(%q) = %v, want %v", tc.query, got, tc.mixed)
			}
		})
	}
}

// TestLower_MixedOrWrapper_AsSetOpOperand_PolicyGated pins that a wrapper
// recognised in set-op operand position stays under both policy rows that
// govern it: the wrapper's own recognizer row (here label rewrites at the
// root site, which authorizes label_replace over its direct mixed `or`)
// and the set operator's existing-plan row, which authorizes consuming the
// mixed plan the wrapper produced. Removing either rejects with that row's
// key rather than admitting the mixed relation unconditionally.
func TestLower_MixedOrWrapper_AsSetOpOperand_PolicyGated(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	query := wrapperOperandLabelReplace + ` or up`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	for _, key := range []mixedWrapperKey{
		{family: mixedLabelFamily, site: mixedRootAdmission},
		{family: mixedSetOperandFamily, site: mixedPlanAdmission},
	} {
		t.Run(string(key.family)+"@"+string(key.site), func(t *testing.T) {
			saved, ok := mixedOperandPolicies[key]
			if !ok {
				t.Fatalf("policy table has no %v row", key)
			}
			delete(mixedOperandPolicies, key)
			defer func() { mixedOperandPolicies[key] = saved }()

			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err == nil || err.Error() != mixedOperandNotAdmitted(key).Error() {
				t.Fatalf("LowerAt(%q) = %v, %v; want error %v", query, plan, err, mixedOperandNotAdmitted(key))
			}
		})
	}
}
