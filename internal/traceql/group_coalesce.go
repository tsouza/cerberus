// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file. group(...) and coalesce() lower exclusively
// through the upstream-fork-exposed public fields on
// traceql.GroupOperation / traceql.CoalesceOperation — no reflect, no
// unsafe.

package traceql

import (
	"fmt"
	"slices"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Aliases for the per-group representative columns emitted by
// `| group(...)` and `| coalesce()`. The /api/search response handler
// decodes these to rebuild spanset rows.
const (
	groupKeyAlias = "GroupKey"
)

// pipelineStage is one lowered pipeline stage in the two forms its readers
// take it in.
//
// Reference Tempo hands every stage the spansets the stage before it
// returned, and its metrics evaluator observes every span of each spanset
// the pipeline returns (grafana/tempo pkg/traceql/ast_execute.go,
// engine_metrics.go). A stage of cerberus's flat-row plan model, by
// contrast, may collapse its spans: a spanset aggregate to one row per
// trace, by() to one row per group, coalesce() to one row per span identity.
// Those rows are what the /api/search envelope reads off the last stage, and
// they carry neither the span columns nor the timestamp a following stage or
// a metrics function reads.
type pipelineStage struct {
	// node is the stage as the /api/search envelope reads it when the stage
	// ends the pipeline.
	node chplan.Node
	// spans is the span rows of every spanset the stage returns — what a
	// following stage, a metrics function and a spanset-operation operand
	// read. They carry every span column, except after select(), whose rows
	// carry the span identity, the timestamp and the selected attributes
	// alone (#4373).
	spans chplan.Node
	// groupKeys are the keys of the by() stages that split each trace's spans
	// into spansets, in pipeline order; empty means one spanset per trace.
	groupKeys []chplan.Expr
}

// lowerFollowingStage lowers el over the spansets prev returns.
//
//   - by(k) keeps every span and splits each spanset further by k, so the
//     stage's spans are prev's and its group keys gain k.
//   - coalesce() merges each trace's spansets back into one, so the stage's
//     spans are prev's with no group key.
//   - A spanset aggregate or scalar filter evaluates once per spanset —
//     per trace, or per group under a by() (groupPerSpanset) — and its
//     spans are the spans of the spansets it passes (spanGranularOperand).
//   - select(...) returns the spansets it reads, so it keeps prev's group
//     keys; its spans are its own span rows, which carry the selected
//     attributes a spanset-operation operand returns.
func lowerFollowingStage(prev pipelineStage, el traceql.PipelineElement, s schema.Traces) (pipelineStage, error) {
	if g, ok := asGroupOperation(el); ok {
		spans, key, err := groupSpans(prev.spans, g, s)
		if err != nil {
			return pipelineStage{}, err
		}
		keys := append(slices.Clip(prev.groupKeys), key)
		return pipelineStage{node: groupAggregate(spans, spansetGroupKey(keys), s), spans: spans, groupKeys: keys}, nil
	}
	if isCoalesceOperation(el) {
		return pipelineStage{node: lowerCoalesce(prev.spans, s), spans: prev.spans}, nil
	}
	node, err := lowerFollowingElement(prev.spans, el, s)
	if err != nil {
		return pipelineStage{}, err
	}
	if len(prev.groupKeys) > 0 {
		groupPerSpanset(node, spansetGroupKey(prev.groupKeys), s)
	}
	return pipelineStage{node: node, spans: spanGranularOperand(node, s), groupKeys: prev.groupKeys}, nil
}

// spansetGroupKey is the one GROUP BY key that stands for keys: the key
// itself for a single by(), and the tuple of the keys for chained by()
// stages, whose spansets are the spans sharing every key.
func spansetGroupKey(keys []chplan.Expr) chplan.Expr {
	if len(keys) == 1 {
		return keys[0]
	}
	return &chplan.FuncCall{Fn: chplan.FnTuple, Args: keys}
}

// groupPerSpanset regroups the spanset aggregate a stage after by() just
// lowered (lowerAggregate's GROUP BY TraceId, under any HAVING Filters) by
// key as well, so it evaluates once per by() group, as reference Tempo's
// Aggregate evaluates once per input spanset. key takes the GroupKey alias,
// the by() stage's own alias for it. The aggregate is the stage's own,
// built for it alone, so it is regrouped in place. A select() stage holds no
// aggregate and is left as it is.
func groupPerSpanset(node chplan.Node, key chplan.Expr, s schema.Traces) {
	agg := spansetAggregate(node, s)
	if agg == nil {
		return
	}
	agg.GroupBy = append(slices.Clip(agg.GroupBy), key)
	agg.GroupByAliases = append(slices.Clip(agg.GroupByAliases), groupKeyAlias)
}

// groupSpans returns the spans a `| by(<field-expr>)` stage groups, with
// every column its key reads, and that key.
//
// The by() stage itself lowers to groupAggregate over them: one
// representative span per (trace, group key), the shape the /api/search
// envelope reads.
func groupSpans(prev chplan.Node, g traceql.GroupOperation, s schema.Traces) (chplan.Node, chplan.Expr, error) {
	if g.Expression == nil {
		return nil, nil, fmt.Errorf("traceql: `| group(...)` requires a field expression")
	}
	// A group key on a nested-set intrinsic (`by(nestedSetParent)`) has
	// no flat OTel-CH column; recompute the numbering with a
	// NestedSetAnnotate pass and group by the synthetic column. Reference
	// Tempo materialises the same positions, so `/api/search` accepts it.
	if col, ok := nestedSetColumnForFieldExpr(g.Expression); ok {
		return annotateNestedSet(prev, s), &chplan.ColumnRef{Name: col}, nil
	}
	// A group key on a nested intrinsic (`by(event:name)`,
	// `by(link:traceID)`) reads the per-span Nested subfield array
	// (Events.Name, Links.TraceId). Reference Tempo groups spans by the
	// event/link value; cerberus groups by the Nested-array column so
	// `/api/search` returns 2xx rather than rejecting. The array column
	// is a valid GROUP BY key in ClickHouse (one group per distinct
	// per-span array).
	if col, sub, ok := nestedIntrinsicGroupTarget(g.Expression, s); ok {
		return prev, &chplan.ColumnRef{Name: col + "." + sub}, nil
	}
	// A key reading a trace-scoped intrinsic (`by(rootName)`,
	// `by(rootName = name)`) groups by each span's own trace's value, joined
	// onto the spans.
	key, err := lowerFieldExpr(g.Expression, s)
	if err != nil {
		return nil, nil, err
	}
	return withTraceScopedValues(prev, s, key), key, nil
}

// groupAggregate builds the per-group Aggregate a by() stage lowers to: the
// group key plus the representative envelope columns the Tempo /api/search
// wrap projection decodes.
//
// The aggregate piggybacks the same per-group envelope columns
// lowerAggregate carries (`any(SpanName) AS MetricName`,
// `any(ResourceAttributes) AS ResourceAttrs`, `min(Timestamp) AS
// TimeUnix`, TraceStartNs / TraceEndNs) so the Tempo handler's
// spanset-aggregate wrap projection (isSpansetAggregateShape →
// spansetAggregateSampleProjections) shapes the rows into real search
// summaries. Value is the per-group span count — the natural scalar for a
// grouped spanset (mirrors Tempo's UI which reports group sizes).
//
// The grouping is per trace: by() splits each trace's spanset into one
// spanset per distinct key value (Tempo's GroupOperation.evaluate groups the
// spans of one input spanset at a time), so two traces that share a key value
// stay two groups. TraceId therefore leads the GROUP BY.
func groupAggregate(prev chplan.Node, key chplan.Expr, s schema.Traces) chplan.Node {
	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}, key},
		GroupByAliases: []string{aggTraceIDAlias, groupKeyAlias},
		AggFuncs: append([]chplan.AggFunc{
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanIDColumn}}, Alias: s.SpanIDColumn},
		}, spansetEnvelopeAggFuncs(s)...),
	}
}

// nestedIntrinsicGroupTarget returns the (Nested column, subfield) a
// field expression resolves to when it is a bare nested intrinsic
// reference (event:name / link:traceID / link:spanID — the shapes a
// group key takes), or ok=false otherwise. Reuses nestedIntrinsicTarget
// (lower.go), the same mapping the comparison path uses.
func nestedIntrinsicGroupTarget(e traceql.FieldExpression, s schema.Traces) (col, sub string, ok bool) {
	a, ok := fieldExprAttribute(e)
	if !ok {
		return "", "", false
	}
	return nestedIntrinsicTarget(a, s)
}

// nestedSetColumnForFieldExpr returns the synthetic nested-set column a
// field expression resolves to when it is a bare nested-set intrinsic
// reference (the only shape group / aggregate keys take), or ok=false
// otherwise.
func nestedSetColumnForFieldExpr(e traceql.FieldExpression) (string, bool) {
	a, ok := fieldExprAttribute(e)
	if !ok {
		return "", false
	}
	return nestedSetColumn(a.Intrinsic)
}

// annotateNestedSet wraps n in a NestedSetAnnotate so the recursive
// numbering CTE materialises the synthetic nested-set columns. Shared by
// the group / aggregate key paths and lowerSpansetFilter.
func annotateNestedSet(n chplan.Node, s schema.Traces) chplan.Node {
	return nestedSetAnnotate(n, s)
}

// foldTrailingGroupByIntoMetrics rewrites `{...} | by(X) | <metric>()` so the
// trailing standalone `by(...)` grouping stages fold into the metric
// aggregate's by-clause — making the query lower identically to
// `{...} | <metric>() by (X)`. Returns the (possibly shortened) pipeline and
// the (possibly re-grouped) metrics first stage.
//
// Only the contiguous run of GroupOperation stages immediately before the
// metrics aggregate is folded, and only when each one's expression is a bare
// attribute (the shape a metrics by-clause can carry — `[]Attribute`). A
// non-attribute group key, a non-grouping stage in between, or a metrics first
// stage with no by-clause (compare()) stops the fold and leaves the pipeline
// untouched. mp is assumed non-nil (the caller gates on a metrics query).
//
// This is the fix for the standalone-`by()`-before-metrics shape that
// otherwise lowers to a Timestamp-stripping GROUP BY feeding the rate grid
// (ClickHouse code 47 / a 502); see lowerRoot.
func foldTrailingGroupByIntoMetrics(pipeline traceql.Pipeline, mp traceql.FirstStageElement) (traceql.Pipeline, traceql.FirstStageElement) {
	els := pipeline.Elements
	keep := len(els)
	var folded []traceql.Attribute
	for keep > 0 {
		g, ok := asGroupOperation(els[keep-1])
		if !ok {
			break
		}
		attr, ok := fieldExprAttribute(g.Expression)
		if !ok {
			// A non-attribute group key (`by(span.a + span.b)`) cannot be
			// represented in a metrics by-clause; leave the whole pipeline as-is.
			return pipeline, mp
		}
		// Prepend so the recovered slice stays in textual (left-to-right) order.
		folded = append([]traceql.Attribute{attr}, folded...)
		keep--
	}
	if len(folded) == 0 {
		return pipeline, mp
	}
	merged := mergeGroupByIntoMetrics(mp, folded)
	if merged == nil {
		// Metrics first stage carries no by-clause (compare()): nothing to fold
		// into; keep the original pipeline so behaviour is unchanged.
		return pipeline, mp
	}
	return traceql.Pipeline{Elements: els[:keep]}, merged
}

// asGroupOperation unwraps a pipeline element into a GroupOperation when it is
// one (value or pointer form, matching lowerPipelineElement's two cases).
func asGroupOperation(el traceql.PipelineElement) (traceql.GroupOperation, bool) {
	switch v := el.(type) {
	case traceql.GroupOperation:
		return v, true
	case *traceql.GroupOperation:
		return *v, true
	}
	return traceql.GroupOperation{}, false
}

// isCoalesceOperation reports whether el is a coalesce() stage, in value or
// pointer form.
func isCoalesceOperation(el traceql.PipelineElement) bool {
	switch el.(type) {
	case traceql.CoalesceOperation, *traceql.CoalesceOperation:
		return true
	}
	return false
}

// mergeGroupByIntoMetrics returns mp with attrs prepended to its by-clause, or
// nil when mp is a metrics first stage that has no by-clause (compare()).
func mergeGroupByIntoMetrics(mp traceql.FirstStageElement, attrs []traceql.Attribute) traceql.FirstStageElement {
	switch v := mp.(type) {
	case *traceql.MetricsAggregate:
		return v.WithLeadingGroupBy(attrs)
	case *traceql.AverageOverTimeAggregator:
		return v.WithLeadingGroupBy(attrs)
	}
	return nil
}

// lowerCoalesce handles `| coalesce()` — Tempo's spanset-flattening
// pipeline element. In Tempo's runtime, coalesce() merges the spansets of
// each trace (typically the groups of a preceding `by(...)`) into a single
// spanset holding all of their spans; a stage after it reads those spans
// unchanged (lowerFollowingStage).
//
// As the last stage, coalesce() lowers to a `DISTINCT (TraceId, SpanId)`
// Aggregate that groups by the span-identity columns and keeps one row per
// span, the rows the /api/search envelope reads.
//
// Like groupAggregate, the aggregate piggybacks the spanset-envelope
// columns so the search wrap projection produces real summaries
// instead of referencing a non-existent `Value` column. Value here is
// the per-(TraceId, SpanId) row count — i.e. how many duplicate rows
// the dedup collapsed.
func lowerCoalesce(prev chplan.Node, s schema.Traces) chplan.Node {
	return &chplan.Aggregate{
		Input: prev,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.TraceIDColumn},
			&chplan.ColumnRef{Name: s.SpanIDColumn},
		},
		GroupByAliases: []string{s.TraceIDColumn, s.SpanIDColumn},
		AggFuncs:       spansetEnvelopeAggFuncs(s),
	}
}

// spansetEnvelopeAggFuncs returns the per-group envelope AggFunc list
// shared by group() / coalesce(): the count-shaped Value plus the
// envelope columns lowerAggregate (aggregate.go) established — the
// alias set isSpansetAggregateShape keys on. Includes
// `any(ParentSpanId) AS ParentSpanId` alongside SpanName /
// ResourceAttributes for the same reason aggregate.go does (issue
// #1481 — see anyAggFunc's doc comment): it lets the /api/search
// root-resolution machinery detect and correct an arbitrarily-picked
// non-root representative row instead of surfacing it unverified.
func spansetEnvelopeAggFuncs(s schema.Traces) []chplan.AggFunc {
	return []chplan.AggFunc{
		{Fn: chplan.FnCount, Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: aggValueAlias},
		anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
		anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
		anyAggFunc(s.ParentSpanIDColumn, aggParentSpanIDAlias),
		minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
		traceStartNsAggFunc(s.TimestampColumn),
		traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
	}
}
