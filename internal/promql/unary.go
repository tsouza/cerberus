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
func lowerUnary(u *parser.UnaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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
		return projectValueOverInner(inner, s, newValue), nil
	}
	return nil, fmt.Errorf("promql: unsupported unary op %v", u.Op)
}
