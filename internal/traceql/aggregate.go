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
	leaf, err := scalarAggLeaf(agg, s, aggValueAlias)
	if err != nil {
		return nil, err
	}
	prev = leaf.prepareInput(prev, s)
	// spansetEnvelopeAggFuncs (group_coalesce.go) returns the same
	// envelope tail group()/coalesce() use, with a count-shaped Value
	// in slot 0; swap in this aggregate's own valueFunc so the two
	// AggFunc lists can't drift out of lock-step.
	envelopeAggFuncs := spansetEnvelopeAggFuncs(s)
	envelopeAggFuncs[0] = leaf.fn
	node := chplan.Node(&chplan.Aggregate{
		Input:          prev,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{aggTraceIDAlias},
		AggFuncs:       envelopeAggFuncs,
	})
	if leaf.nullable {
		node = dropNullValueRows(node, aggValueAlias)
	}
	return node, nil
}

// scalarAggLeafResult is what scalarAggLeaf computes about one aggregate:
// the AggFunc itself plus the two facts its caller must act on before the
// AggFunc can be used. Grouped into a struct rather than returned as a
// widening tuple so neither flag can be silently transposed at the call
// site.
type scalarAggLeafResult struct {
	fn chplan.AggFunc
	// needsNestedSet is true when the leaf reads a nested-set intrinsic
	// (`min(nestedSetLeft)`), which has no flat OTel-CH column: the
	// aggregate's Input must be wrapped in annotateNestedSet before the
	// AggFunc's Args can resolve.
	needsNestedSet bool
	// nullable is true when the leaf reads a Map-carried attribute and so
	// went through coerceMapNumericAggInput's toFloat64OrNull wrap: every
	// span it skipped contributes nothing, so the aggregate answers NULL
	// for a trace whose spans were ALL skipped — the case reference Tempo
	// drops out of the answer entirely. See dropNullValueRows.
	nullable bool
}

// prepareInput returns the aggregate Input the leaf needs — the caller's
// node, wrapped in the nested-set annotation when the leaf reads one.
func (r scalarAggLeafResult) prepareInput(prev chplan.Node, s schema.Traces) chplan.Node {
	if r.needsNestedSet {
		return annotateNestedSet(prev, s)
	}
	return prev
}

// scalarAggLeaf lowers a single TraceQL aggregate (`count()`,
// `sum(...)`, `avg(...)`, `min(...)`, `max(...)`) into the AggFunc that
// computes its value, under the caller-supplied alias. Kept factored
// out of lowerAggregate, its one caller, because the alias is a
// parameter rather than a constant: the split is what keeps the
// value-column naming decision at the call site.
//
// Reports whether the leaf reads a nested-set intrinsic
// (`min(nestedSetLeft)`), which has no flat OTel-CH column: the caller
// must wrap the aggregate's shared Input in annotateNestedSet before
// this AggFunc's Args can resolve. Reference Tempo materialises the
// same positions, so `/api/search` accepts it.
func scalarAggLeaf(agg traceql.Aggregate, s schema.Traces, alias string) (scalarAggLeafResult, error) {
	chFunc, err := mapAggregateOp(agg.Op())
	if err != nil {
		return scalarAggLeafResult{}, err
	}
	if agg.Op() == traceql.AggregateCount {
		// count() takes no inner expression — aggregate a constant.
		return scalarAggLeafResult{fn: chplan.AggFunc{
			Fn:    chFunc,
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: alias,
		}}, nil
	}
	// sum/avg/max/min — read the inner FieldExpression via the fork
	// accessor and lower it.
	inner := agg.InnerExpr()
	if inner == nil {
		return scalarAggLeafResult{}, fmt.Errorf("traceql: aggregate `%s` has nil inner expression", agg.Op())
	}
	if col, ok := nestedSetColumnForFieldExpr(inner); ok {
		return scalarAggLeafResult{
			fn:             chplan.AggFunc{Fn: chFunc, Args: []chplan.Expr{&chplan.ColumnRef{Name: col}}, Alias: alias},
			needsNestedSet: true,
		}, nil
	}
	arg, err := lowerFieldExpr(inner, s)
	if err != nil {
		return scalarAggLeafResult{}, err
	}

	// Map(String, String) coercion: when the aggregate input is a
	// FieldAccess against SpanAttributes / ResourceAttributes the value
	// is a String. ClickHouse refuses `max(String) > 100` with
	// NO_COMMON_TYPE; wrap in `toFloat64OrNull(...)` at lowering time so
	// the aggregate sees a Float64 and the downstream numeric comparison
	// resolves. Intrinsic ColumnRefs (Duration etc.) lower to a bare
	// ColumnRef and pass through unchanged.
	arg, nullable := coerceMapNumericAggInput(arg)

	return scalarAggLeafResult{
		fn:       chplan.AggFunc{Fn: chFunc, Args: []chplan.Expr{arg}, Alias: alias},
		nullable: nullable,
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
		Fn: chplan.FnMin,
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Fn:   chplan.FnToUnixNanos,
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
		Fn: chplan.FnMax,
		Args: []chplan.Expr{
			&chplan.Binary{
				Op: chplan.OpAdd,
				Left: &chplan.FuncCall{
					Fn:   chplan.FnToUnixNanos,
					Args: []chplan.Expr{&chplan.ColumnRef{Name: timestampColumn}},
				},
				Right: &chplan.FuncCall{
					Fn:   chplan.FnToInt64,
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
		Fn:    chplan.FnAny,
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: col}},
		Alias: alias,
	}
}

// minAggFunc returns a `min(<col>) AS <alias>` AggFunc — used to
// derive the per-trace earliest Timestamp for the search envelope's
// startTimeUnixNano field.
func minAggFunc(col, alias string) chplan.AggFunc {
	return chplan.AggFunc{
		Fn:    chplan.FnMin,
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: col}},
		Alias: alias,
	}
}

// coerceMapNumericAggInput wraps Map-subscript expressions
// (`SpanAttributes['foo']`, `ResourceAttributes['foo']`) with
// `toFloat64OrNull(...)` so they can flow into a numeric CH aggregate
// (`max`/`min`/`sum`/`avg`/`quantiles`). The OTel-CH attribute carriers
// are typed `Map(String, String)`, so a bare subscript returns String —
// CH then refuses to compare the aggregate against a numeric literal
// with NO_COMMON_TYPE. Reports whether it wrapped, so the caller knows
// the aggregate's result is now Nullable.
//
// Why `OrNull` and not `OrZero`: the map subscript yields ” for a key
// the span never carried. Reference Tempo does not fold that span into
// the aggregate — it SKIPS it, on both paths. The spanset aggregates
// test `val.IsNil()` and `continue` — the guard each of
// pkg/traceql/ast_execute.go's Aggregate.evaluate avg/max/min/sum arms
// opens its span loop with; the metrics path funnels the read through
// FloatizeAttribute (pkg/traceql/engine_metrics.go), which answers
// TypeNil for the missing key, and NewOverTimeAggregator turns TypeNil
// into the NaN sentinel its reducers skip (that constructor's default
// getSpanAttValue closure in engine_metrics.go,
// engine_metrics_functions.go). `toFloat64OrNull` reproduces that: NULL,
// which every ClickHouse aggregate ignores. `OrZero` instead folded the
// attribute-less span in as a real 0 — `avg(span.size)` over spans
// carrying 10 and 20 plus one carrying nothing answered 10 where
// reference answers 15, and `min` answered 0 where reference answers 10.
//
// The wrap also nulls a key that IS present but whose value is not a
// number. On the metrics path that is again exactly reference:
// FloatizeAttribute maps the NaN that Static.Float returns for every
// non-numeric type (ast.go) back to TypeNil. On the spanset-aggregate
// path reference instead keeps such a span — a TypeString value is not
// nil — and then cannot do arithmetic with it: sumInto returns without
// summing when the two Statics disagree on type while the avg divisor
// still counts the span, and compare falls back to ordering by type
// ordinal (ast.go). The result is not a number the attribute ever
// carried. Skipping is the only reading that keeps a numeric aggregate
// numeric, so cerberus skips on both paths rather than reproducing that.
//
// Skipping also matches what the FILTER path in this package already
// does — coerceFieldAccess in lower.go wraps with `toFloat64OrNull`, and
// its doc comment derives the same reference semantics for comparisons.
// The previous `OrZero` here justified itself by analogy to Loki's
// typed-label-filter fallback, which is a different language with a
// different reference engine and says nothing about what Tempo answers.
//
// Pass-through for everything else: intrinsic ColumnRefs (Duration,
// already Int64) need no cast; pre-wrapped FuncCalls (e.g. an
// arithmetic Binary that was already coerced) keep their existing
// shape.
func coerceMapNumericAggInput(expr chplan.Expr) (chplan.Expr, bool) {
	if isAttributeRead(expr) {
		return &chplan.FuncCall{
			Fn:   chplan.FnToFloat64OrNull,
			Args: []chplan.Expr{expr},
		}, true
	}
	return expr, false
}

// dropNullValueRows wraps agg in the `isNotNull(<alias>)` filter that drops a
// trace whose every span was skipped by the NULL coercion above.
//
// Reference Tempo drops the spanset outright in that case: each of the four
// spanset aggregates starts from a nil accumulator and, having skipped every
// span, hits the post-loop `if sum == nil { continue }` / `maxS == nil` /
// `minS == nil` guard each of pkg/traceql/ast_execute.go's
// Aggregate.evaluate avg/max/min/sum arms closes with — the trace never
// reaches the response. ClickHouse instead emits the group with a NULL
// value, because a GROUP BY key with rows still produces a row; this filter
// is what turns that NULL back into "no row".
//
// It matters even though `{} | avg(span.size) > 5` already drops the NULL
// through its own comparison: a trailing aggregate with no comparison
// (`{} | avg(span.size)`) has no such filter, and the /api/search wire
// shape has no representation for a NULL Value — the spec round-trip
// surfaces it as a literal `null`, and the production scan binds a plain
// float64 destination (chclient.Sample.Value), which a NULL leaves at its
// zero value. Either way the trace is reported with a fabricated aggregate
// instead of being omitted.
//
// A Filter ABOVE the Aggregate rather than the Aggregate's own `Having`
// slot: `Value` is the aggregate's OUTPUT alias, and the scalar-filter path
// already stacks exactly this shape (`Filter predicate=(Value > 0)` over the
// same Aggregate), so the optimizer's projection pushdown reads the
// reference as an output column. Put in `Having`, the same ColumnRef is
// walked as an INPUT of the Aggregate and pushed down into the scan's
// projection list, which then names a column the table does not have
// (ClickHouse: `Column 'otel_traces.Value' is not under aggregate function`).
func dropNullValueRows(agg chplan.Node, alias string) chplan.Node {
	return &chplan.Filter{
		Input: agg,
		Predicate: &chplan.FuncCall{
			Fn:   chplan.FnIsNotNull,
			Args: []chplan.Expr{&chplan.ColumnRef{Name: alias}},
		},
	}
}

// mapAggregateOp turns a TraceQL AggregateOp into the CH agg function
// identifier. count / max / min / sum / avg map 1:1.
func mapAggregateOp(op traceql.AggregateOp) (chplan.Fn, error) {
	switch op {
	case traceql.AggregateCount:
		return chplan.FnCount, nil
	case traceql.AggregateMax:
		return chplan.FnMax, nil
	case traceql.AggregateMin:
		return chplan.FnMin, nil
	case traceql.AggregateSum:
		return chplan.FnSum, nil
	case traceql.AggregateAvg:
		return chplan.FnAvg, nil
	}
	return "", fmt.Errorf("traceql: aggregate op %q is unsupported", op)
}
