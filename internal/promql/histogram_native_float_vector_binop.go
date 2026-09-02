package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_float_vector_binop.go answers the third "drop, don't
// reject" leg for exponential-histogram binary operators (cerberus issue
// #2331): `<float-VECTOR shape> OP <exp-hist shape>` (or its mirror),
// where neither operand is a compile-time scalar literal — so
// [expHistogramDroppingScalarBinop] (histogram_native_scalar_binop.go,
// cerberus issue #2189) does not recognise it — and at most one operand
// is histogram-valued — so [expHistogramHistogramBinop] /
// [expHistogramDroppingHistogramBinop] (histogram_native_binop.go,
// cerberus issues #2263 / #2277) do not either, since both require BOTH
// operands histogram-valued.
//
// Reference Prometheus's vectorElemBinop (promql/engine.go) treats a
// plain float SAMPLE — matched pairwise against the other operand's
// value, vector-vector — exactly like a scalar operand for this
// decision: the `hlhs == nil && hrhs != nil` / `hlhs != nil && hrhs ==
// nil` cases key only on which side carries the histogram, never on
// whether that float value came from a literal or a real per-series
// sample. [expHistogramScalarOpDropsSample] already encodes that op
// family (ADD/SUB/POW/MOD/every comparison/ATAN2 drop unconditionally;
// DIV drops only when the histogram is NOT on the left) — this file
// reuses it verbatim rather than re-deriving the same table.
//
// MUL, and histogram-left DIV, are deliberately left UNMATCHED here:
// those are reference's supported histogram-SCALING shapes, but cerberus
// only implements the scaling machinery
// ([expHistogramScalarBinop]/[lowerExpHistogramScalarBinop]) for a
// compile-time scalar LITERAL multiplier/divisor — scaling a histogram
// by an arbitrary per-series float VECTOR value would need to join the
// two operands row-by-row before folding, a materially bigger feature
// this issue does not build. Leaving those shapes unmatched keeps this
// recognizer from mis-answering a genuinely-scalable pair with an empty
// drop; a query like `some_float_vector * latency_exp_hist` therefore
// still falls through to [expHistogramSelectorRouting]'s pre-existing
// catch-all rejection, unchanged from before this file existed.
//
// Wired at [lowerRoot] — the same TOP-LEVEL-only dispatch point as its
// two siblings, not inside lowerVectorVector — so it composes with
// [lowerRoot]'s existing chain the identical way and is reachable
// regardless of instant vs. range mode.
func expHistogramDroppingVectorBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histSide, floatSide parser.Expr, ok bool) {
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin {
		return nil, nil, false
	}
	// A compile-time scalar literal on either side is
	// expHistogramDroppingScalarBinop's own shape (and lowerBinary
	// routes it to lowerVectorScalar before lowerVectorVector — and
	// this function, reached only via lowerRoot's own dispatch outside
	// lowerVectorVector — would ever see it). Excluding it here keeps
	// the two recognisers disjoint instead of double-matching the same
	// operator pair.
	if _, isScalar := tryScalarLiteral(b.LHS); isScalar {
		return nil, nil, false
	}
	if _, isScalar := tryScalarLiteral(b.RHS); isScalar {
		return nil, nil, false
	}
	lhsHist := isExpHistogramValuedShape(b.LHS, s, ctx)
	rhsHist := isExpHistogramValuedShape(b.RHS, s, ctx)
	switch {
	case lhsHist && !rhsHist:
		if expHistogramScalarOpDropsSample(b.Op, true /* histogramOnLeft */) {
			return b.LHS, b.RHS, true
		}
	case rhsHist && !lhsHist:
		if expHistogramScalarOpDropsSample(b.Op, false /* histogramOnLeft */) {
			return b.RHS, b.LHS, true
		}
	}
	return nil, nil, false
}

// lowerExpHistogramDroppingVectorBinop lowers the shape
// [expHistogramDroppingVectorBinop] recognised. The histogram side
// lowers through its own existing histogram-valued recogniser (the same
// [lowerExpHistogramValuedOperand] helper the histogram/histogram drop
// path uses) and caps the resulting constant-false Filter, matching
// reference's accepted-but-empty result. The float side is lowered too,
// through the ordinary expression path, purely so its OWN lowering
// errors still surface — e.g. an unsupported function nested in the
// float side — rather than being silently swallowed by the drop; its
// plan is otherwise discarded, since every row is filtered out
// regardless of what the float side would have produced.
func lowerExpHistogramDroppingVectorBinop(histSide, floatSide parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	hp, err := lowerExpHistogramValuedOperand(histSide, s, ctx)
	if err != nil {
		return nil, err
	}
	if _, err := lower(floatSide, s, ctx); err != nil {
		return nil, err
	}
	// Reference returns keep=false plus an incompatible-types info
	// annotation. Cerberus has no info-annotation wire channel, but
	// matches the accepted empty result — the same Filter{Predicate:
	// LitBool{false}} shape [lowerExpHistogramScalarBinop] and
	// [lowerExpHistogramDroppingHistogramBinop] use for their own drop
	// paths.
	return &chplan.Filter{Input: hp, Predicate: &chplan.LitBool{V: false}}, nil
}
