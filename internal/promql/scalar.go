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
// This is what powers Grafana's `1+1` datasource health probe and the
// PromQL `vector(scalar)` / `time()` lowerings: the probe never needs
// to reach CH because the answer is a known constant. The
// /api/v1/query_range scalar shortcut in api/prom/handler.go and the
// scalar-only-binop fold in promql/binary.go both consume this folder.
//
// Supported shapes:
//   - NumberLiteral           — `42`, `0.5`, `1e3`, `NaN`, `Inf`
//   - ParenExpr around scalar — `(1+1)`, `((42))`
//   - UnaryExpr +/- scalar    — `-1`, `--5`, `+3`
//   - BinaryExpr scalar OP scalar with arithmetic op — `1+1`, `2*3-1`,
//     `pow(2, 10)`-style `2^10`, `1/0` (yields ±Inf, like Prom),
//     `0/0` and `1 % 0` (NaN).
//   - BinaryExpr scalar OP scalar with comparison op AND ReturnBool —
//     `1 == bool 2 → 0`, `1 < bool 2 → 1`. Bare comparison
//     (no `bool`) is rejected by the Prom parser on two scalars, so
//     the only way one reaches the folder is with the modifier set.
//
// Logical ops (`and`/`or`/`unless`) are not folded — Prom rejects them
// on scalars at parse time.
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
		if isFoldableComparison(v.Op) {
			// Prom requires the `bool` modifier on scalar-scalar
			// comparisons; without it the parser rejects the query
			// at parse time, so an unflagged comparison reaching this
			// far is a parser bug we don't try to paper over.
			if !v.ReturnBool {
				return 0, false
			}
			return foldComparisonScalar(v.Op, lhs, rhs)
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

// isFoldableComparison reports whether op is a comparison operator
// that can be folded on two scalars. Mirrors PromQL's comparison set
// (== / != / < / <= / > / >=) and intentionally excludes logical ops
// (and/or/unless) — those are set operators on vectors, not foldable
// scalars.
func isFoldableComparison(op parser.ItemType) bool {
	switch op {
	case parser.EQLC, parser.NEQ, parser.LSS, parser.LTE, parser.GTR, parser.GTE:
		return true
	}
	return false
}

// foldComparisonScalar applies a comparison op to two scalars and
// returns 1.0 on true / 0.0 on false. PromQL's `bool` modifier maps
// comparison-with-vector to a 1.0/0.0 Project; the same arithmetic
// holds for scalar-scalar so we mirror it here.
//
// NaN-comparison semantics follow IEEE-754: any comparison involving
// NaN is false, so `NaN == bool NaN → 0` and `NaN != bool NaN → 1`.
// This matches Prom's evaluator, which delegates to Go's comparison
// operators at the same precision.
func foldComparisonScalar(op parser.ItemType, lhs, rhs float64) (float64, bool) {
	var result bool
	switch op {
	case parser.EQLC:
		result = lhs == rhs
	case parser.NEQ:
		result = lhs != rhs
	case parser.LSS:
		result = lhs < rhs
	case parser.LTE:
		result = lhs <= rhs
	case parser.GTR:
		result = lhs > rhs
	case parser.GTE:
		result = lhs >= rhs
	default:
		return 0, false
	}
	if result {
		return 1, true
	}
	return 0, true
}
