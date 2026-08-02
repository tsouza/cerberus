package ast

// This file answers one question about a parsed query: does it name a
// given intrinsic anywhere? The Tempo response shaper needs it because
// upstream populates `spanSets[].spans[].name` exactly when the query
// referenced the `name` intrinsic, and leaves it empty otherwise.
//
// The scope is the WHOLE query — every pipeline stage plus the metrics
// first stage — and that is a deliberate mirror of upstream rather than a
// convenience. Upstream's `RootExpr.extractConditions`
// (pkg/traceql/ast_conditions.go:3) folds every stage of the query into a
// single FetchSpansRequest, so its storage layer materialises an
// intrinsic on every candidate span the moment any part of the query
// mentions it. There is no per-spanset-side narrowing to model:
// `{ name = "a" } >> { .x = 1 }` carries `name` on the returned
// descendant spans too, even though the descendant filter never named it.

// ReferencesIntrinsic reports whether the query names intrinsic in — in a
// spanset filter, a `by(...)` grouping, a `select(...)` list, an
// aggregate's inner expression, a scalar filter, or the metrics first
// stage. IntrinsicNone is never "referenced": it is the sentinel for an
// ordinary attribute name, so every non-intrinsic Attribute carries it.
func (r *RootExpr) ReferencesIntrinsic(in Intrinsic) bool {
	if r == nil || in == IntrinsicNone {
		return false
	}
	return pipelineRefsIntrinsic(r.Pipeline, in) ||
		firstStageRefsIntrinsic(r.MetricsPipeline, in)
	// MetricsSecondStage is intentionally absent: MetricsFilter,
	// TopKBottomK and ChainedSecondStage post-process the series values
	// produced by the first stage and carry no Attribute at all, so
	// there is nothing for the walk to reach.
}

func pipelineRefsIntrinsic(p Pipeline, in Intrinsic) bool {
	for _, elem := range p.Elements {
		if pipelineElemRefsIntrinsic(elem, in) {
			return true
		}
	}
	return false
}

func pipelineElemRefsIntrinsic(elem PipelineElement, in Intrinsic) bool {
	if expr, ok := spansetFilterExpression(elem); ok {
		return fieldRefsIntrinsic(expr, in)
	}
	switch e := elem.(type) {
	case SpansetOperation:
		return spansetRefsIntrinsic(e.LHS, in) || spansetRefsIntrinsic(e.RHS, in)
	case ScalarFilter:
		return scalarRefsIntrinsic(e.LHS, in) || scalarRefsIntrinsic(e.RHS, in)
	case GroupOperation:
		return fieldRefsIntrinsic(e.Expression, in)
	case SelectOperation:
		return attrsRefIntrinsic(e.Attrs(), in)
	case Aggregate:
		return fieldRefsIntrinsic(e.InnerExpr(), in)
	case Pipeline:
		return pipelineRefsIntrinsic(e, in)
	case CoalesceOperation:
		// `| coalesce()` re-shapes the spanset and names no attribute.
		return false
	}
	return false
}

func spansetRefsIntrinsic(se SpansetExpression, in Intrinsic) bool {
	if expr, ok := spansetFilterExpression(se); ok {
		return fieldRefsIntrinsic(expr, in)
	}
	switch s := se.(type) {
	case SpansetOperation:
		return spansetRefsIntrinsic(s.LHS, in) || spansetRefsIntrinsic(s.RHS, in)
	case ScalarFilter:
		return scalarRefsIntrinsic(s.LHS, in) || scalarRefsIntrinsic(s.RHS, in)
	case Pipeline:
		return pipelineRefsIntrinsic(s, in)
	}
	return false
}

func scalarRefsIntrinsic(se ScalarExpression, in Intrinsic) bool {
	switch s := se.(type) {
	case Pipeline:
		return pipelineRefsIntrinsic(s, in)
	case Aggregate:
		return fieldRefsIntrinsic(s.InnerExpr(), in)
	case ScalarOperation:
		return scalarRefsIntrinsic(s.LHS, in) || scalarRefsIntrinsic(s.RHS, in)
	case Static:
		// A literal names nothing.
		return false
	}
	return false
}

func fieldRefsIntrinsic(fe FieldExpression, in Intrinsic) bool {
	switch e := fe.(type) {
	case *BinaryOperation:
		if e == nil {
			return false
		}
		return fieldRefsIntrinsic(e.LHS, in) || fieldRefsIntrinsic(e.RHS, in)
	case UnaryOperation:
		return fieldRefsIntrinsic(e.Expression, in)
	case Attribute:
		return e.Intrinsic == in
	case Static:
		return false
	}
	return false
}

func firstStageRefsIntrinsic(fs FirstStageElement, in Intrinsic) bool {
	switch s := fs.(type) {
	case *MetricsAggregate:
		if s == nil {
			return false
		}
		return s.Attribute().Intrinsic == in || attrsRefIntrinsic(s.GroupBy(), in)
	case *AverageOverTimeAggregator:
		if s == nil {
			return false
		}
		return s.Attribute().Intrinsic == in || attrsRefIntrinsic(s.GroupBy(), in)
	case *MetricsCompare:
		if s == nil || s.Filter() == nil {
			return false
		}
		return fieldRefsIntrinsic(s.Filter().Expression, in)
	}
	return false
}

func attrsRefIntrinsic(attrs []Attribute, in Intrinsic) bool {
	for _, a := range attrs {
		if a.Intrinsic == in {
			return true
		}
	}
	return false
}

// spansetFilterExpression returns the filter expression behind either form
// of SpansetFilter. The parser builds the pointer form, but every method
// on SpansetFilter has a value receiver, so the value form satisfies
// PipelineElement and SpansetExpression too; accepting both keeps a
// hand-constructed AST from silently walking past a filter.
func spansetFilterExpression(e any) (FieldExpression, bool) {
	switch f := e.(type) {
	case *SpansetFilter:
		if f == nil {
			return nil, false
		}
		return f.Expression, true
	case SpansetFilter:
		return f.Expression, true
	}
	return nil, false
}
