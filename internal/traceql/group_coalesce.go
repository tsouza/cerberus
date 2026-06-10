// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file. group(...) and coalesce() lower exclusively
// through the upstream-fork-exposed public fields on
// traceql.GroupOperation / traceql.CoalesceOperation — no reflect, no
// unsafe.

package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

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
	key, err := lowerFieldExpr(g.Expression, s)
	if err != nil {
		return nil, err
	}

	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{key},
		GroupByAliases: []string{groupKeyAlias},
		AggFuncs: append([]chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}, Alias: aggTraceIDAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanIDColumn}}, Alias: s.SpanIDColumn},
		}, spansetEnvelopeAggFuncs(s)...),
	}, nil
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
