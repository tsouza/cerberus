package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerUnary handles PromQL UnaryExpr — `+expr` and `-expr` at any depth in
// the tree (top-level, inside a function call, inside a binary expression's
// operand). Unary `+` is the identity (Prom accepts it for symmetry and the
// reference engine emits it unchanged); unary `-` negates element-wise.
//
// The upstream parser folds scalar literals at parse time — `-5` becomes
// `*parser.NumberLiteral{Val: -5}` rather than a UnaryExpr around a
// NumberLiteral. The lowerer therefore only sees UnaryExpr when the operand
// is a non-literal expression (a VectorSelector, a Call, a BinaryExpr, ...).
//
// Vector operands lower to a Project that replaces the Value column with
// `0 - Value` (for `-`) or pass through unchanged (for `+`); MetricName,
// Attributes and TimeUnix are forwarded as-is.
//
// Scalar-only unary in upstream contexts (a clamp bound, the `phi` of a
// quantile, the right-hand side of an arithmetic op, ...) is unwrapped by
// `tryScalarLiteral`, which understands UnaryExpr ADD/SUB over a literal —
// so this lowerer is never invoked for those.
//
// A histogram-VALUED operand (cerberus issue #2583, e.g.
// `-demo_latency_exp_hist`) never reaches this function either:
// [lowerHistogramNativeRoot] resolves it upstream through
// [lowerExpHistogramValuedShape]'s own `*parser.UnaryExpr` producer
// (histogram_native_unary.go), which composes under every wrapper that
// already threads its argument through that same recogniser.
//
// A MIXED float/histogram `or` operand (cerberus issue #2613, e.g.
// `-(demo_latency_exp_hist or histogram_quantile(0.5, demo_latency_exp_hist))`)
// IS checked here, first, before anything else: unlike the histogram-only
// case, a mixed-or is not itself a recognised histogram-VALUED shape (its
// row carries a live Value on some rows and live histogram fields on
// others), so [lowerHistogramNativeRoot] never intercepts it, and this
// function's own [guardedValueProjection] path below would silently drop
// every histogram-shaped row via [mixedRowsFloatOnly] — the drop-family
// treatment, wrong for unary minus's actual reference semantics (Prom's
// UnaryExpr evaluator scales EVERY sample, float or histogram, in place;
// see histogram_native_unary.go's doc comment for the vendored-fork
// citation). Unary `-` over a mixed operand therefore reuses the SAME
// scale fold the scalar `<mixed> * -1` shape already applies
// (histogram_native_mixed_or_scale.go), which scales the Value column on
// float-shaped rows and the five count-bearing histogram fields on
// histogram-shaped rows identically, in one Project, with no
// discriminator-keyed conditional needed (see that file's own doc
// comment for why the two column sets are already disjoint in which row
// shape reads them for real). Unary `+` is the identity, so it defers to
// the mixed-or's own lowering unchanged, matching the pure-histogram
// ADD case just above.
func lowerUnary(u *parser.UnaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b, ok := mixedExpHistogramSetOp(u.Expr, s, ctx); ok {
		switch u.Op {
		case parser.ADD:
			return lowerMixedExpHistogramSetOp(b, s, ctx)
		case parser.SUB:
			return lowerMulOrDivScaleOverMixedExpHistogramSetOp(b, chplan.OpMul, -1, true, s, ctx)
		}
		return nil, fmt.Errorf("promql: unsupported unary op %v", u.Op)
	}
	switch u.Op {
	case parser.ADD:
		// Unary `+` is the identity — lower the operand directly.
		return lower(u.Expr, s, ctx)
	case parser.SUB:
		inner, err := lower(u.Expr, s, ctx)
		if err != nil {
			return nil, fmt.Errorf("promql: unary operand: %w", err)
		}
		newValue := &chplan.Binary{
			Op:    chplan.OpSub,
			Left:  &chplan.LitFloat{V: 0},
			Right: &chplan.ColumnRef{Name: s.ValueColumn},
		}
		return guardedValueProjection(inner, u.Expr, s, newValue), nil
	}
	return nil, fmt.Errorf("promql: unsupported unary op %v", u.Op)
}
