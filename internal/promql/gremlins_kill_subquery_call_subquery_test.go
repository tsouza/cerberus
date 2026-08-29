// Tests in this file kill the LIVED gremlins mutants reported for
// histogram_native_subquery_call_subquery.go's three Histogram-vs-Mixed
// dispatch guards (phase4-promql-h, PR #2727 CI run 33272135039) — each
// one is a `if shape == chplan.HistogramRowShape` conditional inside
// [lowerHistogramOrMixedCallSubqueryInput] that a plain "lowers without
// error" test cannot distinguish, because BOTH branches return a
// non-nil, no-error plan for either input shape. See gremlins_kill_test.go
// for the shared file-header convention this file follows.
//
// This file lives in `package promql` (not `promql_test`) specifically to
// reach [mixedPairValueArrayAlias] — the resets/changes branch is the one
// case where BOTH branches wrap their result in the same
// [expHistogramPairCountProjection] outer node, so [chplan.RowShapeOf]
// alone cannot tell them apart; only the underlying RangeBucketFanout's
// own AggFuncs differ ([mixedPairCountAggs] appends two extra groupArray
// aggregates the Histogram-only [expHistogramPairCountAggs] never emits).
package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

var gremlinsCallSubqueryEnd = time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

const (
	gremlinsCallSubqueryHistInner  = `(latency_exp_hist) and (latency_exp_hist)`
	gremlinsCallSubqueryMixedInner = `(latency_exp_hist) or (num_cpus)`
)

func gremlinsCallSubqueryLower(t *testing.T, fn, inner string) chplan.Node {
	t.Helper()
	query := fn + "((" + inner + ")[2m:1m])[10m:1m]"
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	s := schema.DefaultOTelMetrics()
	node, err := LowerAt(context.Background(), expr, s, gremlinsCallSubqueryEnd, gremlinsCallSubqueryEnd)
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	return node
}

func gremlinsFindRangeBucketFanout(n chplan.Node) *chplan.RangeBucketFanout {
	if n == nil {
		return nil
	}
	if rbf, ok := n.(*chplan.RangeBucketFanout); ok {
		return rbf
	}
	for _, kid := range n.Children() {
		if hit := gremlinsFindRangeBucketFanout(kid); hit != nil {
			return hit
		}
	}
	return nil
}

func gremlinsFanoutAggAliases(rbf *chplan.RangeBucketFanout) map[string]bool {
	aliases := make(map[string]bool, len(rbf.AggFuncs))
	for _, agg := range rbf.AggFuncs {
		aliases[agg.Alias] = true
	}
	return aliases
}

// TestGremlinsKill_CallSubqueryLastFirst_HistVsMixedRouting kills
// histogram_native_subquery_call_subquery.go:125:12 (CONDITIONALS_NEGATION
// on `if shape == chplan.HistogramRowShape` in the last_over_time /
// first_over_time case). A HistogramRowShape inner must route through
// [lowerSelectFnOverCallSubqueryInput] ([nativeHistogramProjection], a
// *chplan.HistogramProjection — chplan.HistogramRowShape); a MixedRowShape
// inner must route through [lowerMixedLastFirstOverCallSubqueryInput]
// ([mixedLastFirstProjection], a plain *chplan.Project with no
// MixedDiscriminatorColumn projection — chplan.SampleRowShape by
// [chplan.RowShapeOf]'s own default). Negating the guard swaps which
// continuation each input reaches, flipping both observed shapes.
func TestGremlinsKill_CallSubqueryLastFirst_HistVsMixedRouting(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"last_over_time", "first_over_time"} {
		t.Run(fn, func(t *testing.T) {
			histShape := chplan.RowShapeOf(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryHistInner))
			if histShape != chplan.HistogramRowShape {
				t.Errorf("%s over hist_and inner: RowShape = %s, want HistogramRowShape", fn, histShape)
			}
			mixedShape := chplan.RowShapeOf(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryMixedInner))
			if mixedShape == chplan.HistogramRowShape {
				t.Errorf("%s over mixed_or inner: RowShape = %s, want anything but HistogramRowShape", fn, mixedShape)
			}
		})
	}
}

// TestGremlinsKill_CallSubqueryFold_HistVsMixedRouting kills
// histogram_native_subquery_call_subquery.go:139:12 (CONDITIONALS_NEGATION
// on the identical guard in the FOLD-family case). A HistogramRowShape
// inner routes through [lowerExpHistogramFoldOverCallSubqueryInput]
// ([aggregatedHistogramProjection], chplan.HistogramRowShape); a
// MixedRowShape inner routes through [lowerMixedFoldOverCallSubqueryInput]
// ([combineMixedAggregateBranches], which republishes the
// MixedDiscriminatorColumn — chplan.MixedRowShape).
func TestGremlinsKill_CallSubqueryFold_HistVsMixedRouting(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"rate", "sum_over_time"} {
		t.Run(fn, func(t *testing.T) {
			histShape := chplan.RowShapeOf(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryHistInner))
			if histShape != chplan.HistogramRowShape {
				t.Errorf("%s over hist_and inner: RowShape = %s, want HistogramRowShape", fn, histShape)
			}
			mixedShape := chplan.RowShapeOf(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryMixedInner))
			if mixedShape != chplan.MixedRowShape {
				t.Errorf("%s over mixed_or inner: RowShape = %s, want MixedRowShape", fn, mixedShape)
			}
		})
	}
}

// TestGremlinsKill_CallSubqueryResetsChanges_HistVsMixedRouting kills
// histogram_native_subquery_call_subquery.go:132:12 (CONDITIONALS_NEGATION
// on the identical guard in the resets/changes case). Both branches wrap
// their result in the SAME [expHistogramPairCountProjection] outer node,
// so chplan.RowShapeOf cannot distinguish them — the two branches differ
// only in the underlying RangeBucketFanout's own AggFuncs:
// [mixedPairCountAggs] appends two extra groupArray aggregates
// ([mixedPairValueArrayAlias] / [mixedPairDiscrArrayAlias]) the
// Histogram-only [expHistogramPairCountAggs] never emits.
func TestGremlinsKill_CallSubqueryResetsChanges_HistVsMixedRouting(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{"resets", "changes"} {
		t.Run(fn, func(t *testing.T) {
			histFanout := gremlinsFindRangeBucketFanout(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryHistInner))
			if histFanout == nil {
				t.Fatalf("%s over hist_and inner: no RangeBucketFanout found in plan", fn)
			}
			if aliases := gremlinsFanoutAggAliases(histFanout); aliases[mixedPairValueArrayAlias] {
				t.Errorf("%s over hist_and inner: fanout AggFuncs unexpectedly include %q (mixed-only aggregate)", fn, mixedPairValueArrayAlias)
			}

			mixedFanout := gremlinsFindRangeBucketFanout(gremlinsCallSubqueryLower(t, fn, gremlinsCallSubqueryMixedInner))
			if mixedFanout == nil {
				t.Fatalf("%s over mixed_or inner: no RangeBucketFanout found in plan", fn)
			}
			if aliases := gremlinsFanoutAggAliases(mixedFanout); !aliases[mixedPairValueArrayAlias] {
				t.Errorf("%s over mixed_or inner: fanout AggFuncs missing %q (mixed-only aggregate)", fn, mixedPairValueArrayAlias)
			}
		})
	}
}

// TestGremlinsKill_CallSubqueryFold_AnchorErrorPropagates kills
// histogram_native_subquery_call_subquery.go:236:9 (CONDITIONALS_NEGATION
// on `if err != nil` inside [lowerExpHistogramFoldOverCallSubqueryInput]'s
// own subqueryAnchor(grid.outerSub, ctx) call — the FOLD-family sibling of
// the identical guard in [lowerSelectFnOverCallSubqueryInput] /
// [lowerMixedLastFirstOverCallSubqueryInput] /
// [lowerMixedResetsOrChangesOverCallSubqueryInput]. An outer subquery
// bracket carrying `@ start()`, lowered through the range-context-free
// [Lower]/[LowerAt] entrypoint (zero Start), makes [subqueryAnchor] itself
// return a real error here — negating the check would swallow it and
// return a nil error with a nil node instead.
func TestGremlinsKill_CallSubqueryFold_AnchorErrorPropagates(t *testing.T) {
	t.Parallel()
	query := "rate((" + gremlinsCallSubqueryHistInner + ")[2m:1m])[10m:1m] @ start()"
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	_, err = LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), time.Time{}, gremlinsCallSubqueryEnd)
	if err == nil {
		t.Fatalf("lower(%q): want error, got nil", query)
	}
	if !strings.Contains(err.Error(), "`@ start()` modifier requires query range context") {
		t.Errorf("lower(%q): got error %q, want it to mention the `@ start()` range-context requirement", query, err)
	}
}
