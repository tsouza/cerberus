package lsyntax

import (
	"fmt"
	"math"
)

// IsLogicalBinOp reports whether op is one of the logical/set binary
// operators (`or`, `and`, `unless`), which operate on vectors only and
// reject literal legs.
func IsLogicalBinOp(op string) bool {
	switch op {
	case OpTypeOr, OpTypeAnd, OpTypeUnless:
		return true
	}
	return false
}

// foldScalarBinOp evaluates a binary operation between two scalar values,
// mirroring the upstream LogQL scalar-merge semantics. It backs the
// parse-time constant folding of literal-only binary expressions, where
// both legs are always concrete scalars (no vector comparison, no filter
// semantics).
func foldScalarBinOp(op string, left, right float64) (float64, error) {
	switch op {
	case OpTypeAdd:
		return left + right, nil
	case OpTypeSub:
		return left - right, nil
	case OpTypeMul:
		return left * right, nil
	case OpTypeDiv:
		if right == 0 {
			return math.NaN(), nil
		}
		return left / right, nil
	case OpTypeMod:
		if right == 0 {
			return math.NaN(), nil
		}
		return math.Mod(left, right), nil
	case OpTypePow:
		return math.Pow(left, right), nil
	case OpTypeCmpEQ:
		return boolToFloat(left == right), nil
	case OpTypeNEQ:
		return boolToFloat(left != right), nil
	case OpTypeGT:
		return boolToFloat(left > right), nil
	case OpTypeGTE:
		return boolToFloat(left >= right), nil
	case OpTypeLT:
		return boolToFloat(left < right), nil
	case OpTypeLTE:
		return boolToFloat(left <= right), nil
	default:
		return 0, fmt.Errorf("should never happen: unexpected operation: (%s)", op)
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

// reduceBinOp folds a binary operation between two literal floats into a
// single literal, maintaining the invariant that a BinOpExpr never has
// two literal legs.
func reduceBinOp(op string, left, right float64) *LiteralExpr {
	val, err := foldScalarBinOp(op, left, right)
	if err != nil {
		return &LiteralExpr{err: err}
	}
	return &LiteralExpr{Val: val}
}
