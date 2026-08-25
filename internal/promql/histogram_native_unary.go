package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_unary.go answers PromQL unary `+`/`-` over an
// already histogram-VALUED operand (cerberus issue #2583) —
// `-demo_latency_exp_hist`, `+sum(demo_latency_exp_hist)`, and so on.
//
// Reference Prometheus's UnaryExpr evaluator (promql/engine.go) treats
// unary `+` as the identity — it returns the operand's own sample
// unchanged, histogram pointer included — and unary `-` as
// `FloatHistogram.Mul(-1)`:
//
//	for j := range mat[i].Histograms {
//	    mat[i].Histograms[j].H = mat[i].Histograms[j].H.Copy().Mul(-1)
//	}
//
// `Mul` (github.com/tsouza/prometheus's
// model/histogram/float_histogram.go) scales exactly the five
// COUNT-bearing fields — Count, Sum, ZeroCount, and BOTH signed bucket
// ladders — and leaves Scale, ZeroThreshold and both bucket offsets
// untouched (those describe where the buckets SIT on the value axis, not
// how much fell into them):
//
//	func (h *FloatHistogram) Mul(factor float64) *FloatHistogram {
//	    h.ZeroCount *= factor
//	    h.Count *= factor
//	    h.Sum *= factor
//	    for i := range h.PositiveBuckets { h.PositiveBuckets[i] *= factor }
//	    for i := range h.NegativeBuckets { h.NegativeBuckets[i] *= factor }
//	    ...
//	}
//
// That is exactly the five-field split [scaleHistogramProjection]
// (histogram_native_scalar_binop.go, cerberus issue #2087) already
// applies for the scalar `histogram * <literal>` shape — ZeroCount
// included, confirmed against the vendored fork above rather than
// assumed. Unary minus over a histogram-valued operand is therefore
// literally "MUL by the compile-time literal scalar -1", so this file
// recognises the AST shape and reuses [lowerExpHistogramScalarBinop]
// for the fold instead of duplicating it.
//
// [unaryOverExpHistogram] is registered directly inside
// [lowerExpHistogramValuedShape] (histogram_native_float_fn.go) as a
// PRODUCER, the same way [labelCallOverExpHistogram] is: that is what
// makes `-hist` compose under every wrapper that already threads its own
// argument through [lowerExpHistogramValuedShape] first — `sum(-hist)`
// resolves via [mergeableExpHistogramAggregate]'s own recursive call,
// `label_replace(-hist, ...)` via [labelCallOverExpHistogram]'s own
// argument check, `-hist * 2` via [isExpHistogramValuedShape]'s widening
// below — without touching any of those files. [lowerRoot] itself
// resolves the reported top-level shape the identical way, through
// [lowerHistogramNativeRoot]'s own direct call to
// [lowerExpHistogramValuedShape]. `internal/promql/unary.go`'s
// `lowerUnary` — the plain generic dispatcher `*parser.UnaryExpr`
// otherwise falls through to — is consequently never reached for a
// histogram-valued operand at all; it keeps handling only the ordinary
// float-Value case it always has.
func unaryOverExpHistogram(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (operand parser.Expr, op parser.ItemType, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, 0, false
	}
	u, isUnary := peelWrappers(expr).(*parser.UnaryExpr)
	if !isUnary || (u.Op != parser.SUB && u.Op != parser.ADD) {
		return nil, 0, false
	}
	if !isExpHistogramValuedShape(u.Expr, s, ctx) {
		return nil, 0, false
	}
	return u.Expr, u.Op, true
}

// lowerUnaryOverExpHistogram lowers the shape [unaryOverExpHistogram]
// accepted. Unary `+` defers to the operand's own histogram-valued
// lowering unchanged (reference's identity case); unary `-` scales it by
// the literal -1 through the SAME fold the scalar `*` shape uses, which
// negates Count/Sum/ZeroCount/PositiveBucketCounts/NegativeBucketCounts
// and leaves Scale/ZeroThreshold/both offsets untouched — see this
// file's own doc comment for the reference-semantics citation.
func lowerUnaryOverExpHistogram(operand parser.Expr, op parser.ItemType, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if op == parser.ADD {
		node, matched, err := lowerExpHistogramValuedShape(operand, s, ctx)
		if err != nil {
			return nil, err
		}
		if !matched {
			return nil, fmt.Errorf("promql: internal invariant violated: unary + histogram operand matched no known histogram-valued shape for %v", operand)
		}
		return node, nil
	}
	return lowerExpHistogramScalarBinop(operand, chplan.OpMul, &chplan.LitFloat{V: -1}, s, ctx, false)
}
