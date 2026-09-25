package promql

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Rendered heads of the subexpressions the native-histogram window, reset
// and merge lowerings bind once. Counting each head counts every render of
// its subtree, bound or not.
const (
	// mergedEndRenderSQL heads the merged bucket range's end: the arrayMax
	// over the per-row downscaled last indices.
	mergedEndRenderSQL = "arrayMax(arrayMap((" + paramExpMergeRowScale + ", " + paramExpMergeRowOffset + ", " + paramExpMergeRowBuckets + ") -> "
	// scaleRatioRenderSQL heads a row's scale ratio.
	scaleRatioRenderSQL = "bitShiftLeft(toInt64(?), "
	// pairScaleRenderSQL is the reset mask's reconciled pair scale.
	pairScaleRenderSQL = "least(`" + hqAggScalesArrayAlias + "`[" + paramResetPrevRow + "], `" + hqAggScalesArrayAlias + "`[" + paramResetCurrRow + "])"
	// mergedRangeBindingSQL heads the one binding of a merged range's
	// start and end.
	mergedRangeBindingSQL = "arrayMap((" + paramExpMergedStart + ", " + paramExpMergedEnd + ") -> "
	// clampedWidthRenderSQL heads the merge budget guard's clamped width.
	clampedWidthRenderSQL = "least(" + mergedRangeBindingSQL
	// pairLambdaSQL heads the reset mask's per-pair lambda, which takes
	// the pair's reconciled scale as an argument.
	pairLambdaSQL = "arrayMap((" + paramResetPrevRow + ", " + paramResetCurrRow + ", " + paramResetPairScale + ", "
)

func renderExprSQL(e chplan.Expr) string {
	sql, _ := chsql.NewQuery().SelectAs(func(b *chsql.Builder) { _ = b.Expr(e) }, "x").Build()
	return sql
}

// TestExpHistogramRepeatedSubexpressionsRenderOnce pins each builder's
// repeated subexpressions to one render per binding scope. The emitter
// renders the plan's expression DAG as a tree, and the older ClickHouse
// analyzer re-analyses every rendered copy once per derived-query level
// above it, so a second read of an unbound node is a second copy of its
// whole subtree in the planning cost.
//
// The expected counts are structural: one render per signed ladder (or
// per pair row) that the expression genuinely computes, never a ceiling.
func TestExpHistogramRepeatedSubexpressionsRenderOnce(t *testing.T) {
	t.Parallel()

	col := func(alias string) chplan.Expr { return &chplan.ColumnRef{Name: alias} }

	cases := []struct {
		name string
		expr chplan.Expr
		want map[string]int
	}{
		{
			// One ladder's clamped width, squared: the width (its start and
			// end) renders once and is squared from its binding.
			name: "merge_budget_guard_squared_width",
			expr: clampedLadderWidthSquaredExpr(
				col(hqAggScalesArrayAlias), col(hqAggPosOffsetsArrayAlias),
				col(hqAggPosBucketsArrayAlias), col(hqAggMergedScaleAlias),
			),
			want: map[string]int{
				clampedWidthRenderSQL: 1,
				mergedEndRenderSQL:    1,
				mergedStartRenderSQL:  1,
			},
		},
		{
			// One ladder's closed-form window fold: the merged length is
			// read per row and by the resize, the row's scale ratio three
			// times per target.
			name: "window_fold_closed_form",
			expr: expHistogramWindowClosedFormBucketsExpr(
				hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias,
				hqAggScalesArrayAlias, hqAggMergedScaleAlias, col(hqWindowFactorAlias),
			),
			want: map[string]int{
				mergedEndRenderSQL:   1,
				mergedStartRenderSQL: 1,
				scaleRatioRenderSQL:  1,
			},
		},
		{
			// One ladder's across-series merge: the merged range is read
			// by the length and by every target.
			name: "across_series_merge",
			expr: expHistogramMergeBucketsExpr(
				hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias,
				hqAggScalesArrayAlias, hqAggMergedScaleAlias,
			),
			want: map[string]int{
				mergedEndRenderSQL:   1,
				mergedStartRenderSQL: 1,
			},
		},
		{
			// The densified reset mask: one pair scale per pair, one ratio
			// per dense row contribution (two rows by two signed ladders),
			// and one merged range per signed ladder.
			name: "reset_mask_densified",
			expr: expHistogramResetMaskExpr(true),
			want: map[string]int{
				pairScaleRenderSQL:   1,
				scaleRatioRenderSQL:  4,
				mergedEndRenderSQL:   2,
				mergedStartRenderSQL: 2,
			},
		},
		{
			// The per-target reset mask shares the pair scale argument.
			name: "reset_mask_per_target",
			expr: expHistogramResetMaskExpr(false),
			want: map[string]int{
				pairScaleRenderSQL: 1,
				mergedEndRenderSQL: 2,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := renderExprSQL(tc.expr)
			for marker, want := range tc.want {
				if got := strings.Count(sql, marker); got != want {
					t.Errorf("%q renders %d time(s), want %d\nSQL: %s", marker, got, want, sql)
				}
			}
		})
	}
}

// TestNativeHistogramDashboardBindsRepeatedSubexpressions checks the same
// property on the whole dense dashboard query: every merged range renders
// its end exactly once (one end per merged-range binding), and every
// reset mask renders its pairs' reconciled scale exactly once (one per
// pair lambda).
func TestNativeHistogramDashboardBindsRepeatedSubexpressions(t *testing.T) {
	t.Parallel()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(
		`histogram_quantile(0.50, sum by (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))`,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []struct{ render, binding string }{
		{mergedEndRenderSQL, mergedRangeBindingSQL},
		{pairScaleRenderSQL, pairLambdaSQL},
	} {
		renders, bindings := strings.Count(sql, check.render), strings.Count(sql, check.binding)
		if bindings == 0 {
			t.Fatalf("no %q binding in the dashboard SQL; the query no longer reaches the site under test\nSQL: %s", check.binding, sql)
		}
		if renders != bindings {
			t.Errorf("%q renders %d time(s) across %d %q binding(s), want one render per binding\nSQL: %s", check.render, renders, bindings, check.binding, sql)
		}
	}
}
