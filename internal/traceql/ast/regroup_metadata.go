package ast

// This file answers one question about a parsed search query: does
// reference Tempo leave a trace-level search-summary field unset for it?
//
// Tempo fills a search summary's trace-level fields (rootServiceName,
// rootTraceName, durationMs) from the spanset the pipeline returns. A
// `by(...)` or `coalesce()` stage returns fresh spansets that carry none of
// those fields, and the storage layer backfills them from its second fetch
// pass. That pass skips every trace-level intrinsic the first pass already
// read for all candidate spans: one whose first-pass condition has no
// predicate (OpNone), or any of them when every condition must hold
// (AllConditions). See SearchMetaConditionsWithout in Tempo's
// pkg/traceql/storage.go and the rebatch copy in vparquet5's
// block_traceql.go. So a regrouped search that reads such an intrinsic
// reports its field empty.
//
// The walk below mirrors Tempo's extractConditions (pkg/traceql/ast.go,
// ast_conditions.go) for the parts that decide that outcome: which
// attributes reach the first pass with which operator, and when
// AllConditions is cleared.

// RegroupLeavesTraceFieldUnset reports whether reference Tempo's search
// response leaves the trace-level field backed by intrinsic in empty for this
// query: the pipeline regroups its spansets (`by(...)` or `coalesce()`) and
// the first fetch pass already read in for every candidate span.
//
// Only the trace-level intrinsics that back a search-summary field
// (IntrinsicTraceRootService, IntrinsicTraceRootSpan,
// IntrinsicTraceDuration) are meaningful arguments; any other returns false.
func (r *RootExpr) RegroupLeavesTraceFieldUnset(in Intrinsic) bool {
	if r == nil {
		return false
	}
	switch in {
	case IntrinsicTraceRootService, IntrinsicTraceRootSpan, IntrinsicTraceDuration:
	default:
		return false
	}
	if !pipelineRegroups(r.Pipeline) {
		return false
	}
	c := fetchConditions{all: true}
	c.pipeline(r.Pipeline)
	for _, fc := range c.conds {
		if fc.intrinsic == in && (fc.op == OpNone || c.all) {
			return true
		}
	}
	return false
}

// pipelineRegroups reports whether a stage of p builds new spansets —
// `by(...)` or `coalesce()` — at any depth.
func pipelineRegroups(p Pipeline) bool {
	for _, elem := range p.Elements {
		if elemRegroups(elem) {
			return true
		}
	}
	return false
}

func elemRegroups(elem any) bool {
	switch e := elem.(type) {
	case GroupOperation, CoalesceOperation:
		return true
	case Pipeline:
		return pipelineRegroups(e)
	case SpansetOperation:
		return elemRegroups(e.LHS) || elemRegroups(e.RHS)
	}
	return false
}

// fetchCondition is one first-pass condition: the intrinsic it reads
// (IntrinsicNone for an ordinary attribute) and its operator.
type fetchCondition struct {
	intrinsic Intrinsic
	op        Operator
}

// fetchConditions accumulates the first-pass conditions and the
// AllConditions flag the way Tempo's FetchSpansRequest does.
type fetchConditions struct {
	conds []fetchCondition
	all   bool
}

func (c *fetchConditions) add(a Attribute, op Operator) {
	c.conds = append(c.conds, fetchCondition{intrinsic: a.Intrinsic, op: op})
}

// pipeline mirrors Pipeline.extractConditions: after a select() stage every
// remaining stage goes to the second pass, which this walk does not track.
func (c *fetchConditions) pipeline(p Pipeline) {
	for _, elem := range p.Elements {
		c.element(elem)
		if _, ok := elem.(SelectOperation); ok {
			return
		}
	}
}

func (c *fetchConditions) element(elem any) {
	if expr, ok := spansetFilterExpression(elem); ok {
		c.field(expr)
		return
	}
	switch e := elem.(type) {
	case GroupOperation:
		c.field(e.Expression)
		c.all = false
	case SpansetOperation:
		c.element(e.LHS)
		c.element(e.RHS)
		c.all = false
	case ScalarFilter:
		c.scalar(e.LHS)
		c.scalar(e.RHS)
	case Pipeline:
		c.pipeline(e)
	}
	// SelectOperation reads only in the second pass; CoalesceOperation
	// reads nothing.
}

func (c *fetchConditions) scalar(se ScalarExpression) {
	switch s := se.(type) {
	case Aggregate:
		c.all = false
		if s.e != nil {
			c.field(s.e)
		}
	case ScalarOperation:
		c.scalar(s.LHS)
		c.scalar(s.RHS)
	case Pipeline:
		c.pipeline(s)
	}
}

// field mirrors the FieldExpression extractConditions methods.
func (c *fetchConditions) field(fe FieldExpression) {
	switch e := fe.(type) {
	case *BinaryOperation:
		if e != nil {
			c.binary(e)
		}
	case UnaryOperation:
		c.unary(e)
	case Attribute:
		c.add(e, OpNone)
	}
}

// binary mirrors (*BinaryOperation).extractConditions: an attribute
// compared with a literal carries the operator, unless the fetch layer
// cannot build a predicate from it (a non-boolean operator, `!= nil`, or a
// boolean literal under && / ||); any other attribute operand is read with
// no predicate.
func (c *fetchConditions) binary(o *BinaryOperation) {
	switch l := o.LHS.(type) {
	case Attribute:
		switch r := o.RHS.(type) {
		case Static:
			c.add(l, literalConditionOp(o.Op, r))
		case Attribute:
			c.add(l, OpNone)
			c.add(r, OpNone)
		default:
			c.add(l, OpNone)
			c.field(o.RHS)
		}
	case Static:
		switch r := o.RHS.(type) {
		case Static:
		case Attribute:
			c.add(r, literalConditionOp(o.Op, l))
		default:
			c.field(o.RHS)
		}
	default:
		c.field(o.LHS)
		c.field(o.RHS)
		c.all = c.all && o.Op != OpOr
	}
}

// literalConditionOp is the operator an attribute-vs-literal comparison
// hands the fetch layer.
func literalConditionOp(op Operator, lit Static) Operator {
	switch {
	case (lit.Type == TypeNil && op == OpNotEqual) || !op.isBoolean():
		return OpNone
	case lit.Type == TypeBoolean && (op == OpOr || op == OpAnd):
		return OpNone
	}
	return op
}

// unary mirrors UnaryOperation.extractConditions: an existence test on an
// attribute carries its operator; anything else reads its operand.
func (c *fetchConditions) unary(o UnaryOperation) {
	if a, ok := o.Expression.(Attribute); ok && (o.Op == OpExists || o.Op == OpNotExists) {
		c.add(a, o.Op)
		return
	}
	c.field(o.Expression)
}
