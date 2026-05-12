package promql

import (
	"math"

	"github.com/prometheus/prometheus/promql/parser"
)

// TryFoldScalar evaluates a scalar-only PromQL expression to a single
// constant float64, without touching the underlying ClickHouse. Returns
// (value, true) on success and (0, false) when the expression touches
// any data (vector selectors, calls, aggregations, …).
//
// This is what powers Grafana's `1+1` datasource health probe: the
// probe never needs to reach CH because the answer is a known constant.
// Without this short-circuit cerberus would surface a "scalar-only
// binary expressions not yet lowered" error and Grafana would flag the
// datasource as unhealthy.
//
// Supported shapes:
//   - NumberLiteral           — `42`, `0.5`, `1e3`, `NaN`, `Inf`
//   - ParenExpr around scalar — `(1+1)`, `((42))`
//   - UnaryExpr +/- scalar    — `-1`, `--5`, `+3`
//   - BinaryExpr scalar OP scalar with arithmetic op — `1+1`, `2*3-1`,
//     `pow(2, 10)`-style `2^10`, `1/0` (yields ±Inf, like Prom),
//     `0/0` and `1 % 0` (NaN).
//
// Comparison and logical ops are intentionally NOT folded here — Prom
// evaluates them as scalars only with the `bool` modifier; without it
// they filter, which doesn't make sense on a pure scalar. Add later
// when a real call-site needs it.
func TryFoldScalar(e parser.Expr) (float64, bool) {
	switch v := e.(type) {
	case *parser.NumberLiteral:
		return v.Val, true
	case *parser.ParenExpr:
		return TryFoldScalar(v.Expr)
	case *parser.UnaryExpr:
		inner, ok := TryFoldScalar(v.Expr)
		if !ok {
			return 0, false
		}
		if v.Op == parser.SUB {
			return -inner, true
		}
		return inner, true
	case *parser.BinaryExpr:
		lhs, lok := TryFoldScalar(v.LHS)
		if !lok {
			return 0, false
		}
		rhs, rok := TryFoldScalar(v.RHS)
		if !rok {
			return 0, false
		}
		return foldBinaryScalar(v.Op, lhs, rhs)
	}
	return 0, false
}

// foldBinaryScalar applies an arithmetic op to two scalars with Prom
// semantics for division/modulo by zero (matching Prom's behaviour:
// `1/0 = +Inf`, `-1/0 = -Inf`, `0/0 = NaN`, `1 % 0 = NaN`).
func foldBinaryScalar(op parser.ItemType, lhs, rhs float64) (float64, bool) {
	switch op {
	case parser.ADD:
		return lhs + rhs, true
	case parser.SUB:
		return lhs - rhs, true
	case parser.MUL:
		return lhs * rhs, true
	case parser.DIV:
		if rhs == 0 {
			if lhs == 0 {
				return math.NaN(), true
			}
			if lhs < 0 {
				return math.Inf(-1), true
			}
			return math.Inf(1), true
		}
		return lhs / rhs, true
	case parser.MOD:
		if rhs == 0 {
			return math.NaN(), true
		}
		return math.Mod(lhs, rhs), true
	case parser.POW:
		return math.Pow(lhs, rhs), true
	}
	return 0, false
}
