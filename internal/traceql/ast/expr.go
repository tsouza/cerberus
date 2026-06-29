package ast

// Element is the root of the AST node hierarchy: everything that can be
// printed back into a query string is an Element.
type Element interface {
	String() string
}

// The following marker interfaces partition Elements by the grammar
// position they may occupy. They are kept closed by unexported marker
// methods so that only the node types defined in this package can satisfy
// them; lowering code type-switches on the concrete types.

// FieldExpression is a scalar expression that may reference span fields —
// the operands of a span filter `{ ... }`.
type FieldExpression interface {
	Element
	typedExpression
	isFieldExpression()
}

// ScalarExpression is an aggregate-producing or constant expression usable
// on either side of a scalar filter (e.g. `| count() > 2`).
type ScalarExpression interface {
	Element
	typedExpression
	isScalarExpression()
}

// SpansetExpression is a pipeline element that yields spansets.
type SpansetExpression interface {
	PipelineElement
	isSpansetExpression()
}

// PipelineElement is a stage in a `{ ... } | ... | ...` pipeline.
type PipelineElement interface {
	Element
	isPipelineElement()
}

// BinaryOperation is a two-operand field expression: `a.x > 5`,
// `duration > 10ms && name = "GET"`, etc.
type BinaryOperation struct {
	Op  Operator
	LHS FieldExpression
	RHS FieldExpression
}

func (*BinaryOperation) isFieldExpression() {}

func (o *BinaryOperation) impliedType() StaticType {
	if o.Op.isBoolean() {
		return TypeBoolean
	}
	if t := o.LHS.impliedType(); t != TypeAttribute {
		return t
	}
	return o.RHS.impliedType()
}

func (o *BinaryOperation) String() string {
	return wrap(o.LHS) + " " + o.Op.String() + " " + wrap(o.RHS)
}

// UnaryOperation is a one-operand field expression: `!ok`, `-duration`,
// or the synthesised existence checks.
type UnaryOperation struct {
	Op         Operator
	Expression FieldExpression
}

func (UnaryOperation) isFieldExpression() {}

func (o UnaryOperation) impliedType() StaticType {
	if o.Op == OpExists || o.Op == OpNotExists {
		return TypeBoolean
	}
	return o.Expression.impliedType()
}

func (o UnaryOperation) String() string {
	return o.Op.String() + wrap(o.Expression)
}

// ScalarOperation combines two scalar expressions with an arithmetic or
// comparison operator.
type ScalarOperation struct {
	Op  Operator
	LHS ScalarExpression
	RHS ScalarExpression
}

func (ScalarOperation) isScalarExpression() {}

func (o ScalarOperation) impliedType() StaticType {
	if o.Op.isBoolean() {
		return TypeBoolean
	}
	if t := o.LHS.impliedType(); t != TypeAttribute {
		return t
	}
	return o.RHS.impliedType()
}

func (o ScalarOperation) String() string {
	return o.LHS.String() + " " + o.Op.String() + " " + o.RHS.String()
}

// impliedType is the package-internal type-inference contract. Each
// expression node knows the StaticType it evaluates to (or TypeAttribute
// when it can only be resolved at query time).
type typedExpression interface {
	impliedType() StaticType
}

// wrap renders a sub-expression, parenthesising binary operations so the
// printed form is unambiguous.
func wrap(e Element) string {
	if _, ok := e.(*BinaryOperation); ok {
		return "(" + e.String() + ")"
	}
	return e.String()
}
