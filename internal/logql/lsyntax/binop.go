package lsyntax

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql"
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

// MergeBinOp evaluates a binary operation between two scalar samples.
// It mirrors the upstream LogQL scalar merge semantics and backs the
// parse-time constant folding of literal-only binary expressions.
func MergeBinOp(op string, left, right *promql.Sample, swap, filter, isVectorComparison bool) (*promql.Sample, error) {
	var merger func(left, right *promql.Sample) *promql.Sample

	switch op {
	case OpTypeAdd:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			res.F += r.F
			return &res
		}
	case OpTypeSub:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			res.F -= r.F
			return &res
		}
	case OpTypeMul:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			res.F *= r.F
			return &res
		}
	case OpTypeDiv:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			if r.F == 0 {
				res.F = math.NaN()
			} else {
				res.F /= r.F
			}
			return &res
		}
	case OpTypeMod:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			if r.F == 0 {
				res.F = math.NaN()
			} else {
				res.F = math.Mod(res.F, r.F)
			}
			return &res
		}
	case OpTypePow:
		merger = func(l, r *promql.Sample) *promql.Sample {
			if l == nil || r == nil {
				return nil
			}
			res := *l
			res.F = math.Pow(l.F, r.F)
			return &res
		}
	case OpTypeCmpEQ:
		merger = comparator(func(l, r float64) bool { return l == r }, filter)
	case OpTypeNEQ:
		merger = comparator(func(l, r float64) bool { return l != r }, filter)
	case OpTypeGT:
		merger = comparator(func(l, r float64) bool { return l > r }, filter)
	case OpTypeGTE:
		merger = comparator(func(l, r float64) bool { return l >= r }, filter)
	case OpTypeLT:
		merger = comparator(func(l, r float64) bool { return l < r }, filter)
	case OpTypeLTE:
		merger = comparator(func(l, r float64) bool { return l <= r }, filter)
	default:
		return nil, fmt.Errorf("should never happen: unexpected operation: (%s)", op)
	}

	res := merger(left, right)
	if !isVectorComparison {
		return res, nil
	}
	if filter {
		retSample := left
		if swap {
			retSample = right
		}
		if res != nil {
			return retSample, nil
		}
	}
	return res, nil
}

func comparator(pred func(l, r float64) bool, filter bool) func(l, r *promql.Sample) *promql.Sample {
	return func(l, r *promql.Sample) *promql.Sample {
		if l == nil || r == nil {
			return nil
		}
		res := *l
		val := 0.0
		if pred(l.F, r.F) {
			val = 1.0
		} else if filter {
			return nil
		}
		res.F = val
		return &res
	}
}

// reduceBinOp folds a binary operation between two literal floats into a
// single literal, maintaining the invariant that a BinOpExpr never has
// two literal legs.
func reduceBinOp(op string, left, right float64) *LiteralExpr {
	merged, err := MergeBinOp(op, &promql.Sample{F: left}, &promql.Sample{F: right}, false, false, false)
	if err != nil {
		return &LiteralExpr{err: err}
	}
	return &LiteralExpr{Val: merged.F}
}
