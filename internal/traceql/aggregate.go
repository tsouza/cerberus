// This file (and select.go) read parser AST nodes exclusively via the
// upstream-fork-exposed accessors on github.com/tsouza/tempo:cerberus-accessors
// — no reflection, no pointer aliasing tricks. See docs/upstream-forks.md.

package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Aggregate output-column aliases for the second-stage spanset
// aggregates (`| count()`, `| sum(...)`, `| avg(...)`, `| max(...)`,
// `| min(...)`).
//
// TraceQL semantics make the spanset aggregates trace-scoped: `{ ... }
// | count() > 0` returns one row per matching trace, NOT a single
// corpus-wide row. The lowering therefore groups by TraceId and
// piggybacks the per-trace envelope columns (representative SpanName,
// merged ResourceAttributes, earliest Timestamp) onto the Aggregate's
// AggFunc list so the wrap projection (internal/api/tempo/handler.go:
// wrapWithSampleProjection's aggregate-shape branch) can surface a
// real per-trace summary instead of synthesising empty
// rootServiceName / rootTraceName fields.
//
// `aggResourceAttrsAlias` names an `any(ResourceAttributes)`
// projection on the inner Aggregate; the wrap-projection then merges
// it with `map('__cerberus_traceID', TraceId)` via mapConcat in an
// outer Project layer. This split keeps the Aggregate pure (group
// keys + aggregate calls only — no derived expressions wrapping both)
// while still threading the per-trace identity into the search
// envelope.
const (
	aggTraceIDAlias       = "TraceId"
	aggValueAlias         = "Value"
	aggMetricNameAlias    = "MetricName"
	aggResourceAttrsAlias = "ResourceAttrs"
	aggTimeUnixAlias      = "TimeUnix"
	// aggTraceStartNsAlias / aggTraceEndNsAlias name the two per-trace
	// timestamp aggregates used to derive the per-trace duration. Tempo's
	// /api/search returns `durationMs` as the **whole-trace** wall-clock
	// span — `max(span.end) - min(span.start)` across every span in the
	// trace — not the matched span's own duration. The shaper subtracts
	// the two aliases in the wrap-projection layer (see
	// internal/api/tempo/handler.go: spansetAggregateSampleProjections).
	aggTraceStartNsAlias = "TraceStartNs"
	aggTraceEndNsAlias   = "TraceEndNs"
)

// lowerAggregate handles `| count()`, `| sum(...)`, `| avg(...)`,
// `| max(...)`, `| min(...)`. count() has no inner expression — we
// aggregate the constant 1 per row. The other four read the inner
// FieldExpression via the upstream-fork-exposed Aggregate.InnerExpr()
// accessor (github.com/tsouza/tempo:cerberus-accessors) — see
// docs/upstream-forks.md.
//
// Per-trace identity is preserved by grouping on TraceId and
// piggybacking representative envelope columns (SpanName,
// ResourceAttributes, Timestamp) via `any(...)` / `min(...)`
// aggregates so the search envelope surfaces real
// rootServiceName / rootTraceName / startTime values for each
// returned trace rather than collapsing the whole corpus into one row.
func lowerAggregate(prev chplan.Node, agg traceql.Aggregate, s schema.Traces) (chplan.Node, error) {
	chFunc, err := mapAggregateOp(agg.Op())
	if err != nil {
		return nil, err
	}

	var valueFunc chplan.AggFunc
	if agg.Op() == traceql.AggregateCount {
		// count() takes no inner expression — aggregate a constant.
		valueFunc = chplan.AggFunc{
			Name:  chFunc,
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: aggValueAlias,
		}
	} else {
		// sum/avg/max/min — read the inner FieldExpression via the fork
		// accessor and lower it.
		inner := agg.InnerExpr()
		if inner == nil {
			return nil, fmt.Errorf("traceql: aggregate `%s` has nil inner expression", agg.Op())
		}
		// An aggregate over a nested-set intrinsic (`min(nestedSetLeft)`)
		// has no flat OTel-CH column; recompute the numbering with a
		// NestedSetAnnotate pass over the input and aggregate the
		// synthetic column. Reference Tempo materialises the same
		// positions, so `/api/search` accepts it.
		if col, ok := nestedSetColumnForFieldExpr(inner); ok {
			prev = annotateNestedSet(prev, s)
			return &chplan.Aggregate{
				Input:          prev,
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
				GroupByAliases: []string{aggTraceIDAlias},
				AggFuncs: []chplan.AggFunc{
					{Name: chFunc, Args: []chplan.Expr{&chplan.ColumnRef{Name: col}}, Alias: aggValueAlias},
					anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
					anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
					minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
					traceStartNsAggFunc(s.TimestampColumn),
					traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
				},
			}, nil
		}
		arg, err := lowerFieldExpr(inner, s)
		if err != nil {
			return nil, err
		}

		// Map(String, String) coercion: when the aggregate input is a
		// FieldAccess against SpanAttributes / ResourceAttributes the value
		// is a String. ClickHouse refuses `max(String) > 100` with
		// NO_COMMON_TYPE; wrap in `toFloat64OrZero(...)` at lowering time so
		// the aggregate sees a Float64 and the downstream numeric
		// comparison resolves. Intrinsic ColumnRefs (Duration etc.) lower
		// to a bare ColumnRef and pass through unchanged.
		arg = coerceMapNumericAggInput(arg)

		valueFunc = chplan.AggFunc{
			Name:  chFunc,
			Args:  []chplan.Expr{arg},
			Alias: aggValueAlias,
		}
	}

	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{aggTraceIDAlias},
		AggFuncs: []chplan.AggFunc{
			valueFunc,
			anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
			anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
			minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
			traceStartNsAggFunc(s.TimestampColumn),
			traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
		},
	}, nil
}

// traceStartNsAggFunc returns `min(toUnixTimestamp64Nano(<Timestamp>))
// AS TraceStartNs` — the earliest span-start across the trace, in
// nanoseconds since the Unix epoch. Paired with traceEndNsAggFunc so
// the wrap-projection can derive the per-trace wall-clock duration
// (max(end) - min(start)) for Tempo's `durationMs` field.
//
// The cast to Int64 ns is what lets the difference fall out as a
// plain integer — `min(Timestamp)` returns DateTime64 and the typed
// chplan Binary lacks an interval-aware subtraction, so we project
// the value as nanoseconds up-front.
func traceStartNsAggFunc(timestampColumn string) chplan.AggFunc {
	return chplan.AggFunc{
		Name: "min",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "toUnixTimestamp64Nano",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: timestampColumn}},
			},
		},
		Alias: aggTraceStartNsAlias,
	}
}

// traceEndNsAggFunc returns `max(toUnixTimestamp64Nano(<Timestamp>) +
// toInt64(<Duration>)) AS TraceEndNs` — the latest span-end across
// the trace, in nanoseconds since the Unix epoch. The OTel-CH
// `Duration` column is UInt64 nanoseconds; coercing to Int64 keeps
// the sum Int64 so CH does not promote to a wider unsigned type that
// `min(...)` would refuse to subtract.
//
// Pairing this with traceStartNsAggFunc lets the wrap-projection
// derive the per-trace wall-clock duration as the simple integer
// difference `TraceEndNs - TraceStartNs` (see
// internal/api/tempo/handler.go: spansetAggregateSampleProjections).
func traceEndNsAggFunc(timestampColumn, durationColumn string) chplan.AggFunc {
	return chplan.AggFunc{
		Name: "max",
		Args: []chplan.Expr{
			&chplan.Binary{
				Op: chplan.OpAdd,
				Left: &chplan.FuncCall{
					Name: "toUnixTimestamp64Nano",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: timestampColumn}},
				},
				Right: &chplan.FuncCall{
					Name: "toInt64",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: durationColumn}},
				},
			},
		},
		Alias: aggTraceEndNsAlias,
	}
}

// anyAggFunc returns an `any(<col>) AS <alias>` AggFunc — the
// per-trace envelope helper used by lowerAggregate to surface a
// representative SpanName / ResourceAttributes value alongside the
// numeric Value. `any` picks an arbitrary row's value within the
// group; for ResourceAttributes that's fine because every span in a
// trace shares the same service identity in the OTel-CH layout (the
// resource map is denormalised per-span). For SpanName a real
// implementation would surface the root span via `argMin(SpanName,
// Timestamp)`; we use `any` for the first cut so the canonical-row
// shape is identical regardless of the inner span-set.
func anyAggFunc(col, alias string) chplan.AggFunc {
	return chplan.AggFunc{
		Name:  "any",
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: col}},
		Alias: alias,
	}
}

// minAggFunc returns a `min(<col>) AS <alias>` AggFunc — used to
// derive the per-trace earliest Timestamp for the search envelope's
// startTimeUnixNano field.
func minAggFunc(col, alias string) chplan.AggFunc {
	return chplan.AggFunc{
		Name:  "min",
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: col}},
		Alias: alias,
	}
}

// coerceMapNumericAggInput wraps Map-subscript expressions
// (`SpanAttributes['foo']`, `ResourceAttributes['foo']`) with
// `toFloat64OrZero(...)` so they can flow into a numeric CH aggregate
// (`max`/`min`/`sum`/`avg`/`quantiles`). The OTel-CH attribute carriers
// are typed `Map(String, String)`, so a bare subscript returns String —
// CH then refuses to compare the aggregate against a numeric literal
// with NO_COMMON_TYPE.
//
// The `OrZero` variant silently coerces strings that don't parse as
// numbers (matches Loki's silent-fallback for typed label filters via
// PR #479).
//
// Pass-through for everything else: intrinsic ColumnRefs (Duration,
// already Int64) need no cast; pre-wrapped FuncCalls (e.g. an
// arithmetic Binary that was already coerced) keep their existing
// shape.
func coerceMapNumericAggInput(expr chplan.Expr) chplan.Expr {
	if _, ok := expr.(*chplan.FieldAccess); ok {
		return &chplan.FuncCall{
			Name: "toFloat64OrZero",
			Args: []chplan.Expr{expr},
		}
	}
	return expr
}

// mapAggregateOp turns a TraceQL AggregateOp into the CH agg function
// name. count / max / min / sum / avg map 1:1.
func mapAggregateOp(op traceql.AggregateOp) (string, error) {
	switch op {
	case traceql.AggregateCount:
		return "count", nil
	case traceql.AggregateMax:
		return "max", nil
	case traceql.AggregateMin:
		return "min", nil
	case traceql.AggregateSum:
		return "sum", nil
	case traceql.AggregateAvg:
		return "avg", nil
	}
	return "", fmt.Errorf("traceql: aggregate op %q is unsupported", op)
}
