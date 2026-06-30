// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file. group(...) and coalesce() lower exclusively
// through the upstream-fork-exposed public fields on
// traceql.GroupOperation / traceql.CoalesceOperation — no reflect, no
// unsafe.

package traceql

import (
	"fmt"

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

// lowerGroup handles `| group(<field-expr>)` — the TraceQL grouping
// pipeline element that collapses results into one representative span
// per group key.
//
// SQL shape: Aggregate { Input: prev, GroupBy: [<key-expr>],
//
//	AggFuncs: [ any(TraceId) AS TraceId, any(SpanId) AS SpanId,
//	            count(1) AS Value, + the spanset-envelope columns ] }
//
// The aggregate piggybacks the same per-group envelope columns
// lowerAggregate carries (`any(SpanName) AS MetricName`,
// `any(ResourceAttributes) AS ResourceAttrs`, `min(Timestamp) AS
// TimeUnix`, TraceStartNs / TraceEndNs) so the Tempo handler's
// spanset-aggregate wrap projection (isSpansetAggregateShape →
// spansetAggregateSampleProjections) shapes the rows into real search
// summaries. Before this, the by() output fell into the generic
// metrics-fallback wrap which referenced a `Value` column the
// aggregate never projected — every `{} | by(...)` search 502'd with
// "Unknown identifier Value".
//
// Value is the per-group span count — the natural scalar for a
// grouped spanset (mirrors Tempo's UI which reports group sizes).
func lowerGroup(prev chplan.Node, g traceql.GroupOperation, s schema.Traces) (chplan.Node, error) {
	if g.Expression == nil {
		return nil, fmt.Errorf("traceql: `| group(...)` requires a field expression")
	}
	// A group key on a nested-set intrinsic (`by(nestedSetParent)`) has
	// no flat OTel-CH column; recompute the numbering with a
	// NestedSetAnnotate pass and group by the synthetic column. Reference
	// Tempo materialises the same positions, so `/api/search` accepts it.
	if col, ok := nestedSetColumnForFieldExpr(g.Expression); ok {
		prev = annotateNestedSet(prev, s)
		return groupAggregate(prev, &chplan.ColumnRef{Name: col}, s), nil
	}
	// A group key on a nested intrinsic (`by(event:name)`,
	// `by(link:traceID)`) reads the per-span Nested subfield array
	// (Events.Name, Links.TraceId). Reference Tempo groups spans by the
	// event/link value; cerberus groups by the Nested-array column so
	// `/api/search` returns 2xx rather than rejecting. The array column
	// is a valid GROUP BY key in ClickHouse (one group per distinct
	// per-span array).
	if col, sub, ok := nestedIntrinsicGroupTarget(g.Expression, s); ok {
		return groupAggregate(prev, &chplan.ColumnRef{Name: col + "." + sub}, s), nil
	}
	key, err := lowerFieldExpr(g.Expression, s)
	if err != nil {
		return nil, err
	}
	return groupAggregate(prev, key, s), nil
}

// groupAggregate builds the per-group Aggregate shared by lowerGroup:
// the group key plus the representative envelope columns the Tempo
// /api/search wrap projection decodes.
func groupAggregate(prev chplan.Node, key chplan.Expr, s schema.Traces) chplan.Node {
	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{key},
		GroupByAliases: []string{groupKeyAlias},
		AggFuncs: append([]chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}, Alias: aggTraceIDAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanIDColumn}}, Alias: s.SpanIDColumn},
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
	return &chplan.NestedSetAnnotate{
		Input:              n,
		SpansTable:         s.SpansTable,
		TraceIDColumn:      s.TraceIDColumn,
		SpanIDColumn:       s.SpanIDColumn,
		ParentSpanIDColumn: s.ParentSpanIDColumn,
		TimestampColumn:    s.TimestampColumn,
	}
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
// pipeline element. In Tempo's runtime, coalesce() merges results that
// span multiple spansets (typically the output of a set-op like
// `A || B`) into a single spanset with duplicates removed.
//
// In cerberus's flat-row plan model the multi-spanset concept doesn't
// exist — every prior stage emits rows already. We faithfully model
// coalesce() as a `DISTINCT (TraceId, SpanId)` dedup via an Aggregate
// that groups by the span-identity columns and keeps one row per group.
// For inputs that don't have duplicates (the common single-spanset
// case) the optimizer can fold the no-op grouping in a later pass.
//
// Like lowerGroup, the aggregate piggybacks the spanset-envelope
// columns so the search wrap projection produces real summaries
// instead of referencing a non-existent `Value` column. Value here is
// the per-(TraceId, SpanId) row count — i.e. how many duplicate rows
// the dedup collapsed.
func lowerCoalesce(prev chplan.Node, s schema.Traces) (chplan.Node, error) {
	return &chplan.Aggregate{
		Input: prev,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.TraceIDColumn},
			&chplan.ColumnRef{Name: s.SpanIDColumn},
		},
		GroupByAliases: []string{s.TraceIDColumn, s.SpanIDColumn},
		AggFuncs:       spansetEnvelopeAggFuncs(s),
	}, nil
}

// spansetEnvelopeAggFuncs returns the per-group envelope AggFunc list
// shared by group() / coalesce(): the count-shaped Value plus the four
// envelope columns lowerAggregate (aggregate.go) established — the
// alias set isSpansetAggregateShape keys on.
func spansetEnvelopeAggFuncs(s schema.Traces) []chplan.AggFunc {
	return []chplan.AggFunc{
		{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: aggValueAlias},
		anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
		anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
		minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
		traceStartNsAggFunc(s.TimestampColumn),
		traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
	}
}
