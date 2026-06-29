package ast

// Compile-time assertions that each concrete node satisfies the grammar
// position(s) it is allowed to occupy. These pin the interface contract so
// the parser and the lowering migration can rely on it.
var (
	_ FieldExpression = Static{}
	_ FieldExpression = Attribute{}
	_ FieldExpression = (*BinaryOperation)(nil)
	_ FieldExpression = UnaryOperation{}

	_ ScalarExpression = Static{}
	_ ScalarExpression = Pipeline{}
	_ ScalarExpression = Aggregate{}
	_ ScalarExpression = ScalarOperation{}

	_ SpansetExpression = Pipeline{}
	_ SpansetExpression = SpansetFilter{}
	_ SpansetExpression = SpansetOperation{}
	_ SpansetExpression = ScalarFilter{}

	_ PipelineElement = Pipeline{}
	_ PipelineElement = SpansetFilter{}
	_ PipelineElement = SpansetOperation{}
	_ PipelineElement = ScalarFilter{}
	_ PipelineElement = GroupOperation{}
	_ PipelineElement = CoalesceOperation{}
	_ PipelineElement = SelectOperation{}
	_ PipelineElement = Aggregate{}

	_ FirstStageElement = (*MetricsAggregate)(nil)
	_ FirstStageElement = (*AverageOverTimeAggregator)(nil)
	_ FirstStageElement = (*MetricsCompare)(nil)

	_ SecondStageElement = (*MetricsFilter)(nil)
	_ SecondStageElement = (*TopKBottomK)(nil)
	_ SecondStageElement = (*ChainedSecondStage)(nil)
)
