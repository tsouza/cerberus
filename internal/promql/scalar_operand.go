package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// scalarOperandExpr recognises the complete scalar-typed operand surface the
// PromQL parser admits at a vector binary operator. Literal scalar trees keep
// the existing fast path; calls such as scalar(vector(2)) and scalar(up) are
// lowered into an expression that can be embedded in a projection. The type
// check is deliberately parser-owned: asking the AST instead of maintaining a
// second list of scalar-returning calls keeps this recogniser aligned with the
// upstream type checker.
func scalarOperandExpr(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Expr, bool) {
	if value, ok := tryScalarLiteral(expr); ok {
		return &chplan.LitFloat{V: value}, true
	}
	if expr.Type() != parser.ValueTypeScalar {
		return nil, false
	}
	value, err := lowerScalarArg(expr, s, ctx)
	if err != nil {
		return nil, false
	}
	return value, true
}
