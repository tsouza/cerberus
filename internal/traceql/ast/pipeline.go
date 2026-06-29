package ast

import "strings"

// RootExpr is the parsed query. A plain spanset query populates only
// Pipeline; a metrics query additionally populates MetricsPipeline (the
// first stage) and, optionally, MetricsSecondStage.
type RootExpr struct {
	Pipeline           Pipeline
	MetricsPipeline    FirstStageElement
	MetricsSecondStage SecondStageElement
	Hints              *Hints
}

func (r RootExpr) String() string {
	var b strings.Builder
	b.WriteString(r.Pipeline.String())
	if r.MetricsPipeline != nil {
		b.WriteString(" | ")
		b.WriteString(r.MetricsPipeline.String())
	}
	if r.MetricsSecondStage != nil {
		b.WriteString(" | ")
		b.WriteString(r.MetricsSecondStage.String())
	}
	if r.Hints != nil {
		b.WriteString(r.Hints.String())
	}
	return b.String()
}

// Hints carries `with(...)` query hints. cerberus does not act on any hint
// during lowering yet; the field exists so the parser can attach them and
// round-trip the query string.
type Hints struct {
	Hints []*Hint
}

func (h *Hints) String() string {
	if h == nil || len(h.Hints) == 0 {
		return ""
	}
	parts := make([]string, len(h.Hints))
	for i, hint := range h.Hints {
		parts[i] = hint.Name + "=" + hint.Value.String()
	}
	return " with(" + strings.Join(parts, ", ") + ")"
}

// Hint is one `name=value` entry inside a `with(...)` clause.
type Hint struct {
	Name  string
	Value Static
}

// Pipeline is the ordered list of stages making up `{ ... } | a | b`.
type Pipeline struct {
	Elements []PipelineElement
}

func (Pipeline) isPipelineElement()   {}
func (Pipeline) isScalarExpression()  {}
func (Pipeline) isSpansetExpression() {}

// impliedType reports the static type of the pipeline's result: a bare
// `{ ... }` yields a spanset, while a trailing aggregate determines the
// type of an aggregating pipeline.
func (p Pipeline) impliedType() StaticType {
	if len(p.Elements) == 0 {
		return TypeSpanset
	}
	if agg, ok := p.Elements[len(p.Elements)-1].(Aggregate); ok {
		return agg.impliedType()
	}
	return TypeSpanset
}

func (p Pipeline) String() string {
	parts := make([]string, len(p.Elements))
	for i, e := range p.Elements {
		parts[i] = e.String()
	}
	return strings.Join(parts, " | ")
}

// SpansetFilter is a `{ <expression> }` stage.
type SpansetFilter struct {
	Expression FieldExpression
}

func (SpansetFilter) isPipelineElement()   {}
func (SpansetFilter) isSpansetExpression() {}

func (f SpansetFilter) String() string {
	return "{ " + f.Expression.String() + " }"
}

// SpansetOperation combines two spanset expressions with a structural or
// set operator (`>>`, `&&`, `||`, …).
type SpansetOperation struct {
	Op  Operator
	LHS SpansetExpression
	RHS SpansetExpression
}

func (SpansetOperation) isPipelineElement()   {}
func (SpansetOperation) isSpansetExpression() {}

func (o SpansetOperation) String() string {
	return o.LHS.String() + " " + o.Op.String() + " " + o.RHS.String()
}

// ScalarFilter compares two scalar expressions (`| count() > 2`).
type ScalarFilter struct {
	Op  Operator
	LHS ScalarExpression
	RHS ScalarExpression
}

func (ScalarFilter) isPipelineElement()   {}
func (ScalarFilter) isSpansetExpression() {}

func (f ScalarFilter) String() string {
	return f.LHS.String() + " " + f.Op.String() + " " + f.RHS.String()
}

// GroupOperation is a `| by(<expression>)` stage.
type GroupOperation struct {
	Expression FieldExpression
}

func (GroupOperation) isPipelineElement() {}

func (o GroupOperation) String() string {
	return "by(" + o.Expression.String() + ")"
}

// CoalesceOperation is the `| coalesce()` stage.
type CoalesceOperation struct{}

func (CoalesceOperation) isPipelineElement() {}

func (CoalesceOperation) String() string { return "coalesce()" }

// SelectOperation is a `| select(a, b, ...)` stage. The selected
// attributes are private and reached through Attrs so the slice cannot be
// mutated by callers.
type SelectOperation struct {
	attrs []Attribute
}

func newSelectOperation(attrs []Attribute) SelectOperation {
	return SelectOperation{attrs: attrs}
}

// Attrs returns the attributes named by the select stage.
func (s SelectOperation) Attrs() []Attribute { return s.attrs }

func (SelectOperation) isPipelineElement() {}

func (o SelectOperation) String() string {
	parts := make([]string, len(o.attrs))
	for i, a := range o.attrs {
		parts[i] = a.String()
	}
	return "select(" + strings.Join(parts, ", ") + ")"
}

// Aggregate is a `| count()` / `| sum(<expr>)` style stage. The op and
// inner expression are private and exposed through Op and InnerExpr.
type Aggregate struct {
	op AggregateOp
	e  FieldExpression
}

func newAggregate(op AggregateOp, e FieldExpression) Aggregate {
	return Aggregate{op: op, e: e}
}

// Op returns the aggregate function.
func (a Aggregate) Op() AggregateOp { return a.op }

// InnerExpr returns the aggregated expression (nil for count()).
func (a Aggregate) InnerExpr() FieldExpression { return a.e }

func (Aggregate) isPipelineElement()  {}
func (Aggregate) isScalarExpression() {}

func (a Aggregate) impliedType() StaticType {
	if a.op == AggregateCount {
		return TypeInt
	}
	if a.e == nil {
		return TypeAttribute
	}
	return a.e.impliedType()
}

func (a Aggregate) String() string {
	if a.e == nil {
		return a.op.String() + "()"
	}
	return a.op.String() + "(" + a.e.String() + ")"
}
