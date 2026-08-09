// This file (and select.go) read parser AST nodes exclusively via the
// typed accessors on cerberus's in-house TraceQL AST
// (internal/traceql/ast) — no reflection, no pointer aliasing tricks.

package traceql

import (
	"fmt"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

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
// merged ResourceAttributes, representative ParentSpanId, earliest
// Timestamp) onto the Aggregate's AggFunc list so the wrap projection
// (internal/api/tempo/handler.go: wrapWithSampleProjection's
// aggregate-shape branch) can surface a real per-trace summary instead
// of synthesising empty rootServiceName / rootTraceName fields.
//
// `aggResourceAttrsAlias` names an `any(ResourceAttributes)`
// projection on the inner Aggregate; the wrap-projection then merges
// it with `map('__cerberus_traceID', TraceId)` via mapConcat in an
// outer Project layer. This split keeps the Aggregate pure (group
// keys + aggregate calls only — no derived expressions wrapping both)
// while still threading the per-trace identity into the search
// envelope.
//
// `aggParentSpanIDAlias` names an `any(ParentSpanId)` projection
// piggybacked alongside SpanName / ResourceAttributes (issue #1481).
// ClickHouse evaluates every aggregate function in a SELECT list
// against the same input row per group-key update, so the three
// `any(...)` calls always read the same underlying span row — see
// anyAggFunc's doc comment. Threading that row's ParentSpanId into the
// search envelope (spansetAggregateSampleProjections in
// internal/api/tempo/handler.go) lets the existing /api/search
// root-resolution machinery (observeRoot / resolveTraceRoots /
// applyRootMetadata in internal/api/tempo) tell whether the
// arbitrarily-picked row happens to be the trace's true root — and, if
// not, fetch the real one via the same follow-up lookup every other
// /api/search shape already relies on. That correction is what turns
// "any span, could be the root, could be a random child, and which one
// isn't even stable across runs" into "always the true root
// eventually", without inventing a second notion of "root" in the
// aggregate path.
const (
	aggTraceIDAlias       = "TraceId"
	aggValueAlias         = "Value"
	aggMetricNameAlias    = "MetricName"
	aggResourceAttrsAlias = "ResourceAttrs"
	aggParentSpanIDAlias  = "ParentSpanId"
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
// FieldExpression via the in-house Aggregate.InnerExpr() accessor
// (internal/traceql/ast).
//
// Per-trace identity is preserved by grouping on TraceId and
// piggybacking representative envelope columns (SpanName,
// ResourceAttributes, ParentSpanId, Timestamp) via `any(...)` /
// `min(...)` aggregates so the search envelope surfaces real
// rootServiceName / rootTraceName / startTime values for each
// returned trace rather than collapsing the whole corpus into one row.
// The piggybacked ParentSpanId lets the /api/search shaper detect and
// correct the case where the arbitrarily-`any()`-picked row isn't
// actually the trace's root (see anyAggFunc's doc comment, issue #1481).
func lowerAggregate(prev chplan.Node, agg traceql.Aggregate, s schema.Traces) (chplan.Node, error) {
	valueFunc, needsNestedSet, err := scalarAggLeaf(agg, s, aggValueAlias)
	if err != nil {
		return nil, err
	}
	if needsNestedSet {
		prev = annotateNestedSet(prev, s)
	}
	return &chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{aggTraceIDAlias},
		AggFuncs: []chplan.AggFunc{
			valueFunc,
			anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
			anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
			anyAggFunc(s.ParentSpanIDColumn, aggParentSpanIDAlias),
			minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
			traceStartNsAggFunc(s.TimestampColumn),
			traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
		},
	}, nil
}

// scalarAggLeaf lowers a single TraceQL aggregate (`count()`,
// `sum(...)`, `avg(...)`, `min(...)`, `max(...)`) into the AggFunc that
// computes its value, under the caller-supplied alias. Factored out of
// lowerAggregate so lowerArithmeticScalarFilter (lower.go) can fold
// MULTIPLE aggregate leaves — one per operand of a `ScalarOperation`
// arithmetic tree, e.g. `max(duration) - min(duration)` — into the
// AggFuncs list of ONE shared chplan.Aggregate node instead of one
// independently-grouped Aggregate node per leaf, which would have no
// common row for the arithmetic to read both operands from.
//
// Reports whether the leaf reads a nested-set intrinsic
// (`min(nestedSetLeft)`), which has no flat OTel-CH column: the caller
// must wrap the aggregate's shared Input in annotateNestedSet before
// this AggFunc's Args can resolve. Reference Tempo materialises the
// same positions, so `/api/search` accepts it.
func scalarAggLeaf(agg traceql.Aggregate, s schema.Traces, alias string) (chplan.AggFunc, bool, error) {
	chFunc, err := mapAggregateOp(agg.Op())
	if err != nil {
		return chplan.AggFunc{}, false, err
	}
	if agg.Op() == traceql.AggregateCount {
		// count() takes no inner expression — aggregate a constant.
		return chplan.AggFunc{
			Name:  chFunc,
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: alias,
		}, false, nil
	}
	// sum/avg/max/min — read the inner FieldExpression via the fork
	// accessor and lower it.
	inner := agg.InnerExpr()
	if inner == nil {
		return chplan.AggFunc{}, false, fmt.Errorf("traceql: aggregate `%s` has nil inner expression", agg.Op())
	}
	if col, ok := nestedSetColumnForFieldExpr(inner); ok {
		return chplan.AggFunc{Name: chFunc, Args: []chplan.Expr{&chplan.ColumnRef{Name: col}}, Alias: alias}, true, nil
	}
	arg, err := lowerFieldExpr(inner, s)
	if err != nil {
		return chplan.AggFunc{}, false, err
	}

	// Map(String, String) coercion: when the aggregate input is a
	// FieldAccess against SpanAttributes / ResourceAttributes the value
	// is a String. ClickHouse refuses `max(String) > 100` with
	// NO_COMMON_TYPE; wrap in `toFloat64OrZero(...)` at lowering time so
	// the aggregate sees a Float64 and the downstream numeric comparison
	// resolves. Intrinsic ColumnRefs (Duration etc.) lower to a bare
	// ColumnRef and pass through unchanged.
	arg = coerceMapNumericAggInput(arg)

	return chplan.AggFunc{Name: chFunc, Args: []chplan.Expr{arg}, Alias: alias}, false, nil
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
// representative SpanName / ResourceAttributes / ParentSpanId value
// alongside the numeric Value. `any` picks an arbitrary row's value
// within the group; for ResourceAttributes that's fine because every
// span in a trace shares the same service identity in the OTel-CH
// layout (the resource map is denormalised per-span).
//
// For SpanName specifically, `any` alone would report an arbitrary
// span's name as the trace's root — nondeterministic across runs of
// the identical query, since ClickHouse's `any` is order-dependent
// across parts/threads (issue #1481). lowerAggregate compensates by
// also piggybacking `any(ParentSpanId)` (aggParentSpanIDAlias): CH
// evaluates every aggregate function in a query against the same
// current row per group-key update (a single pass over the input
// updates all accumulator states together), so the three `any(...)`
// calls are guaranteed to read the *same* underlying span row, not
// three independently-chosen ones. Piggybacking ParentSpanId lets the
// /api/search shaper (internal/api/tempo: observeRoot /
// resolveTraceRoots / applyRootMetadata) recognise when that row is
// not the true root and correct it via the same follow-up root-lookup
// query every other /api/search shape already uses — so the
// SQL-level arbitrariness of `any` never reaches the wire response.
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
