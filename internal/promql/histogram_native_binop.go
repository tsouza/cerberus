package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_binop.go lowers binary arithmetic between two
// histogram-VALUED shapes — `<exp-hist shape> (+|-) <exp-hist shape>` —
// into a histogram-VALUED result (cerberus issue #2263).
//
// Reference Prometheus answers `+` between two native-histogram samples
// with FloatHistogram.Add: bucket-wise addition after reconciling the
// two operands' scales to the coarser of the two
// (promql/engine.go's vectorElemBinop, the `hlhs != nil && hrhs != nil`
// case). `-` is FloatHistogram.Sub, which upstream documents as "works
// like Add but subtracts the other histogram" — the identical
// reconciliation, with the second operand's counts negated first.
//
// Every other histogram/histogram binary op (`*`, `/`, `^`, `%`,
// `atan2`, and every comparison except `==`/`!=`) is rejected by
// reference Prometheus itself (NewIncompatibleTypesInBinOpInfo);
// cerberus's existing rejection at expHistogramSelectorRouting
// (lower.go) already answers those the same way reference does, so this
// file leaves them alone. `==`/`!=` between two histograms ARE answered
// by reference (a structural-equality filter, not a merge), but need
// different mechanics than the bucket-merge below and are tracked
// separately, along with on()/ignoring()/group_left()/group_right()
// support for `+`/`-` (see this file's rejection error below).
//
// The merge itself is NOT new arithmetic: it is the exact scale-fold +
// offset-align + zero-pad reconciliation [expHistogramMergeAggs] /
// [expHistogramMergeProjections] already implement for the cross-SERIES
// sum() merge (histogram_native_sum.go) and the histogram_quantile()
// cross-series fold (histogram_quantile_native_window.go) — both exist
// because Prometheus's own aggregation walks a group calling
// FloatHistogram.Add on each member, definitionally the same operation
// as this file's two-operand case. Reusing it here means a binary op
// between two histograms and `sum()` over two histogram samples can
// never drift apart in their arithmetic.
//
// Lowering shape (instant mode):
//
//	HistogramProjection [MetricName='', Attributes, now64(9), Value=0, <nine histogram columns>]
//	  Project [Attributes (rebuilt from the shared match key), merged
//	           Scale / ZeroCount / ZeroThreshold / {Pos,Neg}{Offset,BucketCounts},
//	           Count, Sum]
//	    Aggregate groupBy=[Attributes] having=count()=2
//	              funcs=<histogramCountSumMergeAggs>
//	      UnionAll
//	        <lhs HistogramProjection>
//	        <rhs HistogramProjection, negated for `-`>
//
// The `having count() = 2` clause stands in for VectorJoin's INNER JOIN:
// default vector matching (no on()/ignoring()/group_left()/group_right())
// keys on the full Attributes map — the same map each operand's own
// HistogramProjection is already grouped by — so a matching key with
// fewer than two contributing rows means the series existed on only one
// side, which PromQL's default one-to-one V-V matching drops. Two rows
// sharing a key on the SAME side cannot happen (each operand's own
// grouping already guarantees one row per Attributes value), so
// `!= 2` can only mean "unmatched", never "ambiguous" — unlike
// VectorJoin's on()/ignoring() case, no runtime many-to-many guard is
// needed here.
func expHistogramHistogramBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhs, rhs parser.Expr, sub bool, vm *parser.VectorMatching, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false, nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.ADD && b.Op != parser.SUB) {
		return nil, nil, false, nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, nil, false, nil, false
	}
	return b.LHS, b.RHS, b.Op == parser.SUB, b.VectorMatching, true
}

// lowerExpHistogramHistogramBinop lowers the shape
// [expHistogramHistogramBinop] recognised. Both operands defer to their
// own existing histogram-valued lowering unchanged — this function only
// adds the join + merge on top.
func lowerExpHistogramHistogramBinop(lhsExpr, rhsExpr parser.Expr, sub bool, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if !isDefaultMatching(vm) {
		return nil, fmt.Errorf(
			"promql: on()/ignoring()/group_left()/group_right() matching is not yet supported for binary " +
				"arithmetic between two exponential histogram selectors; only default (full label set) matching is supported",
		)
	}
	hpL, err := lowerExpHistogramValuedOperand(lhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	hpR, err := lowerExpHistogramValuedOperand(rhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	if sub {
		// `h1 - h2` is FloatHistogram.Sub, which reference documents as
		// Add with the second operand's counts negated first — reuse the
		// existing scalar-scaling machinery (issue #2087) to negate every
		// count-bearing field (Count, Sum, ZeroCount, both bucket
		// ladders) while leaving Scale/ZeroThreshold/offsets untouched,
		// then fold through the same merge as `+`.
		hpR = scaleHistogramProjection(hpR, chplan.OpMul, &chplan.LitFloat{V: -1}, s)
	}
	return mergeTwoHistogramProjections(hpL, hpR, s, ctx), nil
}

// lowerExpHistogramValuedOperand lowers one binop operand through its own
// histogram-valued recogniser and asserts the shared HistogramProjection
// cap every such lowering publishes.
func lowerExpHistogramValuedOperand(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*chplan.HistogramProjection, error) {
	node, matched, err := lowerExpHistogramValuedShape(expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram binop operand matched no known histogram-valued shape for %v", expr)
	}
	hp, ok := node.(*chplan.HistogramProjection)
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram binop operand lowering did not cap with *chplan.HistogramProjection, got %T", node)
	}
	return hp, nil
}

// mergeTwoHistogramProjections adds hpL and hpR's distributions,
// series-by-series, via the same scale-fold + offset-align + zero-pad
// reconciliation the cross-series merges apply. See this file's doc for
// why the join collapses to a `having count() = 2` guard rather than a
// VectorJoin.
func mergeTwoHistogramProjections(hpL, hpR *chplan.HistogramProjection, s schema.Metrics, ctx lowerCtx) chplan.Node {
	histSchema := histogramProjectionSchema(s)
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		nil, &chplan.ColumnRef{Name: histSchema.AttributesColumn}, histSchema,
	)

	stepAligned := ctx.step > 0
	var projs []chplan.Projection
	if stepAligned {
		// Range mode: both operands' HistogramProjection already
		// publishes the per-step grid anchor under the canonical
		// TimestampColumn (see nativeHistogramProjection), so grouping
		// by it keeps each anchor's merge independent — the same
		// prepend [expHistogramGroupMerge] applies for its own anchor
		// parameter.
		anchor := &chplan.ColumnRef{Name: histSchema.TimestampColumn}
		groupBy = append([]chplan.Expr{anchor}, groupBy...)
		groupByAliases = append([]string{histSchema.TimestampColumn}, groupByAliases...)
		projs = append(projs, chplan.Projection{Expr: anchor, Alias: histSchema.TimestampColumn})
	}

	merged := &chplan.Aggregate{
		Input:              &chplan.UnionAll{Inputs: []chplan.Node{hpL, hpR}},
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           histogramCountSumMergeAggs(histSchema),
		Having:             histogramBinopBothSidesMatchedGuard(),
		DropEmptyOnNoGroup: true,
	}
	projs = append(projs, chplan.Projection{Expr: attrsRebuild, Alias: histSchema.AttributesColumn})
	projs = append(projs, histogramCountSumMergeProjections(histSchema)...)
	reshaped := &chplan.Project{Input: merged, Projections: projs}

	tsExpr := chplan.Expr(chplan.NowNano())
	if stepAligned {
		tsExpr = &chplan.ColumnRef{Name: histSchema.TimestampColumn}
	}
	return nativeHistogramProjection(reshaped, &chplan.LitString{V: ""}, tsExpr, histSchema)
}

// histogramBinopBothSidesMatchedGuard renders `count() = 2` — see this
// file's doc comment for why that alone implements default one-to-one
// V-V matching's INNER JOIN semantics here.
func histogramBinopBothSidesMatchedGuard() chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.FuncCall{Fn: chplan.FnCount},
		Right: &chplan.LitInt{V: 2},
	}
}
