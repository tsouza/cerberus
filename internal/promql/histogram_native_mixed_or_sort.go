package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_sort.go lowers `sort`/`sort_desc` wrapping a
// mixed float/histogram `or` — cerberus issue #2605, the sort-specific
// sibling of the topk/bottomk/quantile composition
// (histogram_native_mixed_or_aggregate_topk.go, cerberus issue #2600) and
// the min/max/stddev/stdvar/quantile float-only family
// (histogram_native_mixed_or_aggregate_float_only.go, cerberus issues
// #2595/#2600).
//
// Reference Prometheus (promql/functions.go's funcSort / funcSortDesc)
// starts from `filterFloats(vectorVals[0])` — every sample whose `.H`
// field is set is dropped BEFORE the stable value sort runs, the same
// "process only float samples" rule the clamp family, the instant-math
// functions and the five REDUCE-family aggregates
// (histogram_native_mixed_or_aggregate_float_only.go) already apply
// against a mixed `or`. [lowerSort]'s own doc comment already cites this
// exact rule for the NON-mixed (all-histogram) case — see #2456.
//
// sort()/sort_desc() are neither a REDUCE operator (they don't fold a
// group to one row) nor a RANK/SELECT operator (they keep every
// surviving row, only reordering them) — so, like the float-only family
// and unlike topk/bottomk's own [chplan.TopK] node, they need no
// reduction or K-selection machinery at all, just the shadow-resolved
// FLOAT arm [shadowResolveMixedExpHistogramOperands] already builds for
// every sibling composition in this file group, fed straight into the
// SAME `ORDER BY Value [DESC]` plan [lowerSort]'s non-mixed path already
// builds for an ordinary (non-histogram) argument:
//
//  1. [shadowResolveMixedExpHistogramOperands] resolves the `or`'s own
//     shadow rule for both arms. The histogram-valued return is unused —
//     filterFloats drops every `.H`-set sample unconditionally, so a
//     histogram row can never survive into sort()'s output regardless of
//     which side of the `or` it shadow-resolved to keep (identical
//     reasoning to topk/bottomk's own header).
//  2. An ordinary [chplan.OrderBy] over `s.ValueColumn`, ascending for
//     `sort`/descending for `sort_desc` — the exact node [lowerSort]'s
//     non-mixed path already emits for a plain instant-vector argument.
//
// Checked directly inside [lowerSort] ([sortOverMixedExpHistogramSetOp]
// below) rather than as a sibling root-only recognizer registered in
// [lowerMixedExpHistogramFamily]: `sort`/`sort_desc` are ordinary
// [lowerCall] dispatch targets reached from EVERY nesting depth — the
// query root, via [lowerRoot]'s own fallthrough to the generic [lower]
// when [lowerHistogramNativeRoot] finds no root-only recognizer for a
// bare Call node, AND any wrapper's own recursive [lower] call on one of
// its own operands (`abs(sort(<mixed or>))`,
// `label_replace(sort(<mixed or>), ...)`, `sum(sort(<mixed or>))`, …) —
// not only from [lowerRoot] directly. A root-only registration in
// [lowerMixedExpHistogramFamily] would therefore have composed the bare
// root case but left every one of those nested shapes rejected, since
// nothing else in the histogram-native dispatch tables recurses into a
// generic Call node's own arguments looking for this shape. Mirrors
// [lowerVectorSetOpOperand]'s own precedent
// (histogram_native_mixed_or.go's header, cerberus issue #2555) of
// checking [mixedExpHistogramSetOp] directly at the one call site that
// reaches every nesting depth of ITS OWN wrapper, rather than trying to
// thread a recognizer through [lowerHistogramNativeRoot]'s root-only
// table.
func sortOverMixedExpHistogramSetOp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (*parser.BinaryExpr, bool) {
	if len(c.Args) != 1 {
		return nil, false
	}
	return mixedExpHistogramSetOp(c.Args[0], s, ctx)
}

// lowerSortOverMixedExpHistogramSetOp lowers the shape
// [sortOverMixedExpHistogramSetOp] recognised. See this file's header for
// why the shadow-resolved float arm alone, fed through an ordinary
// ORDER BY, already answers reference's semantics.
func lowerSortOverMixedExpHistogramSetOp(c *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatForAgg, err := shadowResolveFloatArmChecked(b, s, ctx)
	if err != nil {
		return nil, err
	}

	desc := c.Func.Name == "sort_desc"
	return &chplan.OrderBy{
		Input: floatForAgg,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Desc: desc},
		},
	}, nil
}
