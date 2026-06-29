package ast

// This file implements the meaning-preserving array-fold rewrites the
// TraceQL language applies after parsing. They collapse a homogeneous chain of
// per-value comparisons against a single attribute into one array comparison:
//
//	{ .x = "a" || .x = "b" }   =>  { .x IN ["a", "b"] }
//	{ .x != "a" && .x != "b" } =>  { .x NOT IN ["a", "b"] }
//	{ .x =~ "a" || .x =~ "b" } =>  { .x =~ ["a", "b"] }   (regex match-any)
//	{ .x !~ "a" && .x !~ "b" } =>  { .x !~ ["a", "b"] }   (regex match-none)
//
// Lowering consumes the folded OpIn/OpNotIn/OpRegexMatchAny/OpRegexMatchNone
// forms directly, so the rewrite is load-bearing rather than cosmetic. The
// pass walks every field expression in the query post-order, so inner folds
// feed outer ones and a 3+ wide chain (`a=1 || a=2 || a=3`) collapses fully.

// applyRewrites returns r with the array-fold rewrites applied across its
// pipeline (and any nested pipelines reachable from spanset operations). It
// mutates the field expressions in place and returns the same root for
// convenience.
func applyRewrites(r *RootExpr) *RootExpr {
	if r == nil {
		return r
	}
	rewritePipeline(&r.Pipeline)
	return r
}

// rewritePipeline rewrites every field-expression-bearing element of p.
func rewritePipeline(p *Pipeline) {
	for i, elem := range p.Elements {
		switch e := elem.(type) {
		case *SpansetFilter:
			e.Expression = rewriteFieldExpr(e.Expression)
		case GroupOperation:
			e.Expression = rewriteFieldExpr(e.Expression)
			p.Elements[i] = e
		case SpansetOperation:
			rewriteSpansetExpr(e.LHS)
			rewriteSpansetExpr(e.RHS)
		case Pipeline:
			rewritePipeline(&e)
			p.Elements[i] = e
		}
	}
}

// rewriteSpansetExpr descends a spanset expression looking for nested
// pipelines / filters whose field expressions can fold.
func rewriteSpansetExpr(se SpansetExpression) {
	switch s := se.(type) {
	case *SpansetFilter:
		s.Expression = rewriteFieldExpr(s.Expression)
	case SpansetOperation:
		rewriteSpansetExpr(s.LHS)
		rewriteSpansetExpr(s.RHS)
	case Pipeline:
		rewritePipeline(&s)
	}
}

// rewriteFieldExpr rewrites fe and all of its sub-expressions post-order,
// returning the (possibly replaced) expression.
func rewriteFieldExpr(fe FieldExpression) FieldExpression {
	switch e := fe.(type) {
	case *BinaryOperation:
		e.LHS = rewriteFieldExpr(e.LHS)
		e.RHS = rewriteFieldExpr(e.RHS)
		if folded, ok := foldArrayComparison(e); ok {
			return folded
		}
		return e
	case UnaryOperation:
		e.Expression = rewriteFieldExpr(e.Expression)
		return e
	default:
		return fe
	}
}

// foldRule names the outer/scalar/array operator triple and any type
// restriction for one of the four array-fold rewrites.
type foldRule struct {
	outer    Operator
	scalar   Operator
	array    Operator
	restrict []StaticType // empty => any mergeable scalar type
}

var arrayFoldRules = []foldRule{
	{outer: OpOr, scalar: OpEqual, array: OpIn},
	{outer: OpAnd, scalar: OpNotEqual, array: OpNotIn},
	{outer: OpOr, scalar: OpRegex, array: OpRegexMatchAny, restrict: []StaticType{TypeString, TypeStringArray}},
	{outer: OpAnd, scalar: OpNotRegex, array: OpRegexMatchNone, restrict: []StaticType{TypeString, TypeStringArray}},
}

// foldArrayComparison attempts to collapse a single `<cmp> <outer> <cmp>`
// binary operation into one array comparison. It returns ok=false when no rule
// applies (the operands disagree on attribute, the comparison operators don't
// match a rule, or the literal types are not mergeable).
func foldArrayComparison(op *BinaryOperation) (FieldExpression, bool) {
	for _, rule := range arrayFoldRules {
		if op.Op != rule.outer {
			continue
		}
		attrL, valL, ok := attrLiteralOperands(op.LHS, rule.scalar, rule.array)
		if !ok {
			continue
		}
		attrR, valR, ok := attrLiteralOperands(op.RHS, rule.scalar, rule.array)
		if !ok || attrL != attrR {
			continue
		}
		if len(rule.restrict) > 0 {
			if !staticTypeAllowed(valL.Type, rule.restrict) || !staticTypeAllowed(valR.Type, rule.restrict) {
				continue
			}
		}
		merged, ok := staticMerge(valL, valR)
		if !ok {
			continue
		}
		return &BinaryOperation{Op: rule.array, LHS: attrL, RHS: merged}, true
	}
	return nil, false
}

// attrLiteralOperands extracts (attribute, literal) from a comparison binary
// operation whose operator is one of ops. The attribute may sit on either side
// (`.x = 1` or `1 = .x`). The literal may itself already be an array (left by
// an inner fold), which is how 3+ wide chains accumulate.
func attrLiteralOperands(fe FieldExpression, ops ...Operator) (Attribute, Static, bool) {
	bin, ok := fe.(*BinaryOperation)
	if !ok || !operatorIn(bin.Op, ops) {
		return Attribute{}, StaticNil, false
	}
	attr, ok := bin.LHS.(Attribute)
	lit := bin.RHS
	if !ok {
		attr, ok = bin.RHS.(Attribute)
		if !ok {
			return Attribute{}, StaticNil, false
		}
		lit = bin.LHS
	}
	val, ok := lit.(Static)
	if !ok {
		return Attribute{}, StaticNil, false
	}
	return attr, val, true
}

func operatorIn(op Operator, ops []Operator) bool {
	for _, o := range ops {
		if op == o {
			return true
		}
	}
	return false
}

func staticTypeAllowed(t StaticType, allowed []StaticType) bool {
	for _, a := range allowed {
		if t == a {
			return true
		}
	}
	return false
}

// staticMerge combines two statics into the array static of the matching
// element type. Either operand may be a scalar or the already-merged array of
// the same family; mixed families are rejected (ok=false).
func staticMerge(a, b Static) (Static, bool) {
	switch a.Type {
	case TypeString, TypeStringArray:
		out, ok := stringMembers(a)
		if !ok {
			return StaticNil, false
		}
		more, ok := stringMembers(b)
		if !ok {
			return StaticNil, false
		}
		return NewStaticStringArray(append(out, more...)), true
	case TypeInt, TypeIntArray:
		out, ok := intMembers(a)
		if !ok {
			return StaticNil, false
		}
		more, ok := intMembers(b)
		if !ok {
			return StaticNil, false
		}
		return NewStaticIntArray(append(out, more...)), true
	case TypeFloat, TypeFloatArray:
		out, ok := floatMembers(a)
		if !ok {
			return StaticNil, false
		}
		more, ok := floatMembers(b)
		if !ok {
			return StaticNil, false
		}
		return NewStaticFloatArray(append(out, more...)), true
	case TypeBoolean, TypeBooleanArray:
		out, ok := boolMembers(a)
		if !ok {
			return StaticNil, false
		}
		more, ok := boolMembers(b)
		if !ok {
			return StaticNil, false
		}
		return NewStaticBooleanArray(append(out, more...)), true
	default:
		return StaticNil, false
	}
}

func stringMembers(s Static) ([]string, bool) {
	switch s.Type {
	case TypeString:
		return []string{s.EncodeToString(false)}, true
	case TypeStringArray:
		arr, _ := s.StringArray()
		return append([]string(nil), arr...), true
	default:
		return nil, false
	}
}

func intMembers(s Static) ([]int, bool) {
	switch s.Type {
	case TypeInt:
		n, _ := s.Int()
		return []int{n}, true
	case TypeIntArray:
		arr, _ := s.IntArray()
		return append([]int(nil), arr...), true
	default:
		return nil, false
	}
}

func floatMembers(s Static) ([]float64, bool) {
	switch s.Type {
	case TypeFloat:
		return []float64{s.Float()}, true
	case TypeFloatArray:
		arr, _ := s.FloatArray()
		return append([]float64(nil), arr...), true
	default:
		return nil, false
	}
}

func boolMembers(s Static) ([]bool, bool) {
	switch s.Type {
	case TypeBoolean:
		b, _ := s.Bool()
		return []bool{b}, true
	case TypeBooleanArray:
		arr, _ := s.BooleanArray()
		return append([]bool(nil), arr...), true
	default:
		return nil, false
	}
}
