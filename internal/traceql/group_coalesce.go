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
//	AggFuncs: [ any(TraceId) AS TraceId,
//	            any(SpanId) AS SpanId,
//	            min(Timestamp) AS Timestamp ] }
//
// The earliest span per group (min(Timestamp)) is the natural
// representative for trace UIs; `any()` for the identity columns picks
// a deterministic-per-group row (CH's `any` is implementation-defined
// but stable within a query). Future work: argMin(SpanId, Timestamp)
// once the optimizer can prove the group-key resolves to a single row.
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
		AggFuncs: []chplan.AggFunc{
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}, Alias: s.TraceIDColumn},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SpanIDColumn}}, Alias: s.SpanIDColumn},
			{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: s.TimestampColumn},
		},
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
func lowerCoalesce(prev chplan.Node, s schema.Traces) (chplan.Node, error) {
	return &chplan.Aggregate{
		Input: prev,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.TraceIDColumn},
			&chplan.ColumnRef{Name: s.SpanIDColumn},
		},
		GroupByAliases: []string{s.TraceIDColumn, s.SpanIDColumn},
		AggFuncs: []chplan.AggFunc{
			{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}, Alias: s.TimestampColumn},
		},
	}, nil
}
