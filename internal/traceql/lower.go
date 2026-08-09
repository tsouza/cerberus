// Package traceql lowers Tempo TraceQL queries into the shared cerberus
// chplan IR. Covers the SpansetFilter form (attribute matchers like
// `{ .service.name = "x" }`, `{ duration > 100ms }`,
// `{ span.http.status_code >= 500 }`), structural operators
// (`>>`/`>`), aggregators, time filters, and `| select(...)`.
package traceql

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// tracer emits the `lower` pipeline-stage span for TraceQL lowering.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/traceql")

// Lower turns a parsed TraceQL expression into a chplan tree.
//
// When `expr.MetricsPipeline` is non-nil the query is a metrics
// aggregation (`{ ... } | rate()`, `{ ... } | sum_over_time(attr)`,
// etc.). The spanset prefix in `expr.Pipeline.Elements` (typically a
// single `{ ... }` selector) lowers to a Scan/Filter tree, then
// lowerMetricsPipeline wraps it with a chplan.Aggregate carrying the
// CH aggregate function + group-by labels. The query time range itself
// is supplied by the HTTP /api/metrics/query_range handler (which
// wraps the returned tree with a chplan.RangeWindow) — TraceQL's
// grammar doesn't carry the range argument in the AST. See
// docs/upstream-forks.md.
func Lower(ctx context.Context, expr *traceql.RootExpr, s schema.Traces) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("traceql")))
	defer span.End()
	plan, err := lowerRoot(expr, s)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	// The request window resolves from ctx for BOTH the span-search path
	// (handler.go WithSearchWindow) and the metrics handlers (which thread it too
	// — see internal/api/tempo/metrics_query_range.go et al.). searchWindowFromCtx
	// returns zero times for callers that pass no window (spec/property harnesses,
	// /traces/{id}).
	start, end := searchWindowFromCtx(ctx)
	// LEAF stamping is SEARCH-ONLY. The /api/search trace-limit + window folds
	// apply to the chplan-leaf scans of a SPAN search plan. A metrics query
	// (MetricsPipeline / MetricsSecondStage) gets its leaf time bound from the
	// /api/metrics/query_range handler's RangeWindow wrap, never from these
	// stamps — and its leaves must NOT take the request window even if the query
	// is (mis)routed through /api/search with a limit. Gating here keeps the
	// "only span plans reach pushLeafPredicate" invariant enforced rather than
	// conventional, so the generic leaf recurse can never fold a window onto a
	// metrics-pipeline leaf.
	if expr.MetricsPipeline == nil && expr.MetricsSecondStage == nil {
		// Bound the nested-set numbering walk to the N traces /api/search will
		// return, ranked within the request window (no-op unless the request set
		// a limit AND the plan is a select() over the Drilldown structure shape
		// — see search_limit.go).
		plan = stampNestedSetTraceLimit(plan, searchTraceLimit(ctx), start, end, s)
		// Push the response trace limit + request window into the plain-search
		// row source (a bare Scan or Filter(Scan)) so /api/search drains only the
		// N newest traces in the window instead of buffering every matching span
		// (the summaries-drain OOM). No-op unless the request set a limit AND the
		// plan is a plain search — structural / set-op plans are left unchanged.
		plan = stampSearchTraceLimit(plan, searchTraceLimit(ctx), start, end, s)
		// Fold the request window into the leaf scans of the COMPOUND search
		// shapes (&&/||, structural, select(nestedSet*), AGGREGATE) that
		// stampSearchTraceLimit leaves untouched, so they scan only [start, end]
		// instead of full retention. Runs last so it skips the already-windowed
		// plain-search SearchTraceLimit node (no double-fold).
		plan = stampSearchWindow(plan, searchTraceLimit(ctx), start, end, s)
	}
	// RECURSIVE-ARM stamping is UNIVERSAL — search AND metrics. The
	// EMITTER-SYNTHETIC recursive spans scans (the nested-set numbering CTE on
	// NestedSetAnnotate, the structural-closure step arm on StructuralJoin) scan
	// the physical spans table directly inside a WITH RECURSIVE, BELOW where the
	// metrics RangeWindow wrap can reach. So a metrics pipeline over a structural
	// / nested-set source (`{ } >> { } | rate()`,
	// `{ nestedSetParent<0 } | by(nestedSetParent) | rate()`) would otherwise emit
	// a windowless recursive arm that reads full retention behind the inert
	// `TraceId IN (<seed>)`. The stamp needs only a non-zero [start,end] + tsCol,
	// independent of the response trace limit, so it runs for every plan; a zero
	// window no-ops, keeping non-windowed callers byte-identical.
	plan = stampRecursiveScanWindow(plan, start, end, s)
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

// lowerRoot is the body of Lower minus the span bookkeeping; split so
// the public entry point keeps tracing concerns separate.
func lowerRoot(expr *traceql.RootExpr, s schema.Traces) (chplan.Node, error) {
	if expr == nil {
		return nil, fmt.Errorf("traceql: nil RootExpr")
	}
	if len(expr.Pipeline.Elements) == 0 {
		return nil, fmt.Errorf("traceql: empty pipeline")
	}

	// Fold a standalone trailing `| by(X)` grouping stage into a following
	// metrics aggregate so `{...} | by(X) | rate()` lowers identically to the
	// valid `{...} | rate() by (X)`. Without this the `by(X)` stage lowers to a
	// standalone GROUP-BY Aggregate that strips Timestamp, and the metrics rate
	// grid the /api/metrics/query_range handler wraps over it then references a
	// Timestamp the inner aggregate already collapsed away — ClickHouse code 47
	// ("Unknown expression or function identifier `Timestamp`"), a 502. Routing
	// the grouping through the aggregate's by-clause keeps Timestamp in scope.
	pipeline, mp := expr.Pipeline, expr.MetricsPipeline
	if mp != nil {
		pipeline, mp = foldTrailingGroupByIntoMetrics(pipeline, mp)
	}

	plan, err := lowerPipeline(pipeline, s)
	if err != nil {
		return nil, err
	}

	if mp != nil {
		plan, err = lowerMetricsPipeline(plan, mp, s)
		if err != nil {
			return nil, err
		}
	}
	if expr.MetricsSecondStage != nil {
		plan, err = lowerMetricsSecondStage(plan, expr.MetricsSecondStage)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// lowerPipeline folds a TraceQL Pipeline into a chplan tree: the first
// element lowers to a Scan/Filter (or nested spanset operation) and each
// subsequent element (aggregators, scalar filter, group / coalesce /
// select) layers onto the previous result. Shared by lowerRoot and by
// lowerSpansetExpr, which lowers a parenthesised sub-pipeline operand of
// a spanset set operation (`({…} | count() > 1) && ({…} | count() > 1)`).
func lowerPipeline(p traceql.Pipeline, s schema.Traces) (chplan.Node, error) {
	if len(p.Elements) == 0 {
		return nil, fmt.Errorf("traceql: empty pipeline")
	}
	plan, err := lowerPipelineElement(p.Elements[0], s)
	if err != nil {
		return nil, err
	}
	for _, el := range p.Elements[1:] {
		next, err := lowerFollowingElement(plan, el, s)
		if err != nil {
			return nil, err
		}
		plan = next
	}
	return plan, nil
}

// lowerMetricsSecondStage wraps the metrics-aggregate subtree with a
// chplan.MetricsSecondStage carrying the `| topk(N)` / `| bottomk(N)`
// / `| > N` / `| < N` / `| >= N` / `| <= N` / `| == N` / `| != N`
// transform. Chained second-stage (`| topk(5) | > 10`) is supported
// via traceql.ChainedSecondStage: each element wraps the previous
// result in document order, so the rightmost element ends up as the
// outermost chplan node (which matches the chsql emitter's
// inside-out subquery wrap).
//
// The IR + chsql emit foundation landed in PR #437. The in-house ast
// (internal/traceql/ast) exposes Op() / Limit() / Value() / Elements() /
// Separators() accessors on its SecondStageElement variants, which this
// lowering reads to build the load-bearing shapes.
func lowerMetricsSecondStage(inner chplan.Node, ss traceql.SecondStageElement) (chplan.Node, error) {
	switch v := ss.(type) {
	case *traceql.TopKBottomK:
		return lowerTopKBottomK(inner, v)
	case *traceql.MetricsFilter:
		return lowerMetricsFilter(inner, v)
	case traceql.ChainedSecondStage:
		return lowerChainedSecondStage(inner, v)
	case *traceql.ChainedSecondStage:
		return lowerChainedSecondStage(inner, *v)
	}
	return nil, fmt.Errorf("traceql: metrics second-stage element %T is unsupported", ss)
}

// lowerTopKBottomK turns `| topk(N)` / `| bottomk(N)` into a
// chplan.MetricsSecondStage wrap with discriminator SecondStageTopK
// or SecondStageBottomK. K is the upstream limit; the emitter
// renders `ORDER BY Value <DESC|ASC> LIMIT K` and treats PartitionBy
// (empty here — TraceQL instant-metrics path; matrix path supplied
// by the /api/metrics/query_range handler via a wrapping
// RangeWindow) as the per-anchor key.
func lowerTopKBottomK(inner chplan.Node, t *traceql.TopKBottomK) (chplan.Node, error) {
	op, err := mapSecondStageOp(t.Op())
	if err != nil {
		return nil, err
	}
	limit := t.Limit()
	if limit <= 0 {
		return nil, fmt.Errorf("traceql: %s(%d): limit must be > 0", t.Op(), limit)
	}
	return &chplan.MetricsSecondStage{
		Input:      inner,
		Op:         op,
		K:          int64(limit),
		ValueAlias: metricsValueAlias,
	}, nil
}

// lowerMetricsFilter turns `| > N` / `| < N` / `| >= N` / `| <= N`
// / `| == N` / `| != N` into a chplan.MetricsSecondStage with
// discriminator SecondStageThreshold. The chsql emitter renders the
// wrap as `WHERE Value <Op> <Value>` on the inner aggregate's row
// shape.
func lowerMetricsFilter(inner chplan.Node, f *traceql.MetricsFilter) (chplan.Node, error) {
	op, err := mapBinaryOp(f.Op())
	if err != nil {
		return nil, fmt.Errorf("traceql: metrics filter operator %s: %w", f.Op(), err)
	}
	if !isThresholdBinaryOp(op) {
		return nil, fmt.Errorf("traceql: metrics filter operator %s is not a supported threshold comparison", f.Op())
	}
	return &chplan.MetricsSecondStage{
		Input:          inner,
		Op:             chplan.SecondStageThreshold,
		ThresholdOp:    op,
		ThresholdValue: f.Value(),
		ValueAlias:     metricsValueAlias,
	}, nil
}

// lowerChainedSecondStage walks ChainedSecondStage.Elements() in
// source order, wrapping the previous result in each successive
// second-stage node. The first element wraps the upstream metrics
// aggregate (`inner`); each subsequent element wraps the previous
// chplan.MetricsSecondStage. The rightmost element in the TraceQL
// source ends up as the outermost chplan node, matching the
// inside-out subquery wrap the chsql emitter renders (see
// test/spec/chsql/metrics_second_stage_chained_topk_threshold.txtar).
//
// Separators() carries the pipeline punctuation upstream uses for
// String() round-trip fidelity. The lowering does not care about
// the punctuation per se (it's a chained pipe stream — the wrapping
// order is what matters), but the accessor existence keeps the
// upstream contract explicit for future regression cases.
func lowerChainedSecondStage(inner chplan.Node, c traceql.ChainedSecondStage) (chplan.Node, error) {
	elements := c.Elements()
	if len(elements) == 0 {
		return nil, fmt.Errorf("traceql: chained second-stage has no elements")
	}
	// Validate Separators() length matches Elements() so a future
	// upstream change (e.g. dropping a separator slot) trips this
	// check rather than silently dropping a wrap.
	if seps := c.Separators(); len(seps) != len(elements) {
		return nil, fmt.Errorf("traceql: chained second-stage element/separator length mismatch (%d vs %d)", len(elements), len(seps))
	}
	current := inner
	for _, el := range elements {
		next, err := lowerMetricsSecondStage(current, el)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// mapSecondStageOp translates Tempo's SecondStageOp (OpTopK /
// OpBottomK) to the chplan discriminator. Reserved-for-future
// SecondStageOp values surface as a clean unsupported error rather
// than collapse to SecondStageInvalid (which the emitter would
// reject anyway).
func mapSecondStageOp(op traceql.SecondStageOp) (chplan.SecondStageOp, error) {
	switch op {
	case traceql.OpTopK:
		return chplan.SecondStageTopK, nil
	case traceql.OpBottomK:
		return chplan.SecondStageBottomK, nil
	}
	return chplan.SecondStageInvalid, fmt.Errorf("traceql: second-stage op %s is not supported", op)
}

// isThresholdBinaryOp reports whether op is one of the six
// comparison operators Tempo's `MetricsFilter.validate()` accepts
// (>, >=, <, <=, =, !=). Mirrors chsql.isThresholdOp; duplicated
// here because the chsql helper is unexported and importing
// chsql from a lowering package would create the wrong dep
// direction (chsql consumes chplan; chsql consuming lowering would
// invert the layering).
func isThresholdBinaryOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpGt, chplan.OpGe, chplan.OpLt, chplan.OpLe, chplan.OpEq, chplan.OpNe:
		return true
	}
	return false
}

// lowerFollowingElement layers a pipeline element onto the previous
// stage's plan. Aggregate / ScalarFilter / SelectOperation /
// GroupOperation / CoalesceOperation are supported.
func lowerFollowingElement(prev chplan.Node, elem traceql.PipelineElement, s schema.Traces) (chplan.Node, error) {
	switch v := elem.(type) {
	case traceql.Aggregate:
		return lowerAggregate(prev, v, s)
	case *traceql.Aggregate:
		return lowerAggregate(prev, *v, s)
	case traceql.ScalarFilter:
		return lowerScalarFilter(prev, v, s)
	case *traceql.ScalarFilter:
		return lowerScalarFilter(prev, *v, s)
	case traceql.SelectOperation:
		return lowerSelect(prev, v, s)
	case *traceql.SelectOperation:
		return lowerSelect(prev, *v, s)
	case traceql.GroupOperation:
		return lowerGroup(prev, v, s)
	case *traceql.GroupOperation:
		return lowerGroup(prev, *v, s)
	case traceql.CoalesceOperation:
		return lowerCoalesce(prev, s)
	case *traceql.CoalesceOperation:
		return lowerCoalesce(prev, s)
	}
	return nil, fmt.Errorf("traceql: pipeline tail element %T is unsupported", elem)
}

// lowerScalarFilter handles `| count() > 0`, `| sum(.duration) >= 1s`,
// etc. The overwhelmingly common shape — a single aggregate compared
// against a literal — takes the fast path (lowerSimpleScalarFilter),
// which is byte-identical to the plan this function produced before
// #1708: a Filter wrapping the lone Aggregate node. Anything else
// (arithmetic between aggregates via ast.ScalarOperation on either
// side, arbitrary nesting of that arithmetic, or an aggregate directly
// on the RHS — `| max(duration) - min(duration) >= 0`,
// `| (max(duration) - min(duration)) / avg(duration) > 0.5`,
// `| max(duration) > avg(duration)`) takes the general path
// (lowerArithmeticScalarFilter).
func lowerScalarFilter(prev chplan.Node, sf traceql.ScalarFilter, s schema.Traces) (chplan.Node, error) {
	op, err := mapBinaryOp(sf.Op)
	if err != nil {
		return nil, err
	}
	if isBareAggregate(sf.LHS) && isBareStatic(sf.RHS) {
		return lowerSimpleScalarFilter(prev, sf, op, s)
	}
	return lowerArithmeticScalarFilter(prev, sf, op, s)
}

// lowerSimpleScalarFilter is the pre-#1708 lowering for the single-
// aggregate-vs-literal shape: LHS lowers to the lone chplan.Aggregate
// node and RHS lowers to a chplan.Expr literal; the Filter compares
// the Aggregate's aggValueAlias ("Value") column against that literal
// directly, no intermediate Project. Kept as its own function (rather
// than folded into the general multi-aggregate path) so this shape's
// plan — pinned by test/spec TXTAR goldens and
// TestLowerSpansetAggregate_PerTraceShape's `Filter.Input =
// *chplan.Aggregate` type assertion — never moves.
func lowerSimpleScalarFilter(prev chplan.Node, sf traceql.ScalarFilter, op chplan.BinaryOp, s schema.Traces) (chplan.Node, error) {
	aggNode, err := lowerScalarExpr(prev, sf.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerScalarExpr(prev, sf.RHS, s)
	if err != nil {
		return nil, err
	}

	// rhs is expected to be a chplan.Expr from a Static literal; the
	// LHS recursed back as a chplan.Node (Aggregate). For the typical
	// `count() > 0` shape, wrap aggNode with a Filter. isBareAggregate /
	// isBareStatic guard the dispatch above, but a defensive type-assert
	// stays here so a future AST shape this dispatch didn't anticipate
	// surfaces a structured error instead of a panic (see #324 / the
	// pathological `{} | 0 > 0` regression pin).
	aggPlan, ok := aggNode.(chplan.Node)
	if !ok {
		return nil, fmt.Errorf("traceql: scalar-filter LHS must aggregate to a series (count() / sum(...) / avg(...) / min(...) / max(...)), got %T", aggNode)
	}
	rhsExpr, ok := rhs.(chplan.Expr)
	if !ok {
		return nil, fmt.Errorf("traceql: scalar-filter RHS must be a literal, got %T", rhs)
	}

	return &chplan.Filter{
		Input:     aggPlan,
		Predicate: &chplan.Binary{Op: op, Left: &chplan.ColumnRef{Name: aggValueAlias}, Right: rhsExpr},
	}, nil
}

// scalarFilterRHSAlias names the Project column
// lowerArithmeticScalarFilter uses to carry the RHS operand's value
// (which, like the LHS, may itself reference one or more aggregate
// leaves) out of the shared Aggregate node. Reserved-name style
// matches the `__cerberus_*` convention internal/api/tempo/handler.go
// uses for its own synthesised columns.
const scalarFilterRHSAlias = "__cerberus_scalar_rhs"

// lowerArithmeticScalarFilter handles scalar-filter shapes beyond
// lowerSimpleScalarFilter's single "aggregate compared to a literal"
// fast path: arithmetic between aggregates
// (`max(duration) - min(duration) >= 0`), arbitrary nesting of that
// arithmetic via ast.ScalarOperation
// (`(max(duration) - min(duration)) / avg(duration) > 0.5`), and an
// aggregate directly on the RHS (`... > avg(duration)`).
//
// Two independently-grouped chplan.Aggregate nodes (the shape a naive
// per-operand lowering would produce) have no shared row for a
// chplan.Binary to read both operands from — GROUP BY TraceId on two
// separate Aggregate nodes can put different trace's rows at different
// output positions with nothing to join them on inline. So every
// ast.Aggregate leaf found while walking LHS and RHS (lowerScalarOperand
// recurses through ast.ScalarOperation, collecting leaves via
// scalarAggLeaf) is folded into the AggFuncs list of ONE shared
// chplan.Aggregate node — same TraceId grouping and envelope columns
// lowerAggregate uses for the single-aggregate shape — so every leaf's
// value lands as a column of the same per-trace row.
//
// A Project on top of that shared Aggregate evaluates the LHS/RHS
// expression trees (chplan.Binary composed from the per-leaf AggFunc
// aliases) and republishes LHS under aggValueAlias ("Value") and RHS
// under scalarFilterRHSAlias, passing the envelope columns through
// unchanged, so:
//   - the outer Filter can compare two plain ColumnRefs instead of
//     embedding arbitrarily-nested Binary trees in its Predicate, and
//   - downstream shape-detection (isSpansetAggregateShape /
//     aggregateCarriesSpansetEnvelope in internal/api/tempo/handler.go)
//     keeps recognising the spanset-aggregate search-envelope shape
//     through the extra Project layer — see the Project case added
//     there.
func lowerArithmeticScalarFilter(prev chplan.Node, sf traceql.ScalarFilter, op chplan.BinaryOp, s schema.Traces) (chplan.Node, error) {
	var aggFuncs []chplan.AggFunc
	needsNestedSet := false
	nextID := 0

	lhsExpr, err := lowerScalarOperand(sf.LHS, s, &aggFuncs, &needsNestedSet, &nextID)
	if err != nil {
		return nil, err
	}
	rhsExpr, err := lowerScalarOperand(sf.RHS, s, &aggFuncs, &needsNestedSet, &nextID)
	if err != nil {
		return nil, err
	}
	if len(aggFuncs) == 0 {
		// The Tempo parser happily accepts pathological forms like
		// `{} | 0 > 0` (no aggregate on either side) — reject explicitly
		// rather than emit an Aggregate with no value columns.
		return nil, fmt.Errorf("traceql: scalar-filter must aggregate to a series on at least one side (count() / sum(...) / avg(...) / min(...) / max(...)), got LHS %T and RHS %T", sf.LHS, sf.RHS)
	}

	input := prev
	if needsNestedSet {
		input = annotateNestedSet(prev, s)
	}

	aggNode := &chplan.Aggregate{
		Input:          input,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{aggTraceIDAlias},
		AggFuncs: append(
			aggFuncs,
			anyAggFunc(s.SpanNameColumn, aggMetricNameAlias),
			anyAggFunc(s.ResourceAttributesColumn, aggResourceAttrsAlias),
			anyAggFunc(s.ParentSpanIDColumn, aggParentSpanIDAlias),
			minAggFunc(s.TimestampColumn, aggTimeUnixAlias),
			traceStartNsAggFunc(s.TimestampColumn),
			traceEndNsAggFunc(s.TimestampColumn, s.DurationColumn),
		),
	}

	projected := &chplan.Project{
		Input: aggNode,
		Projections: []chplan.Projection{
			{Expr: lhsExpr, Alias: aggValueAlias},
			{Expr: rhsExpr, Alias: scalarFilterRHSAlias},
			{Expr: &chplan.ColumnRef{Name: aggMetricNameAlias}, Alias: aggMetricNameAlias},
			{Expr: &chplan.ColumnRef{Name: aggResourceAttrsAlias}, Alias: aggResourceAttrsAlias},
			{Expr: &chplan.ColumnRef{Name: aggParentSpanIDAlias}, Alias: aggParentSpanIDAlias},
			{Expr: &chplan.ColumnRef{Name: aggTimeUnixAlias}, Alias: aggTimeUnixAlias},
			{Expr: &chplan.ColumnRef{Name: aggTraceStartNsAlias}, Alias: aggTraceStartNsAlias},
			{Expr: &chplan.ColumnRef{Name: aggTraceEndNsAlias}, Alias: aggTraceEndNsAlias},
			{Expr: &chplan.ColumnRef{Name: aggTraceIDAlias}, Alias: aggTraceIDAlias},
		},
	}

	return &chplan.Filter{
		Input: projected,
		Predicate: &chplan.Binary{
			Op:    op,
			Left:  &chplan.ColumnRef{Name: aggValueAlias},
			Right: &chplan.ColumnRef{Name: scalarFilterRHSAlias},
		},
	}, nil
}

// lowerScalarOperand lowers one operand of a scalar-filter comparison
// (or of a nested ast.ScalarOperation) into a chplan.Expr. Aggregate
// leaves append an AggFunc to aggFuncs (via scalarAggLeaf, under a
// fresh numbered alias so multiple leaves never collide) and return a
// ColumnRef to that alias; Static leaves lower straight to a literal;
// ScalarOperation recurses on both sides and composes a chplan.Binary
// — the recursion is what makes arbitrary nesting
// (`(max(duration) - min(duration)) / avg(duration)`) fall out for
// free, since a nested ScalarOperation's own composed Binary is just
// another Expr operand to the outer one.
func lowerScalarOperand(e traceql.ScalarExpression, s schema.Traces, aggFuncs *[]chplan.AggFunc, needsNestedSet *bool, nextID *int) (chplan.Expr, error) {
	switch v := e.(type) {
	case traceql.Aggregate:
		return lowerScalarAggregateOperand(v, s, aggFuncs, needsNestedSet, nextID)
	case *traceql.Aggregate:
		return lowerScalarAggregateOperand(*v, s, aggFuncs, needsNestedSet, nextID)
	case traceql.Static:
		return lowerStatic(v)
	case *traceql.Static:
		return lowerStatic(*v)
	case traceql.ScalarOperation:
		return lowerScalarOperationOperand(v, s, aggFuncs, needsNestedSet, nextID)
	case *traceql.ScalarOperation:
		return lowerScalarOperationOperand(*v, s, aggFuncs, needsNestedSet, nextID)
	}
	return nil, fmt.Errorf("traceql: scalar expression %T is unsupported", e)
}

// lowerScalarAggregateOperand lowers a single ast.Aggregate leaf
// within a scalar-filter expression tree: computes its AggFunc under a
// fresh numbered alias (scalarLeafAliasPrefix + nextID), appends it to
// aggFuncs, and returns a ColumnRef to that alias.
func lowerScalarAggregateOperand(agg traceql.Aggregate, s schema.Traces, aggFuncs *[]chplan.AggFunc, needsNestedSet *bool, nextID *int) (chplan.Expr, error) {
	alias := fmt.Sprintf("%s%d", scalarLeafAliasPrefix, *nextID)
	*nextID++
	fn, leafNeedsNestedSet, err := scalarAggLeaf(agg, s, alias)
	if err != nil {
		return nil, err
	}
	if leafNeedsNestedSet {
		*needsNestedSet = true
	}
	*aggFuncs = append(*aggFuncs, fn)
	return &chplan.ColumnRef{Name: alias}, nil
}

// scalarLeafAliasPrefix names the per-leaf AggFunc aliases
// lowerScalarAggregateOperand assigns (scalarLeafAliasPrefix + an
// incrementing index, e.g. "__cerberus_scalar_agg0"), keeping them out
// of the aggValueAlias / envelope-alias namespace so multiple aggregate
// leaves in one ScalarOperation tree never collide with each other or
// with the envelope columns.
const scalarLeafAliasPrefix = "__cerberus_scalar_agg"

// lowerScalarOperationOperand lowers an ast.ScalarOperation node
// (arithmetic or comparison between two nested scalar expressions) by
// recursing on both operands and composing a chplan.Binary over
// whatever each side resolved to (a literal, a ColumnRef to a freshly
// collected aggregate leaf, or another Binary from a nested
// ScalarOperation).
func lowerScalarOperationOperand(op traceql.ScalarOperation, s schema.Traces, aggFuncs *[]chplan.AggFunc, needsNestedSet *bool, nextID *int) (chplan.Expr, error) {
	lhs, err := lowerScalarOperand(op.LHS, s, aggFuncs, needsNestedSet, nextID)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerScalarOperand(op.RHS, s, aggFuncs, needsNestedSet, nextID)
	if err != nil {
		return nil, err
	}
	binOp, err := mapBinaryOp(op.Op)
	if err != nil {
		return nil, fmt.Errorf("traceql: scalar operation operator %s: %w", op.Op, err)
	}
	return &chplan.Binary{Op: binOp, Left: lhs, Right: rhs}, nil
}

// isBareAggregate reports whether a ScalarExpression is a plain
// ast.Aggregate (value or pointer form) — the shape
// lowerSimpleScalarFilter's fast path requires on the LHS.
func isBareAggregate(e traceql.ScalarExpression) bool {
	switch e.(type) {
	case traceql.Aggregate, *traceql.Aggregate:
		return true
	}
	return false
}

// isBareStatic reports whether a ScalarExpression is a plain
// ast.Static literal (value or pointer form) — the shape
// lowerSimpleScalarFilter's fast path requires on the RHS.
func isBareStatic(e traceql.ScalarExpression) bool {
	switch e.(type) {
	case traceql.Static, *traceql.Static:
		return true
	}
	return false
}

// lowerScalarExpr converts a TraceQL ScalarExpression into either a
// chplan.Node (when the expression aggregates / produces a series) or
// a chplan.Expr (when it's a literal). Returns `any`; callers
// type-assert based on context. Used only by lowerSimpleScalarFilter's
// fast path (isBareAggregate / isBareStatic already exclude
// ast.ScalarOperation, so it need not handle that case) and by
// lowerFollowingElement's defensive `*traceql.Aggregate` pipeline-tail
// dispatch.
func lowerScalarExpr(prev chplan.Node, e traceql.ScalarExpression, s schema.Traces) (any, error) {
	switch v := e.(type) {
	case traceql.Aggregate:
		return lowerAggregate(prev, v, s)
	case *traceql.Aggregate:
		return lowerAggregate(prev, *v, s)
	case traceql.Static:
		return lowerStatic(v)
	case *traceql.Static:
		return lowerStatic(*v)
	}
	return nil, fmt.Errorf("traceql: scalar expression %T is unsupported", e)
}

// lowerPipelineElement dispatches the first TraceQL pipeline element to
// its corresponding lowering routine: SpansetFilter or SpansetOperation.
// Aggregates, select, and scalar filters appear only as following
// elements and are dispatched by lowerFollowingElement.
func lowerPipelineElement(elem traceql.PipelineElement, s schema.Traces) (chplan.Node, error) {
	switch v := elem.(type) {
	case *traceql.SpansetFilter:
		return lowerSpansetFilter(v, s)
	case traceql.SpansetOperation:
		return lowerSpansetOperation(&v, s)
	case *traceql.SpansetOperation:
		return lowerSpansetOperation(v, s)
	}
	return nil, fmt.Errorf("traceql: pipeline element %T is unsupported", elem)
}

// lowerSpansetOperation handles structural relations (`A > B`, `A < B`,
// `A ~ B`, the recursive forms `A >> B` / `A << B`, plus their negated
// (`A !> B`, `A !< B`, `A !~ B`, `A !>> B`, `A !<< B`) and union-prefixed
// (`A &> B`, `A &< B`, `A &~ B`, `A &>> B`, `A &<< B`) variants) and set
// operations (`A && B`, `A || B`). Multi-hop chains (`A > B > C`) fall
// out of the binary StructuralJoin shape — the Tempo grammar binds `>`
// left-associatively, so chained operators parse as nested
// SpansetOperation nodes that this function recurses into via
// lowerSpansetExpr.
func lowerSpansetOperation(op *traceql.SpansetOperation, s schema.Traces) (chplan.Node, error) {
	left, err := lowerSpansetExpr(op.LHS, s)
	if err != nil {
		return nil, err
	}
	right, err := lowerSpansetExpr(op.RHS, s)
	if err != nil {
		return nil, err
	}

	// Set operations (`&&` / `||`) lower to a chplan.SetOperation; the
	// emitter renders an identity-deduped UNION ALL of the two arms,
	// keyed on (TraceID, SpanID) — for `&&` gated on the trace appearing
	// in both arms, for `||` ungated (see chsql.emitSetOperation for why
	// `&&` is a span union and not a span intersection).
	if setOp, ok := mapSetOp(op.Op); ok {
		// A CH UNION matches arm columns positionally and errors (CH
		// code 258) when the counts differ. Structural arms expose the
		// narrow span envelope (3 keys + the
		// structuralExtraProjectionColumns list) while plain filter
		// arms expose `SELECT *`; mixing them — the exact shape of
		// Grafana Traces Drilldown's structure-tab query
		// `({...} &>> {...}) || ({...})` — needs the wide arm projected
		// down to the same ordered column list. Both ops emit a UNION,
		// so both need the alignment.
		left, right = alignUnionArms(left, right, s)
		return &chplan.SetOperation{
			Left:          left,
			Right:         right,
			Op:            setOp,
			TraceIDColumn: s.TraceIDColumn,
			SpanIDColumn:  s.SpanIDColumn,
		}, nil
	}

	relation, err := mapStructuralOp(op.Op)
	if err != nil {
		return nil, err
	}
	return &chplan.StructuralJoin{
		Left:                   left,
		Right:                  right,
		Op:                     relation,
		TraceIDColumn:          s.TraceIDColumn,
		SpanIDColumn:           s.SpanIDColumn,
		ParentSpanIDColumn:     s.ParentSpanIDColumn,
		ExtraProjectionColumns: structuralExtraProjectionColumns(s),
		CandidatePrefilter:     candidatePrefilterEligible(relation, left, right),
	}, nil
}

// candidatePrefilterEligible reports whether a recursive descendant/ancestor
// join should carry the candidate prefilter — restricting its anchor seed to
// traces present on BOTH sides (the L-intersect-R set) so a sparse selective
// query walks the closure over only the traces that can possibly match.
//
// It fires only for the plain recursive relations (`>>` / `<<`) — the shapes
// whose WITH RECURSIVE closure over the whole trace set is the expensive part
// — and only when BOTH sides are cheap selective Filter(Scan) leaves. Two
// exclusions are load-bearing:
//
//   - a bare `{}` side lowers to a plain Scan (no Filter): dense, the closure
//     already has to walk it, so intersecting buys nothing and the extra
//     candidate subquery would only add work. Skip => plain closure, no
//     regression on the dense case.
//   - a derived / structural side (a *chplan.StructuralJoin) arises at the
//     OUTER level of a left-associative chain `A>>B>>C`, whose left side is
//     itself a StructuralJoin. Prefiltering there would re-execute the inner
//     closure inside the candidate subquery — doubling the work. Only the
//     innermost level, where both sides are Filter(Scan), gets the prefilter.
func candidatePrefilterEligible(relation chplan.StructuralOp, left, right chplan.Node) bool {
	switch relation {
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
	default:
		return false
	}
	return isCheapSelectiveLeaf(left) && isCheapSelectiveLeaf(right)
}

// isCheapSelectiveLeaf reports whether n is a cheap, selective spans leaf: a
// Filter directly over a Scan whose predicate is a real matcher (not nil, not
// a bare constant boolean). A bare Scan (`{}`), a nested-set-annotated scan, a
// set operation, or a structural join all return false — they are either dense
// or derived, and the candidate prefilter must not fire on them (see
// candidatePrefilterEligible for why).
func isCheapSelectiveLeaf(n chplan.Node) bool {
	f, ok := n.(*chplan.Filter)
	if !ok {
		return false
	}
	if _, ok := f.Input.(*chplan.Scan); !ok {
		return false
	}
	switch f.Predicate.(type) {
	case nil, *chplan.LitBool:
		// A missing or constant-boolean predicate is not selective.
		return false
	}
	return true
}

// structuralExtraProjectionColumns returns the non-key column list the
// structural-join wrap subquery must expose as bare-name aliases so the
// Tempo API-layer wrap projection (rQualifiedSampleProjections in
// internal/api/tempo/handler.go) can reference them without the
// `Unknown identifier 'SpanName' in scope` CH 25.8 analyzer rejection
// exposed by tempo compat run 26098988786.
//
// The list mirrors the schema columns the canonical/sample wrap
// projections AND a downstream `| select(...)` projection can read:
// the original envelope four (SpanName, Duration, Timestamp,
// ResourceAttributes) plus the columns select() lowers intrinsics and
// span attributes to (SpanAttributes, StatusCode, StatusMessage,
// SpanKind, ScopeName, ScopeVersion) — `{A} >> {B} | select(status)`
// otherwise dies at execution with `Unknown identifier 'StatusCode'`.
// Adding a column the wrap projection reads goes through this helper
// so the Tempo handler stays the source of truth for "what the search
// envelope needs". alignUnionArms reuses the same list so mixed
// structural/plain `||` arms line up positionally.
// structuralExtraProjectionColumnCount is the number of candidate
// columns the loop below iterates — the exact pre-size for cols.
const structuralExtraProjectionColumnCount = 10

func structuralExtraProjectionColumns(s schema.Traces) []string {
	cols := make([]string, 0, structuralExtraProjectionColumnCount)
	for _, col := range []string{
		s.SpanNameColumn,
		s.DurationColumn,
		s.TimestampColumn,
		s.ResourceAttributesColumn,
		s.AttributesColumn,
		s.StatusCodeColumn,
		s.StatusMessageColumn,
		s.SpanKindColumn,
		s.ScopeNameColumn,
		s.ScopeVersionColumn,
	} {
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
}

// alignUnionArms gives both `||` arms the same positional column
// shape. A StructuralJoin arm exposes the narrow span envelope —
// (TraceID, SpanID, ParentSpanID) + structuralExtraProjectionColumns,
// in that order (see chsql.structuralProjectionFrags) — while plain
// Filter/Scan arms expose every table column via `SELECT *`. When the
// two shapes mix, the wide arm is wrapped in a Project emitting
// exactly the narrow list so ClickHouse's positional UNION matches
// column-for-column. Same-shape pairs pass through untouched (two wide
// arms are deduped on span identity like any other pair).
func alignUnionArms(left, right chplan.Node, s schema.Traces) (chplan.Node, chplan.Node) {
	ln, rn := isNarrowSpanArm(left, s), isNarrowSpanArm(right, s)
	switch {
	case ln && !rn:
		if project, ok := right.(*chplan.Project); ok {
			return projectSpanArm(left, project.Projections), right
		}
		return left, narrowSpanProjection(right, s)
	case !ln && rn:
		if project, ok := left.(*chplan.Project); ok {
			return left, projectSpanArm(right, project.Projections)
		}
		return narrowSpanProjection(left, s), right
	default:
		return left, right
	}
}

// projectSpanArm gives an already-narrow structural arm the same output shape
// as a select() arm. Applying narrowSpanProjection after select() would read
// columns that select intentionally removed.
func projectSpanArm(n chplan.Node, projections []chplan.Projection) chplan.Node {
	return &chplan.Project{Input: n, Projections: projections}
}

// isNarrowSpanArm reports whether n's output is the narrow span
// envelope rather than the full `SELECT *` table shape. SetOperation
// output mirrors its arms (alignUnionArms keeps them consistent, and
// the intersect emitter projects L.*), so recurse left. Project arms
// only arise from narrowSpanProjection itself within spanset expressions.
// Parenthesized pipelines make `| select(...)` valid as a set-op operand, so
// recognize the projection's exact output shape rather than every Project.
func isNarrowSpanArm(n chplan.Node, s schema.Traces) bool {
	switch v := n.(type) {
	case *chplan.StructuralJoin:
		return true
	case *chplan.Project:
		return isNarrowSpanProjection(v, s)
	case *chplan.SetOperation:
		return isNarrowSpanArm(v.Left, s)
	}
	return false
}

// isNarrowSpanProjection identifies only the projection generated by
// narrowSpanProjection. A select() Project has its own, caller-selected
// columns and must not be treated as the structural envelope.
func isNarrowSpanProjection(p *chplan.Project, s schema.Traces) bool {
	if len(p.Replacements) != 0 {
		return false
	}
	want := append(
		[]string{s.TraceIDColumn, s.SpanIDColumn, s.ParentSpanIDColumn},
		structuralExtraProjectionColumns(s)...,
	)
	if len(p.Projections) != len(want) {
		return false
	}
	for i, projection := range p.Projections {
		column, ok := projection.Expr.(*chplan.ColumnRef)
		if !ok || projection.Alias != "" || column.Name != want[i] {
			return false
		}
	}
	return true
}

// narrowSpanProjection wraps n in a Project that emits the narrow
// span envelope in the structural-join order: the three join keys
// followed by structuralExtraProjectionColumns.
func narrowSpanProjection(n chplan.Node, s schema.Traces) chplan.Node {
	cols := append(
		[]string{s.TraceIDColumn, s.SpanIDColumn, s.ParentSpanIDColumn},
		structuralExtraProjectionColumns(s)...,
	)
	projections := make([]chplan.Projection, 0, len(cols))
	for _, col := range cols {
		projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: col}})
	}
	return &chplan.Project{Input: n, Projections: projections}
}

// mapSetOp identifies the TraceQL operators that lower to a
// chplan.SetOperation. Returns ok=false for non-set operators so the
// caller falls back to structural-relation handling.
func mapSetOp(op traceql.Operator) (chplan.SetOp, bool) {
	switch op {
	case traceql.OpSpansetAnd:
		return chplan.SetIntersect, true
	case traceql.OpSpansetUnion:
		return chplan.SetUnion, true
	}
	return "", false
}

// lowerSpansetExpr lowers a TraceQL SpansetExpression (the LHS/RHS of
// a SpansetOperation). Handles SpansetFilter and nested SpansetOperation
// — the nested case is what makes multi-hop chains (`A > B > C`) and
// mixed direct/recursive chains (`A > B >> C`) work without any
// dedicated lowering pass.
func lowerSpansetExpr(e traceql.SpansetExpression, s schema.Traces) (chplan.Node, error) {
	switch v := e.(type) {
	case *traceql.SpansetFilter:
		return lowerSpansetFilter(v, s)
	case *traceql.SpansetOperation:
		return lowerSpansetOperation(v, s)
	case traceql.SpansetOperation:
		return lowerSpansetOperation(&v, s)
	case *traceql.Pipeline:
		return lowerSpansetOperand(*v, s)
	case traceql.Pipeline:
		return lowerSpansetOperand(v, s)
	}
	return nil, fmt.Errorf("traceql: spanset expression %T is unsupported", e)
}

// lowerSpansetOperand lowers a parenthesised sub-pipeline operand of a
// spanset operation (`({…} | count() > 1) || ({…})`), then re-expresses
// a trailing spanset aggregate as the spans it stands for — every
// operator downstream of here combines spans, not per-trace rows. See
// spanGranularOperand for the Tempo semantics that fixes.
func lowerSpansetOperand(p traceql.Pipeline, s schema.Traces) (chplan.Node, error) {
	plan, err := lowerPipeline(p, s)
	if err != nil {
		return nil, err
	}
	return spanGranularOperand(plan, s), nil
}

// mapStructuralOp translates Tempo's structural Operator enum to the
// chplan.StructuralOp this emitter understands. Covers the positive
// relations (`>` / `<` / `>>` / `<<` / `~`), their negated variants
// (`!>` / `!<` / `!>>` / `!<<` / `!~`), and the union-prefixed
// variants (`&>` / `&<` / `&>>` / `&<<` / `&~`). The negated forms
// lower to the same StructuralJoin shape with a `Not…` Op constant;
// the emitter swaps the outer INNER JOIN for a LEFT ANTI JOIN. The
// union forms lower to a `Union…` Op constant; the emitter emits the
// positive relation twice (once projecting each side) glued with a
// UNION ALL deduped on span identity.
func mapStructuralOp(op traceql.Operator) (chplan.StructuralOp, error) {
	switch op {
	case traceql.OpSpansetChild:
		return chplan.StructuralChild, nil
	case traceql.OpSpansetParent:
		return chplan.StructuralParent, nil
	case traceql.OpSpansetDescendant:
		return chplan.StructuralDescendant, nil
	case traceql.OpSpansetAncestor:
		return chplan.StructuralAncestor, nil
	case traceql.OpSpansetSibling:
		return chplan.StructuralSibling, nil
	case traceql.OpSpansetNotChild:
		return chplan.StructuralNotChild, nil
	case traceql.OpSpansetNotParent:
		return chplan.StructuralNotParent, nil
	case traceql.OpSpansetNotDescendant:
		return chplan.StructuralNotDescendant, nil
	case traceql.OpSpansetNotAncestor:
		return chplan.StructuralNotAncestor, nil
	case traceql.OpSpansetNotSibling:
		return chplan.StructuralNotSibling, nil
	case traceql.OpSpansetUnionChild:
		return chplan.StructuralUnionChild, nil
	case traceql.OpSpansetUnionParent:
		return chplan.StructuralUnionParent, nil
	case traceql.OpSpansetUnionDescendant:
		return chplan.StructuralUnionDescendant, nil
	case traceql.OpSpansetUnionAncestor:
		return chplan.StructuralUnionAncestor, nil
	case traceql.OpSpansetUnionSibling:
		return chplan.StructuralUnionSibling, nil
	}
	return "", fmt.Errorf("traceql: spanset op %s is not a structural relation", op)
}

// lowerSpansetFilter turns `{ <field-expr> }` into Scan + Filter on
// otel_traces. The field expression is recursively lowered into a
// chplan.Expr predicate.
func lowerSpansetFilter(f *traceql.SpansetFilter, s schema.Traces) (chplan.Node, error) {
	// notContextTop: f.Expression is the very top of the spanset filter's
	// boolean tree — the one position where reference Tempo's
	// constant-false NOT-evaluation bug (see lowerUnaryNot) applies.
	pred, err := lowerBooleanFieldExpr(f.Expression, s, notContextTop)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = &chplan.Scan{Table: s.SpansTable}
	// A position-dependent nested-set comparison
	// (`nestedSetParent = 5`, `nestedSetLeft > 0`, …) lowers to a
	// reference against a synthetic NestedSet*Column; back it with the
	// recursive-numbering annotation pass so the column resolves to the
	// real per-span position rather than an unknown identifier.
	if predicateUsesNestedSetColumns(pred) {
		input = annotateNestedSet(input, s)
	}
	if pred == nil {
		return input, nil
	}
	return &chplan.Filter{Input: input, Predicate: pred}, nil
}

// predicateUsesNestedSetColumns reports whether expr references any of
// the synthetic nested-set columns the annotation pass materialises —
// the signal lowerSpansetFilter uses to decide whether to wrap the scan
// in a NestedSetAnnotate.
func predicateUsesNestedSetColumns(expr chplan.Expr) bool {
	switch v := expr.(type) {
	case nil:
		return false
	case *chplan.ColumnRef:
		switch v.Name {
		case chplan.NestedSetLeftColumn, chplan.NestedSetRightColumn, chplan.NestedSetParentColumn:
			return true
		}
		return false
	case *chplan.Binary:
		return predicateUsesNestedSetColumns(v.Left) || predicateUsesNestedSetColumns(v.Right)
	case *chplan.FuncCall:
		for _, a := range v.Args {
			if predicateUsesNestedSetColumns(a) {
				return true
			}
		}
		return false
	case *chplan.FieldAccess:
		return predicateUsesNestedSetColumns(v.Source)
	}
	return false
}

// lowerFieldExpr recursively translates a TraceQL FieldExpression into
// a chplan.Expr. Handles BinaryOperation (= / != / </ <= / > / >= /
// =~ / !~ / + / - / etc.), Attribute (dotted paths), Static (typed
// literal).
func lowerFieldExpr(e traceql.FieldExpression, s schema.Traces) (chplan.Expr, error) {
	switch v := e.(type) {
	case *traceql.BinaryOperation:
		return lowerBinaryOperation(v, s)
	case *traceql.UnaryOperation:
		// This generic dispatch only ever sees a NOT node for value
		// (non-boolean) positions, where TraceQL's grammar does not
		// actually allow `!(...)` — lowerBooleanFieldExpr intercepts
		// every legal NOT position (top-level, AND/OR operand) before
		// falling through here. notContextOrOperand is the conservative
		// choice for this unreachable-in-practice path: it folds a
		// stray NOT to constant-false rather than constant-true.
		return lowerUnaryOperation(*v, s, notContextOrOperand)
	case traceql.UnaryOperation:
		return lowerUnaryOperation(v, s, notContextOrOperand)
	case *traceql.Attribute:
		return lowerAttributeExpr(*v, s)
	case traceql.Attribute:
		return lowerAttributeExpr(v, s)
	case *traceql.Static:
		return lowerStatic(*v)
	case traceql.Static:
		return lowerStatic(v)
	}
	return nil, fmt.Errorf("traceql: field expression %T is unsupported", e)
}

// lowerBooleanFieldExpr is lowerFieldExpr for the three positions a
// FieldExpression is required to yield a BOOLEAN chplan.Expr: a spanset
// filter's whole predicate (lowerSpansetFilter), an AND/OR operand
// (lowerBinaryOperation), and a logical-NOT operand (lowerUnaryNot).
//
// Those are also the only positions where a bare `parent` (IntrinsicParent)
// means TraceQL's "this span has a parent" boolean — ast.Attribute's own
// impliedType() marks IntrinsicParent TypeNil (unlike every other backed
// intrinsic, which has a real scalar type) precisely because it has no
// value-position identity of its own; `parent = "<hex span id>"` is a
// perfectly ordinary comparison against the raw ParentSpanId string
// (span:parentID / IntrinsicParentID is the separate, TypeString intrinsic
// for that value, but reference Tempo also accepts bare `parent` compared to
// a literal) and must keep lowering to the bare column through the generic
// lowerFieldExpr path — only the boolean position needs the rewrite.
// ParentSpanId is a real ClickHouse String column, and a bare String
// reference is not a valid boolean filter predicate — ClickHouse rejects it
// with ILLEGAL_TYPE_OF_COLUMN_FOR_FILTER ("Invalid type for filter") — so
// synthesise the presence check instead of a bare ColumnRef. Mirrors
// rootnessReduction's identical `ParentSpanId != ""` pattern for the related
// nestedSetParent root-ness reduction.
//
// ctx identifies e's position in the spanset filter's boolean-expression
// tree — see notContext — which determines how a NOT node found at (or
// under) e resolves reference Tempo's evaluation quirks; see
// lowerUnaryNot's doc comment.
func lowerBooleanFieldExpr(e traceql.FieldExpression, s schema.Traces, ctx notContext) (chplan.Expr, error) {
	if attr, ok := fieldExprAttribute(e); ok && attr.Intrinsic == traceql.IntrinsicParent {
		return &chplan.Binary{
			Op:    chplan.OpNe,
			Left:  &chplan.ColumnRef{Name: s.ParentSpanIDColumn},
			Right: &chplan.LitString{V: ""},
		}, nil
	}
	if u, ok := asUnaryOperation(e); ok && u.Op == traceql.OpNot {
		return lowerUnaryNot(u, s, ctx)
	}
	return lowerFieldExpr(e, s)
}

// notContext identifies where a `!(...)` unary-NOT node sits in the
// spanset filter's boolean-expression tree. Reference Tempo's `/api/search`
// evaluates a NOT node differently depending on this position — see
// lowerUnaryNot's doc comment for the reference-probed rationale behind
// each case.
type notContext int

const (
	// notContextTop is the entire spanset filter predicate — the one
	// position where reference's constant-false-unless-doubly-negated
	// bug (issue #1712) applies.
	notContextTop notContext = iota
	// notContextAndOperand is a direct operand of a logical AND.
	notContextAndOperand
	// notContextOrOperand is a direct operand of a logical OR.
	notContextOrOperand
)

// lowerUnaryOperation handles the unary FieldExpression forms.
//
// `<attr> != nil` and `nil != <attr>` parse to UnaryOperation{OpExists}
// and `<attr> = nil` / `nil = <attr>` to UnaryOperation{OpNotExists}
// (the grammar rewrites the nil comparison — see upstream expr.y).
// Grafana's first-party Traces Drilldown app (preinstalled since
// Grafana 12.x) appends `&& <groupBy> != nil` to EVERY breakdown
// query — including intrinsic group-bys like
// `{nestedSetParent<0 && true && kind != nil} | rate() by(kind)` —
// so both the attribute and the intrinsic existence forms are
// load-bearing shapes.
//
// Reference semantics (grafana/tempo pkg/traceql, the AGPL upstream
// cerberus reimplements clean-room — test-only oracle, never linked):
//
//   - `x != nil` (OpExists) evaluates to `static.Type != TypeNil`
//     after executing x against the span (ast_execute.go). Intrinsic
//     columns are required parquet fields (vparquet4 schema.go: Kind,
//     StatusCode, Name, DurationNano, …) and the vparquet4 span
//     collector adds the intrinsic static unconditionally — even
//     kind=SPAN_KIND_UNSPECIFIED becomes a non-nil TypeKind static
//     (block_traceql.go spanCollector). So `<intrinsic> != nil`
//     matches EVERY span — it is a constant TRUE, not an
//     enum-zero/empty-string check. OTel-CH mirrors this exactly:
//     every intrinsic column is always present.
//   - `<intrinsic> = nil` (OpNotExists) is rejected by reference
//     validation: "X = nil is not valid because intrinsics cannot be
//     nil" (ast_validate.go UnaryOperation.validate), double-enforced
//     by vparquet4 checkConditions. Same for `resource.service.name
//     = nil`.
//   - `<span|resource attr> != nil` ≡ the attribute key exists on the
//     span — `mapContains(<carrier>, '<key>')`; `= nil` is the
//     negation (missing attributes surface as the nil sentinel the
//     OpNotExists branch matches — ast_execute.go + vparquet4
//     collectors).
//   - `<event.|link. attr> != nil` ≡ at least one event/link carries
//     the key (event attrs resolve per fetched Nested element);
//     `= nil` ≡ at least one event/link element LACKS the key (the
//     collectors surface fetched-but-null per-element attribute cells
//     as the matchable nil sentinel; spans with no elements at all
//     execute to StaticNil, which OpNotExists does NOT match —
//     Static.Equals is false when either side is TypeNil).
//   - Nested intrinsics (event:name / event:timeSinceStart /
//     link:traceID / link:spanID) `!= nil` ≡ the span has at least
//     one event/link: the sub-fields are required within each
//     element, so any element answers the probe.
//   - `childCount` conditions (any op, including != nil) error in
//     reference vparquet4 (checkConditions: "intrinsic 'childCount'
//     not supported in vParquet4") — keep rejecting.
func lowerUnaryOperation(u traceql.UnaryOperation, s schema.Traces, ctx notContext) (chplan.Expr, error) {
	switch u.Op {
	case traceql.OpExists, traceql.OpNotExists:
		attr, ok := fieldExprAttribute(u.Expression)
		if !ok {
			// A nil comparison whose operand is a compound expression
			// (arithmetic like `(span.a + 1) != nil`, a bare literal,
			// etc.) rather than a bare attribute. Reference Tempo accepts
			// it: the inner expression always executes to a non-nil Static
			// (a number when the attributes resolve, or StaticFalse via
			// the isMatchingOperand guard when one is absent — both
			// non-nil), so `!= nil` (OpExists) is constant-true and
			// `= nil` (OpNotExists) constant-false for every span. Fold to
			// a constant rather than rejecting.
			return &chplan.LitBool{V: u.Op == traceql.OpExists}, nil
		}
		return lowerNilComparison(u.Op, attr, s)
	case traceql.OpNot:
		return lowerUnaryNot(u, s, ctx)
	case traceql.OpSub:
		return lowerUnaryMinus(u, s)
	}
	return nil, fmt.Errorf("traceql: unary operator %s is unsupported", u.Op)
}

// lowerUnaryMinus lowers the arithmetic negation `-<numeric-expr>`
// (UnaryOperation{OpSub}) — e.g. `{ -span.foo > 0 }`,
// `{ -(span.a + span.b) = -5 }`, `{ -span.duration < 0ns }`.
//
// Reference semantics (grafana/tempo pkg/traceql ast_execute.go
// UnaryOperation.execute, OpSub branch — the AGPL upstream cerberus
// reimplements clean-room, test-only oracle, never linked): the operand executes to a
// Static; if its type is not numeric (int / float / duration) the
// reference returns an error, otherwise it returns `-1 * n` preserving
// the operand's numeric type (NewStaticInt / NewStaticFloat /
// NewStaticDuration). The parser AST-rewrites a unary minus over a
// constant operand into a folded negative Static (newUnaryOperation's
// `!referencesSpan()` simplification), so a UnaryOperation{OpSub} that
// survives to lowering always references a span — its operand is an
// attribute (or arithmetic over attributes), never a bare literal.
//
// We mirror reference `-1 * n` as `0 - <operand>`: a Binary{OpSub} with
// a zero-int left arm. This reuses the existing numeric-coercion path —
// the operand's FieldAccess children are wrapped in toFloat64OrNull by
// coerceFieldAccess so the Map(String,String) subscript computes as a
// number server-side, with absent/non-numeric values folding to NULL
// exactly as the binary-arithmetic path does. The enclosing comparison
// (or outer arithmetic) then coerces the whole `0 - operand` Binary via
// coerceNumericFieldAccess, so duration/int/float operands all land as
// Float64 — numerically identical to reference's typed negation for the
// comparisons TraceQL allows (`<neg-expr> <op> <numeric-literal>`).
func lowerUnaryMinus(u traceql.UnaryOperation, s schema.Traces) (chplan.Expr, error) {
	operand, err := lowerFieldExpr(u.Expression, s)
	if err != nil {
		return nil, err
	}
	return &chplan.Binary{
		Op:    chplan.OpSub,
		Left:  &chplan.LitInt{V: 0},
		Right: coerceFieldAccess(operand),
	}, nil
}

// lowerUnaryNot lowers the boolean negation `!( <bool-expr> )`.
//
// Reference Tempo's `/api/search` has three DIFFERENT evaluation
// behaviours for this shape depending on ctx, all pinned by differential
// probing against the same instance + fixture the compatibility/tempo
// harness uses (issues #1711/#1712):
//
//  1. Double negation always cancels, regardless of ctx: `!(!(x))`
//     evaluates exactly like `x` on its own. This is checked first and
//     unconditionally, before any of the position-specific rules below —
//     `!(!(!(!(kind = server))))` still cancels down to `kind = server`.
//
//  2. notContextTop — the NOT node IS the entire spanset filter
//     predicate (`{ !(<expr>) }`, nothing else in the `{ }`): reference
//     matches ZERO traces, regardless of what <expr> is (a single
//     comparison, a satisfiable AND, or a satisfiable OR).
//
//  3. notContextAndOperand / notContextOrOperand — the NOT node is a
//     direct operand of a logical AND/OR, and <expr> (after the double-
//     negation check above) is a single comparison (NOT itself a
//     compound AND/OR): reference silently drops the NOT operand,
//     behaving as if it were that combinator's identity element —
//     `x && !(<comparison>)` ≡ `x`, `x || !(<comparison>)` ≡ `x`. See
//     const_boolean_true_matches_all in
//     compatibility/tempo/driver/corpus/smoke.txtar (`("foo" != "bar")
//     && !("foo" = "bar")`, an AND-operand case) and
//     test/spec/traceql/unary_not_or_composition.txtar (an OR-operand
//     case) for corpus/fixture pins of this behaviour.
//
// Everything else — an AND/OR-operand NOT wrapping a compound (AND/OR)
// sub-expression — is genuinely unresolved: differential probing showed
// reference's behaviour there is data-dependent and does not reduce to a
// single clean rule (e.g. `x && !(a && b)` does not behave as a simple
// identity element the way `x && !(a)` does). No corpus case or fixture
// exercises that shape, so rather than guess and risk shipping a
// confidently-wrong answer, this function falls back to the
// logically-correct translation there: SQL `not(<inner>)` (Tempo's own
// UnaryOperation.execute OpNot semantics), wrapping the recursively
// lowered operand in `chplan.FuncCall{Name: "not"}`.
func lowerUnaryNot(u traceql.UnaryOperation, s schema.Traces, ctx notContext) (chplan.Expr, error) {
	if inner, ok := asUnaryNot(u.Expression); ok {
		// Double negation cancels unconditionally: lower the
		// doubly-wrapped operand as if neither `!` were there, at the
		// SAME ctx (a further NOT inside <inner> re-applies this same
		// rule, still in this position). The NOT operand is a boolean
		// position — same rationale as the AND/OR operand rewrite in
		// lowerBinaryOperation (see lowerBooleanFieldExpr's doc comment):
		// `!(!(parent))` must lower via `ParentSpanId != ""`, not a bare
		// ColumnRef.
		return lowerBooleanFieldExpr(inner, s, ctx)
	}
	if ctx == notContextTop {
		return &chplan.LitBool{V: false}, nil
	}
	if !isCompoundBoolean(u.Expression) {
		// notContextAndOperand → true (AND's identity element, so
		// foldTrivialBoolConjunct collapses `x && true` to `x`);
		// notContextOrOperand → false (OR's identity element, collapsing
		// `x || false` to `x`).
		return &chplan.LitBool{V: ctx == notContextAndOperand}, nil
	}
	inner, err := lowerBooleanFieldExpr(u.Expression, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.FuncCall{Name: "not", Args: []chplan.Expr{inner}}, nil
}

// isCompoundBoolean reports whether e is itself a logical AND/OR
// BinaryOperation (either the pointer or value FieldExpression form the
// parser produces) — the distinction lowerUnaryNot needs between "a
// single comparison wrapped by NOT" (reference drops it as the parent
// combinator's identity element) and "a compound AND/OR wrapped by NOT"
// (reference's behaviour there isn't a clean identity rule, so cerberus
// computes the correct answer instead — see lowerUnaryNot's doc comment).
func isCompoundBoolean(e traceql.FieldExpression) bool {
	b, ok := e.(*traceql.BinaryOperation)
	if !ok {
		return false
	}
	return b.Op == traceql.OpAnd || b.Op == traceql.OpOr
}

// asUnaryOperation unwraps e into a UnaryOperation value, handling both
// the pointer and value FieldExpression forms the parser produces (see
// lowerFieldExpr's dual type switch), regardless of operator.
func asUnaryOperation(e traceql.FieldExpression) (traceql.UnaryOperation, bool) {
	switch v := e.(type) {
	case traceql.UnaryOperation:
		return v, true
	case *traceql.UnaryOperation:
		return *v, true
	}
	return traceql.UnaryOperation{}, false
}

// asUnaryNot reports whether e is itself a `!(...)` unary-NOT node and,
// if so, returns its operand.
func asUnaryNot(e traceql.FieldExpression) (traceql.FieldExpression, bool) {
	if u, ok := asUnaryOperation(e); ok && u.Op == traceql.OpNot {
		return u.Expression, true
	}
	return nil, false
}

// lowerNilComparison lowers `<attr> != nil` (OpExists) / `<attr> = nil`
// (OpNotExists) per the reference semantics documented on
// lowerUnaryOperation.
func lowerNilComparison(op traceql.Operator, attr traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	if attr.Intrinsic != traceql.IntrinsicNone {
		return lowerIntrinsicNilComparison(op, attr, s)
	}
	if op == traceql.OpNotExists &&
		attr.Scope == traceql.AttributeScopeResource && attr.Name == "service.name" {
		// Reference rejection (pkg/traceql/ast_validate.go):
		// resource.service.name is mandatory on every OTLP resource.
		return nil, fmt.Errorf("traceql: %s = nil is not valid because resource.service.name cannot be nil", attr)
	}
	if attr.Scope == traceql.AttributeScopeLink || attr.Scope == traceql.AttributeScopeEvent {
		col, key, ok := nestedAttrTarget(attr, s)
		if !ok {
			return nil, fmt.Errorf("traceql: nil comparison on %s.%s is unsupported — the configured schema has no %s column", attr.Scope, attr.Name, attr.Scope)
		}
		presence := chplan.PresenceHasKey
		if op == traceql.OpNotExists {
			presence = chplan.PresenceLacksKey
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Presence: presence,
		}, nil
	}
	carrier := s.AttributesColumn
	switch attr.Scope {
	case traceql.AttributeScopeResource:
		carrier = s.ResourceAttributesColumn
	case traceql.AttributeScopeInstrumentation:
		// The OTel-CH traces schema materialises no scope-attributes map,
		// so a custom instrumentation.<key> is absent from every span.
		// Reference Tempo accepts the existence probe and resolves the
		// absent key to StaticNil: `!= nil` (OpExists) is false for every
		// span, `= nil` (OpNotExists) is true. Mirror that as a constant
		// predicate rather than rejecting (or silently reading
		// SpanAttributes).
		if s.ScopeAttributesColumn == "" {
			return &chplan.LitBool{V: op == traceql.OpNotExists}, nil
		}
		carrier = s.ScopeAttributesColumn
	}
	contains := &chplan.FuncCall{Name: "mapContains", Args: []chplan.Expr{
		&chplan.ColumnRef{Name: carrier},
		&chplan.LitString{V: attr.Name},
	}}
	if op == traceql.OpNotExists {
		return &chplan.FuncCall{Name: "not", Args: []chplan.Expr{contains}}, nil
	}
	return contains, nil
}

// lowerIntrinsicNilComparison lowers nil comparisons whose subject is
// an intrinsic. See lowerUnaryOperation for the reference-semantics
// derivation of each branch.
func lowerIntrinsicNilComparison(op traceql.Operator, attr traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	if op == traceql.OpNotExists {
		// Reference rejection (pkg/traceql/ast_validate.go
		// UnaryOperation.validate; vparquet4 checkConditions repeats
		// it at fetch time).
		return nil, fmt.Errorf("traceql: %s = nil is not valid because intrinsics cannot be nil", attr.Intrinsic)
	}
	switch attr.Intrinsic {
	case traceql.IntrinsicChildCount:
		// Reference errors on every childCount condition (vparquet4
		// checkConditions: "not supported in vParquet4").
		return nil, fmt.Errorf(
			"traceql: intrinsic %s requires per-span child counts the OTel ClickHouse schema does not materialise", attr.Intrinsic,
		)
	case traceql.IntrinsicEventName, traceql.IntrinsicEventTimeSinceStart:
		if s.EventsColumn == "" {
			return nil, fmt.Errorf("traceql: nil comparison on intrinsic %s is unsupported — the configured schema has no events column", attr.Intrinsic)
		}
		// ≥1 event: Events.Name is a required sub-field of every
		// Nested element, so element presence answers the probe.
		return &chplan.NestedArrayExists{
			Column:   s.EventsColumn,
			SubField: "Name",
			Presence: chplan.PresenceHasKey,
		}, nil
	case traceql.IntrinsicLinkTraceID, traceql.IntrinsicLinkSpanID:
		if s.LinksColumn == "" {
			return nil, fmt.Errorf("traceql: nil comparison on intrinsic %s is unsupported — the configured schema has no links column", attr.Intrinsic)
		}
		// ≥1 link, same shape as the event probe.
		return &chplan.NestedArrayExists{
			Column:   s.LinksColumn,
			SubField: "TraceId",
			Presence: chplan.PresenceHasKey,
		}, nil
	}
	// Every other intrinsic — kind, status, name, duration,
	// statusMessage, trace/span IDs, parent, nested-set positions,
	// trace-scoped values (rootName / rootServiceName /
	// traceDuration), instrumentation:name/version — is an
	// always-present value in reference Tempo (required parquet
	// columns + unconditional collector statics), so the existence
	// probe is TRUE for every span.
	return &chplan.LitBool{V: true}, nil
}

// fieldExprAttribute unwraps a FieldExpression into its Attribute when
// it is a bare attribute reference (pointer or value form).
func fieldExprAttribute(e traceql.FieldExpression) (traceql.Attribute, bool) {
	switch v := e.(type) {
	case *traceql.Attribute:
		if v == nil {
			return traceql.Attribute{}, false
		}
		return *v, true
	case traceql.Attribute:
		return v, true
	}
	return traceql.Attribute{}, false
}

// lowerAttributeExpr wraps lowerAttribute with a guard: link- /
// event-scoped attributes can only appear inside a comparison
// (lowerBinaryOperation intercepts them and returns a
// NestedArrayExists). Reaching this path means the attribute is being
// used as a scalar value (e.g. `| select(link.span_id)`) which would
// silently dereference the wrong CH column — error out so the operator
// can surface the gap rather than ship wrong SQL.
func lowerAttributeExpr(a traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	if a.Scope == traceql.AttributeScopeLink || a.Scope == traceql.AttributeScopeEvent {
		// A bare event-/link-scoped attribute as the whole filter
		// expression (`{ event.name }`) is a truthiness test. Reference
		// Tempo accepts it: the SpansetFilter requires the expression to
		// type to a boolean or attribute (ast_validate.go
		// SpansetFilter.validate), and a bare attribute is matched when it
		// resolves to a non-nil truthy value — for the per-element Nested
		// columns that is "at least one element carries the key". Lower to
		// the same hasKey existence probe `<attr> != nil` produces rather
		// than rejecting.
		col, key, ok := nestedAttrTarget(a, s)
		if !ok {
			return nil, fmt.Errorf("traceql: nil comparison on %s.%s is unsupported — the configured schema has no %s column", a.Scope, a.Name, a.Scope)
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Presence: chplan.PresenceHasKey,
		}, nil
	}
	// Nested-set intrinsics never resolve to a column here: comparisons
	// are intercepted by lowerNestedSetBinary and select() projections
	// by lowerSelect's NestedSetAnnotate wrap; any other use would
	// silently dereference SpanAttributes['nestedSet…'] (which the
	// OTel-CH exporter never writes) — error instead.
	switch a.Intrinsic {
	case traceql.IntrinsicNestedSetParent, traceql.IntrinsicNestedSetLeft, traceql.IntrinsicNestedSetRight:
		return nil, fmt.Errorf("traceql: intrinsic %s is only supported in root-ness comparisons (e.g. nestedSetParent < 0) and select() projections", a.Intrinsic)
	}
	return lowerAttribute(a, s)
}

func lowerBinaryOperation(b *traceql.BinaryOperation, s schema.Traces) (chplan.Expr, error) {
	// `attr = a || attr = b` is folded by the Tempo parser into a single
	// `attr IN [a, b]` BinaryOperation (OpIn / OpNotIn) — Grafana's Traces
	// Drilldown emits this shape for multi-value filters. Intercept it
	// before mapBinaryOp (which has no IN op) and lower to a flat
	// membership test.
	if b.Op == traceql.OpIn || b.Op == traceql.OpNotIn {
		return lowerInOperation(b, s)
	}
	op, err := mapBinaryOp(b.Op)
	if err != nil {
		return nil, err
	}
	// Comparisons against a carrier the OTel-CH schema does not
	// materialise (instrumentation-scoped attributes; the per-event
	// intrinsics childCount / event:timeSinceStart) resolve to
	// StaticNil in reference execution, so the comparison is
	// constant-false (the isMatchingOperand guard never matches a nil
	// operand). Reference `/api/search` accepts these — fold them to a
	// constant predicate instead of rejecting. Equality/inequality both
	// collapse to false: `nil = x` and `nil != x` are both StaticFalse
	// upstream.
	if expr, handled := lowerAbsentFieldBinary(b, s); handled {
		return expr, nil
	}
	// Trace-scoped root-identity intrinsics (rootName / rootServiceName
	// / traceDuration, bare or trace:-scoped spelling) have no per-span
	// column — the value depends on every span in the trace — but
	// reference Tempo DOES resolve them (from the root span / the
	// trace's overall time bounds), so the comparison is real rather
	// than constant-false. Intercept before generic lowering would
	// mis-resolve the name to a SpanAttributes map lookup.
	if expr, handled, err := lowerTraceScopedBinary(b, op, s); handled {
		return expr, err
	}
	// Nested-set intrinsics (nestedSetParent / nestedSetLeft /
	// nestedSetRight) have no OTel-CH backing column; intercept them
	// before generic lowering would mis-resolve the name to a
	// SpanAttributes map lookup. The root-span idiom
	// (`nestedSetParent < 0`) lowers exactly; anything else errors.
	if expr, handled, err := lowerNestedSetBinary(b, op, s); handled {
		return expr, err
	}
	// Nested intrinsics (event:name / link:traceID / link:spanID) live on
	// the OTel-CH `Events` / `Links` Nested columns as direct subfields
	// (Events.Name, Links.TraceId, Links.SpanId) rather than inside the
	// per-row Attributes map; intercept them before generic lowering
	// would mis-resolve the spelling to a SpanAttributes lookup.
	if expr, handled, err := lowerNestedIntrinsicBinary(b, op, s); handled {
		return expr, err
	}
	// TraceQL link / event spanset filters live on the OTel-CH `Links` /
	// `Events` Nested columns. Their predicate shape is
	//   arrayExists(x -> x[<key>] <op> <value>, <Col>.Attributes)
	// rather than a flat column comparison; capture that as a
	// NestedArrayExists before generic Binary lowering kicks in.
	if nested, ok := lowerNestedAttrBinary(b, op, s); ok {
		return nested, nil
	}
	// AND/OR operands are boolean positions — route through
	// lowerBooleanFieldExpr so a bare `parent` operand (e.g.
	// `{ parent && resource.service.name = "api" }`) lowers to the
	// `ParentSpanId != ""` presence check rather than a bare (invalid
	// filter-predicate-typed) ColumnRef. Every other op's operands are
	// value positions (`parent = "<hex>"` must keep reading the raw
	// column), so they keep going through the generic lowerFieldExpr.
	lowerOperand := lowerFieldExpr
	if op == chplan.OpAnd || op == chplan.OpOr {
		// AND/OR operands are never the top of the spanset filter's
		// boolean tree, so a NOT operand here never takes
		// reference's top-level-only constant-false bug path — see
		// lowerUnaryNot's doc comment for what notContextAndOperand /
		// notContextOrOperand each resolve to instead.
		operandCtx := notContextOrOperand
		if op == chplan.OpAnd {
			operandCtx = notContextAndOperand
		}
		lowerOperand = func(e traceql.FieldExpression, s schema.Traces) (chplan.Expr, error) {
			return lowerBooleanFieldExpr(e, s, operandCtx)
		}
	}
	lhs, err := lowerOperand(b.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerOperand(b.RHS, s)
	if err != nil {
		return nil, err
	}
	// Map(String, String) coercion: SpanAttributes / ResourceAttributes
	// are typed Map(String, String) in OTel-CH, so a bare
	// `SpanAttributes['http.status_code'] >= 500` comparison fails in
	// ClickHouse with NO_COMMON_TYPE ("there is no supertype for types
	// String, UInt8"). When the lowered Binary has numeric semantics
	// (arithmetic op, or comparison whose peer is a numeric expression)
	// we wrap any FieldAccess child in `toFloat64(...)` so the cast
	// happens server-side. Float64 widens both int and float literals
	// without precision loss for the magnitudes typical of attribute
	// values (HTTP status codes, percentages, sizes).
	lhs, rhs = coerceNumericFieldAccess(op, lhs, rhs)
	// Boolean coercion: the OTel-CH exporter stringifies bool-typed
	// attribute values into the Map(String, String) carriers as
	// "true" / "false", so `{ .cache.hit = true }` must compare against
	// the STRING form — `SpanAttributes['cache.hit'] = 'true'`. Without
	// the rewrite ClickHouse rejects the String-vs-Bool comparison with
	// NO_COMMON_TYPE (the showcase's static:bool panel 502'd).
	lhs, rhs = coerceBoolFieldAccess(op, lhs, rhs)
	if folded, ok := foldTrivialBoolConjunct(op, lhs, rhs); ok {
		return folded, nil
	}
	return &chplan.Binary{Op: op, Left: lhs, Right: rhs}, nil
}

// foldTrivialBoolConjunct collapses a logical AND / OR with a constant-true or
// constant-false operand to the other operand (the algebraic identity). It
// targets the shape Grafana's Traces Drilldown app appends to every breakdown
// query — `{nestedSetParent<0 && true}` — where the `&& true` conjunct is the
// literal `traceql.Static{Bool:true}` and lowers to a chplan.LitBool. Folding
// it keeps the emitted predicate to the meaningful conjunct (`ParentSpanId = ”`)
// instead of `ParentSpanId = ” AND true`, which is byte-noise CH would
// evaluate per row.
//
//   - AND true  → other      AND false → false
//   - OR  false → other      OR  true  → true
//
// Only logical AND/OR fold here; arithmetic/comparison ops never carry a bare
// LitBool operand. ok is false (caller keeps the Binary) for any non-logical op
// or when neither operand is a constant boolean.
func foldTrivialBoolConjunct(op chplan.BinaryOp, lhs, rhs chplan.Expr) (chplan.Expr, bool) {
	if op != chplan.OpAnd && op != chplan.OpOr {
		return nil, false
	}
	lb, lok := lhs.(*chplan.LitBool)
	rb, rok := rhs.(*chplan.LitBool)
	switch op {
	case chplan.OpAnd:
		// `x AND true` → x; `true AND x` → x; either side false → false.
		if lok {
			if !lb.V {
				return &chplan.LitBool{V: false}, true
			}
			return rhs, true
		}
		if rok {
			if !rb.V {
				return &chplan.LitBool{V: false}, true
			}
			return lhs, true
		}
	case chplan.OpOr:
		// `x OR false` → x; `false OR x` → x; either side true → true.
		if lok {
			if lb.V {
				return &chplan.LitBool{V: true}, true
			}
			return rhs, true
		}
		if rok {
			if rb.V {
				return &chplan.LitBool{V: true}, true
			}
			return lhs, true
		}
	}
	return nil, false
}

// lowerInOperation lowers a folded membership comparison
// `attr IN [v0, v1, …]` (OpIn) / `attr NOT IN [...]` (OpNotIn). The
// Tempo parser collapses `attr = a || attr = b` into this single
// BinaryOperation shape, which Grafana's Traces Drilldown emits for
// multi-value filters; reference Tempo accepts it (enum_operators.go
// binaryTypesValid lists OpIn/OpNotIn for every operand type), so
// cerberus must too.
//
// The membership set lowers to a flat chplan.InList (constant parser
// depth — see chplan.InList's doc on the max_parser_depth trap that an
// OR-chain would hit). When the attribute resolves to a column with no
// OTel-CH backing (instrumentation-scoped / nested-set / trace-scoped
// intrinsics) the comparison is constant per reference's StaticNil
// execution semantics: a missing attribute never matches any RHS, so
// `IN` is constant-false and `NOT IN` constant-true.
func lowerInOperation(b *traceql.BinaryOperation, s schema.Traces) (chplan.Expr, error) {
	attr, ok := fieldExprAttribute(b.LHS)
	if !ok {
		return nil, fmt.Errorf("traceql: IN comparison LHS must be an attribute reference, got %T", b.LHS)
	}
	st, ok := fieldExprStatic(b.RHS)
	if !ok {
		return nil, fmt.Errorf("traceql: IN comparison RHS must be a literal array, got %T", b.RHS)
	}
	elems, err := lowerStaticArray(st)
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		// Empty membership set: `x IN []` matches nothing, `x NOT IN []`
		// matches everything (reference array semantics).
		return &chplan.LitBool{V: b.Op == traceql.OpNotIn}, nil
	}

	if pred, absent := absentAttributePredicate(attr, s, b.Op == traceql.OpNotIn); absent {
		return pred, nil
	}

	if valueNode, ok := traceScopedValueNode(attr.Intrinsic, s); ok {
		in := chplan.Expr(&chplan.InList{Left: &chplan.ColumnRef{Name: traceScopedValueAlias}, List: elems})
		if b.Op == traceql.OpNotIn {
			in = &chplan.FuncCall{Name: "not", Args: []chplan.Expr{in}}
		}
		return traceScopedInSubquery(valueNode, in, s), nil
	}

	left, lerr := lowerAttribute(attr, s)
	if lerr != nil {
		return nil, lerr
	}
	// String-map carriers store every value as text, so coerce numeric /
	// bool list literals to their stringified OTel-CH encoding when the
	// LHS is a Map subscript (mirrors coerceBoolFieldAccess / the numeric
	// coercion path for scalar comparisons).
	if _, isField := left.(*chplan.FieldAccess); isField {
		elems = stringifyListForMap(elems)
	}
	in := &chplan.InList{Left: left, List: elems}
	if b.Op == traceql.OpNotIn {
		return &chplan.FuncCall{Name: "not", Args: []chplan.Expr{in}}, nil
	}
	return in, nil
}

// lowerAbsentFieldBinary intercepts a comparison where either operand
// is an attribute the OTel-CH schema does not materialise (see
// attributeHasNoBacking). Reference Tempo resolves the absent operand
// to StaticNil; the type-mismatch guard then makes every comparison
// (=, !=, <, <=, >, >=, =~, !~) evaluate StaticFalse, so the predicate
// is constant-false. Returns handled=false when neither operand is an
// unbacked attribute (the caller continues with generic lowering).
func lowerAbsentFieldBinary(b *traceql.BinaryOperation, s schema.Traces) (chplan.Expr, bool) {
	if a, ok := fieldExprAttribute(b.LHS); ok && attributeHasNoBacking(a, s) {
		return &chplan.LitBool{V: false}, true
	}
	if a, ok := fieldExprAttribute(b.RHS); ok && attributeHasNoBacking(a, s) {
		return &chplan.LitBool{V: false}, true
	}
	return nil, false
}

// absentAttributePredicate reports whether attr resolves to a column
// the OTel-CH traces schema does not materialise, and if so returns the
// constant predicate that mirrors reference Tempo's StaticNil execution
// semantics: a missing attribute compared against any typed RHS never
// matches (the isMatchingOperand guard in BinaryOperation.execute
// returns StaticFalse), so a positive membership / comparison is
// constant-false and its negation constant-true.
//
// Only the genuinely-unbacked carriers report absent here:
// instrumentation-scoped attributes (no scope-attributes map) and the
// per-event / per-span intrinsics with no per-span column. Span /
// resource attributes and intrinsics that DO map to a column
// (Duration, SpanName, StatusCode, …) return absent=false so the
// caller lowers them against their real carrier. Nested-set intrinsics
// are handled by their own dedicated path (lowerNestedSetBinary) and
// are not classified here. The trace-scoped root-identity intrinsics
// (rootName / rootServiceName / traceDuration) used to be classified
// absent too; they now have a real (correlated-subquery) lowering via
// lowerTraceScopedBinary, so attributeHasNoBacking no longer reports
// them (see issue #1711).
func absentAttributePredicate(attr traceql.Attribute, s schema.Traces, negated bool) (chplan.Expr, bool) {
	if !attributeHasNoBacking(attr, s) {
		return nil, false
	}
	return &chplan.LitBool{V: negated}, true
}

// attributeHasNoBacking reports whether attr names a carrier the OTel-CH
// traces schema does not materialise. Instrumentation-scoped attributes
// have no scope-attributes map; traceStartTime / childCount /
// event:timeSinceStart have no per-span column and no aggregate
// lowering either. Every other attribute (span / resource maps,
// intrinsics with a column, and the trace-scoped root-identity
// intrinsics lowerTraceScopedBinary now resolves via a correlated
// subquery) has a real backing.
func attributeHasNoBacking(attr traceql.Attribute, s schema.Traces) bool {
	if attr.Intrinsic == traceql.IntrinsicNone {
		return attr.Scope == traceql.AttributeScopeInstrumentation && s.ScopeAttributesColumn == ""
	}
	switch attr.Intrinsic {
	case traceql.IntrinsicTraceStartTime, traceql.IntrinsicChildCount,
		traceql.IntrinsicEventTimeSinceStart:
		return true
	}
	return false
}

// isTraceScopedIntrinsic reports whether i names one of the "trace root
// identity" intrinsics — rootName / rootServiceName / traceDuration —
// bare or trace:-scoped spelling. The OTel-CH schema has no per-span
// column for these: the value depends on every span in the trace (which
// one is root, when the trace started/ended), so lowerTraceScopedBinary
// resolves them via a correlated per-trace subquery instead of a direct
// column reference. traceql.ScopedIntrinsicTrace* are currently
// unreachable from the parser — scopedIntrinsic in ast/parser.go maps
// the trace:-prefixed spellings onto the SAME unscoped Intrinsic
// constants bareIntrinsic produces — so this switch defensively
// includes them too, at zero runtime cost today, in case a future
// parser change starts emitting them directly.
func isTraceScopedIntrinsic(i traceql.Intrinsic) bool {
	switch i {
	case traceql.IntrinsicTraceRootService, traceql.ScopedIntrinsicTraceRootService,
		traceql.IntrinsicTraceRootSpan, traceql.ScopedIntrinsicTraceRootName,
		traceql.IntrinsicTraceDuration, traceql.ScopedIntrinsicTraceDuration:
		return true
	}
	return false
}

// traceScopedRootSpanCond is the OTel-CH root-span test the /api/search
// root-lookup decoration (internal/api/tempo/root_lookup.go's rootCond)
// and the `| compare()` root lookup (metrics_compare.go's
// compareRootLookup) both build independently: a span with no parent
// (empty, or the all-zero 16-hex-digit ParentSpanId some exporters
// write instead of empty) is the trace's root.
func traceScopedRootSpanCond(s schema.Traces) chplan.Expr {
	return &chplan.InList{
		Left: &chplan.ColumnRef{Name: s.ParentSpanIDColumn},
		List: []chplan.Expr{&chplan.LitString{V: ""}, &chplan.LitString{V: "0000000000000000"}},
	}
}

// traceScopedValueAlias is the synthetic SELECT-list alias
// traceScopedValueNode plants for the per-trace root-identity value a
// caller's Filter predicate then compares against.
const traceScopedValueAlias = "_cerb_trace_scoped_val"

// traceScopedValueNode returns the un-filtered per-trace aggregate for a
// trace-scoped root-identity intrinsic: one row per TraceId, with the
// resolved root-service / root-name / trace-duration value under
// traceScopedValueAlias. rootService/rootName resolve via a single
// argMinIf (the root span's ServiceName/SpanName, keyed by earliest
// Timestamp among root-flagged rows, mirroring root_lookup.go's
// technique); duration needs two aggregates (max(end) - min(start)), so
// it routes through an extra Project layer that computes the
// subtraction the single-AggFunc SELECT-list shape can't express
// directly. ok is false for any intrinsic other than the three
// trace-scoped ones.
func traceScopedValueNode(i traceql.Intrinsic, s schema.Traces) (node chplan.Node, ok bool) {
	base := &chplan.Scan{Table: s.SpansTable}
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}
	groupByAliases := []string{s.TraceIDColumn}
	rootCond := traceScopedRootSpanCond(s)

	switch i {
	case traceql.IntrinsicTraceRootService, traceql.ScopedIntrinsicTraceRootService:
		return &chplan.Aggregate{
			Input: base, GroupBy: groupBy, GroupByAliases: groupByAliases,
			AggFuncs: []chplan.AggFunc{{
				Name: "argMinIf",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.ServiceNameColumn},
					&chplan.ColumnRef{Name: s.TimestampColumn},
					rootCond,
				},
				Alias: traceScopedValueAlias,
			}},
		}, true
	case traceql.IntrinsicTraceRootSpan, traceql.ScopedIntrinsicTraceRootName:
		return &chplan.Aggregate{
			Input: base, GroupBy: groupBy, GroupByAliases: groupByAliases,
			AggFuncs: []chplan.AggFunc{{
				Name: "argMinIf",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.SpanNameColumn},
					&chplan.ColumnRef{Name: s.TimestampColumn},
					rootCond,
				},
				Alias: traceScopedValueAlias,
			}},
		}, true
	case traceql.IntrinsicTraceDuration, traceql.ScopedIntrinsicTraceDuration:
		tsNs := func() chplan.Expr {
			return &chplan.FuncCall{Name: "toUnixTimestamp64Nano", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}}
		}
		const startAlias, endAlias = "_cerb_trace_start_ns", "_cerb_trace_end_ns"
		agg := &chplan.Aggregate{
			Input: base, GroupBy: groupBy, GroupByAliases: groupByAliases,
			AggFuncs: []chplan.AggFunc{
				{Name: "min", Args: []chplan.Expr{tsNs()}, Alias: startAlias},
				{Name: "max", Args: []chplan.Expr{
					&chplan.Binary{
						Op:   chplan.OpAdd,
						Left: tsNs(),
						Right: &chplan.FuncCall{
							Name: "toInt64",
							Args: []chplan.Expr{&chplan.ColumnRef{Name: s.DurationColumn}},
						},
					},
				}, Alias: endAlias},
			},
		}
		return &chplan.Project{
			Input: agg,
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}, Alias: s.TraceIDColumn},
				{
					Expr: &chplan.Binary{
						Op:    chplan.OpSub,
						Left:  &chplan.ColumnRef{Name: endAlias},
						Right: &chplan.ColumnRef{Name: startAlias},
					},
					Alias: traceScopedValueAlias,
				},
			},
		}, true
	}
	return nil, false
}

// traceScopedInSubquery wraps pred (a predicate over traceScopedValueAlias)
// around valueNode and narrows the result back down to the bare TraceId
// column an outer `TraceId IN (...)` membership test needs — CH rejects
// a multi-column subquery on the right of a single-column IN. This is the
// SQL-HAVING shape expressed as nested subqueries instead: `SELECT TraceId
// FROM (SELECT * FROM (<valueNode>) WHERE <pred>)`.
func traceScopedInSubquery(valueNode chplan.Node, pred chplan.Expr, s schema.Traces) chplan.Expr {
	filtered := &chplan.Filter{Input: valueNode, Predicate: pred}
	proj := &chplan.Project{
		Input:       filtered,
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}}},
	}
	return &chplan.InSubquery{Left: &chplan.ColumnRef{Name: s.TraceIDColumn}, Subquery: proj}
}

// lowerTraceScopedBinary intercepts a comparison against one of the
// trace-scoped root-identity intrinsics (rootName / rootServiceName /
// traceDuration). These have no per-span column — the value depends on
// every span in the trace — so attributeHasNoBacking used to fold every
// comparison against them to constant-false. Reference Tempo instead
// computes the value from the whole trace and answers real matches
// (issue #1711); this lowers to `TraceId IN (<per-trace aggregate,
// filtered by the comparison>)`, evaluated as its own GROUP BY over the
// Spans table so ClickHouse aggregates root identity / duration once
// per trace and the outer predicate only ever compares real values.
//
// Returns handled=false when neither operand references a trace-scoped
// intrinsic (the caller continues with the next interceptor / generic
// lowering).
func lowerTraceScopedBinary(b *traceql.BinaryOperation, op chplan.BinaryOp, s schema.Traces) (chplan.Expr, bool, error) {
	lAttr, lok := fieldExprAttribute(b.LHS)
	rAttr, rok := fieldExprAttribute(b.RHS)

	var attr traceql.Attribute
	var valueSide traceql.FieldExpression
	effectiveOp := op
	switch {
	case lok && isTraceScopedIntrinsic(lAttr.Intrinsic):
		attr, valueSide = lAttr, b.RHS
	case rok && isTraceScopedIntrinsic(rAttr.Intrinsic):
		attr, valueSide = rAttr, b.LHS
		effectiveOp = flipComparisonOp(op)
	default:
		return nil, false, nil
	}

	valueNode, ok := traceScopedValueNode(attr.Intrinsic, s)
	if !ok {
		return nil, false, nil
	}
	rhs, err := lowerFieldExpr(valueSide, s)
	if err != nil {
		return nil, true, err
	}
	pred := &chplan.Binary{Op: effectiveOp, Left: &chplan.ColumnRef{Name: traceScopedValueAlias}, Right: rhs}
	return traceScopedInSubquery(valueNode, pred, s), true, nil
}

// lowerStaticArray turns a TraceQL array Static (TypeStringArray /
// TypeIntArray / TypeFloatArray / TypeBooleanArray) into chplan literal
// elements via the public array accessors.
func lowerStaticArray(st traceql.Static) ([]chplan.Expr, error) {
	if strs, ok := st.StringArray(); ok {
		out := make([]chplan.Expr, len(strs))
		for i, v := range strs {
			out[i] = &chplan.LitString{V: v}
		}
		return out, nil
	}
	if ints, ok := st.IntArray(); ok {
		out := make([]chplan.Expr, len(ints))
		for i, v := range ints {
			out[i] = &chplan.LitInt{V: int64(v)}
		}
		return out, nil
	}
	if floats, ok := st.FloatArray(); ok {
		out := make([]chplan.Expr, len(floats))
		for i, v := range floats {
			out[i] = &chplan.LitFloat{V: v}
		}
		return out, nil
	}
	if bools, ok := st.BooleanArray(); ok {
		out := make([]chplan.Expr, len(bools))
		for i, v := range bools {
			out[i] = &chplan.LitBool{V: v}
		}
		return out, nil
	}
	return nil, fmt.Errorf("traceql: IN comparison RHS literal type %s is not an array", st.Type)
}

// stringifyListForMap rewrites numeric / bool list literals into the
// String form the OTel-CH Map(String, String) carriers store, so an
// `IN` test against a map subscript compares like-typed values rather
// than tripping NO_COMMON_TYPE.
func stringifyListForMap(elems []chplan.Expr) []chplan.Expr {
	out := make([]chplan.Expr, len(elems))
	for i, e := range elems {
		switch v := e.(type) {
		case *chplan.LitBool:
			if v.V {
				out[i] = &chplan.LitString{V: "true"}
			} else {
				out[i] = &chplan.LitString{V: "false"}
			}
		default:
			out[i] = e
		}
	}
	return out
}

// coerceBoolFieldAccess rewrites a LitBool compared against a
// FieldAccess into the OTel-CH string encoding ("true" / "false").
// Only equality ops apply — TraceQL's type checker
// (binaryTypeValid) rejects ordered comparisons on booleans before
// lowering ever runs.
func coerceBoolFieldAccess(op chplan.BinaryOp, lhs, rhs chplan.Expr) (chplan.Expr, chplan.Expr) {
	if op != chplan.OpEq && op != chplan.OpNe {
		return lhs, rhs
	}
	boolToString := func(e chplan.Expr) chplan.Expr {
		b, ok := e.(*chplan.LitBool)
		if !ok {
			return e
		}
		if b.V {
			return &chplan.LitString{V: "true"}
		}
		return &chplan.LitString{V: "false"}
	}
	if _, ok := lhs.(*chplan.FieldAccess); ok {
		return lhs, boolToString(rhs)
	}
	if _, ok := rhs.(*chplan.FieldAccess); ok {
		return boolToString(lhs), rhs
	}
	return lhs, rhs
}

// coerceNumericFieldAccess wraps FieldAccess children in toFloat64(...)
// when the parent Binary needs numeric semantics:
//
//   - Arithmetic ops (+ / - / * / / / % / ^) always coerce both sides,
//     recursing into nested arithmetic so a chain like `.a + .b + .c`
//     yields `toFloat64(.a) + toFloat64(.b) + toFloat64(.c)`.
//
//   - Comparison ops (= / != / < / <= / > / >=) coerce both sides only
//     when at least one side is a numeric expression (literal int /
//     float, an arithmetic Binary, or an already-coerced FuncCall).
//     The "both sides" rule covers commutative comparisons where the
//     literal appears on the left (`500 <= span.http.status_code`).
//
//   - Regex / logical ops (=~ / !~ / AND / OR) leave both sides alone
//     because their operands are strings or booleans.
//
// FieldAccess that resolves to an intrinsic column (e.g. Duration,
// already Int64) doesn't reach this path — intrinsics lower to a
// ColumnRef, not a FieldAccess. So the wrap is restricted to the
// Map(String, String) carriers by construction.
func coerceNumericFieldAccess(op chplan.BinaryOp, lhs, rhs chplan.Expr) (chplan.Expr, chplan.Expr) {
	if isArithmeticOp(op) {
		return coerceFieldAccess(lhs), coerceFieldAccess(rhs)
	}
	if isComparisonOp(op) && (isNumericExpr(lhs) || isNumericExpr(rhs)) {
		return coerceFieldAccess(lhs), coerceFieldAccess(rhs)
	}
	// Two bare attribute accesses under an ORDERING comparison (`span.a > span.b`,
	// `a <= b`) carry numeric intent: Tempo compares them by their typed value,
	// but OTel-CH stores every attribute as String, so a raw compare is
	// lexicographic — `'10' > '5'` is false. Coerce both via toFloat64OrNull, the
	// same path the literal-hint branch above takes. Equality (`=` / `!=`) stays a
	// string compare (the common attribute-equality / label-matcher case); a
	// legitimately-string ordering compare coerces to NULL and drops — the
	// identical trade-off the literal-hint path already accepts.
	if isOrderingComparisonOp(op) {
		_, lhsField := lhs.(*chplan.FieldAccess)
		_, rhsField := rhs.(*chplan.FieldAccess)
		if lhsField && rhsField {
			return coerceFieldAccess(lhs), coerceFieldAccess(rhs)
		}
	}
	return lhs, rhs
}

// isOrderingComparisonOp reports whether op is an ordering comparison
// (`<` `<=` `>` `>=`) — the comparison subset that implies numeric intent on two
// attribute operands. Equality (`=` `!=`) is excluded: attribute equality is
// string-valued (label-matcher semantics).
func isOrderingComparisonOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// coerceFieldAccess wraps every FieldAccess inside expr in
// toFloat64OrNull(...), recursing into arithmetic Binary nodes so a
// nested `.a + .b` becomes `toFloat64OrNull(.a) + toFloat64OrNull(.b)`.
// Non-arithmetic sub-expressions (literals, ColumnRefs, FuncCalls
// already produced by a deeper coercion) pass through unchanged.
//
// Why OrNull rather than the bare cast: the Map(String, String)
// subscript returns ” for absent keys and arbitrary text for
// non-numeric values; bare toFloat64(”) makes ClickHouse abort the
// whole query ("Cannot parse string") — so any numeric comparison over
// a table where even ONE row lacks the attribute 502'd. OrNull turns
// unparseable values into NULL, the comparison evaluates NULL, and
// WHERE drops the row — exactly Tempo's reference semantics (a span
// without the attribute, or with a non-numeric value, simply doesn't
// match). OrZero would instead make `{ .x < 5 }` match spans that
// never carried x at all.
func coerceFieldAccess(expr chplan.Expr) chplan.Expr {
	switch v := expr.(type) {
	case *chplan.FieldAccess:
		return &chplan.FuncCall{Name: "toFloat64OrNull", Args: []chplan.Expr{v}}
	case *chplan.Binary:
		if isArithmeticOp(v.Op) {
			return &chplan.Binary{
				Op:    v.Op,
				Left:  coerceFieldAccess(v.Left),
				Right: coerceFieldAccess(v.Right),
			}
		}
	}
	return expr
}

// isArithmeticOp reports whether op is one of the numeric arithmetic
// operators where both operands must compute as numbers.
func isArithmeticOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpAdd, chplan.OpSub, chplan.OpMul, chplan.OpDiv, chplan.OpMod, chplan.OpPow:
		return true
	}
	return false
}

// isComparisonOp reports whether op is one of the value-comparison
// operators eligible for numeric-attribute coercion. Excludes regex
// (=~ / !~) which operate on strings, and AND / OR which compose
// booleans.
func isComparisonOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// isNumericExpr reports whether expr has numeric semantics on the CH
// side — a numeric literal, an arithmetic Binary, or a FuncCall
// (which in this lowering only comes from a prior toFloat64 wrap).
// Used to decide whether a comparison's "other side" needs a numeric
// peer, which is what triggers FieldAccess coercion.
//
// ColumnRef deliberately does NOT count as numeric here: the only
// intrinsic ColumnRef that's numeric in OTel-CH is Duration, and a
// `Duration > 100ms` comparison doesn't need attribute coercion (both
// sides are already typed Int64). Treating ColumnRef as non-numeric
// keeps `{ name = "checkout" }` (string intrinsic) from incorrectly
// triggering toFloat64 wraps on the literal side.
func isNumericExpr(expr chplan.Expr) bool {
	if b, ok := expr.(*chplan.Binary); ok {
		return isArithmeticOp(b.Op)
	}
	switch expr.(type) {
	case *chplan.LitInt, *chplan.LitFloat, *chplan.FuncCall:
		return true
	}
	return false
}

// lowerNestedSetBinary intercepts comparisons against the nested-set
// intrinsics (`nestedSetParent` / `nestedSetLeft` / `nestedSetRight`).
//
// Tempo materialises a nested-set tree model per trace at ingest time:
// every span gets left/right interval bounds plus the parent's left
// bound, with root spans carrying nestedSetParent == -1 and every
// non-root span a positive position (>= 1). The OTel-CH schema has no
// equivalent columns, but cerberus recomputes the exact numbering at
// query time from the (TraceId, SpanId, ParentSpanId) adjacency via
// chplan.NestedSetAnnotate (see select.go / nested_set_annotate.go).
//
// Two lowering shapes result:
//
//   - The root-span idiom `nestedSetParent <op> <int>` whose truth
//     depends only on root-ness (e.g. `nestedSetParent < 0`, what
//     Grafana's Traces Drilldown stamps on every query) reduces to a
//     cheap `ParentSpanId = ”` / `!= ”` test with no annotation pass.
//   - Every other position-dependent comparison
//     (`nestedSetParent = 5`, `nestedSetLeft > 0`,
//     `nestedSetParent = span.a`, float literals, …) compares against
//     the synthetic NestedSet*Column the annotation pass materialises.
//     lowerSpansetFilter detects the synthetic column reference and
//     wraps the scan in a NestedSetAnnotate so the recursive numbering
//     CTE backs the column. This matches reference Tempo's content, not
//     just its 2xx status.
//
// Returns handled=false when neither side references a nested-set
// intrinsic (the caller continues with generic lowering).
func lowerNestedSetBinary(b *traceql.BinaryOperation, op chplan.BinaryOp, s schema.Traces) (chplan.Expr, bool, error) {
	var attr traceql.Attribute
	var other traceql.FieldExpression
	flipped := false
	if a, ok := nestedSetIntrinsicAttr(b.LHS); ok {
		attr, other = a, b.RHS
	} else if a, ok := nestedSetIntrinsicAttr(b.RHS); ok {
		attr, other, flipped = a, b.LHS, true
	} else {
		return nil, false, nil
	}
	if flipped {
		op = flipComparisonOp(op)
	}

	// Fast path: a `nestedSetParent <op> <int-literal>` comparison whose
	// truth is constant across the non-root position domain reduces to a
	// ParentSpanId root-ness test (root parent = -1, every non-root
	// position >= 1) — no recursive numbering needed.
	if attr.Intrinsic == traceql.IntrinsicNestedSetParent {
		if lit, ok := fieldExprStatic(other); ok && lit.Type == traceql.TypeInt {
			if expr, ok := rootnessReduction(op, lit, s); ok {
				return expr, true, nil
			}
		}
	}

	// General path: compare against the synthetic nested-set column the
	// annotation pass materialises. The other operand lowers normally
	// (literal, span attribute, …); numeric coercion wraps any Map
	// subscript so an `= span.a` comparison resolves Int64-vs-Float.
	col, ok := nestedSetColumn(attr.Intrinsic)
	if !ok {
		return nil, true, fmt.Errorf("traceql: intrinsic %s is not a nested-set position", attr.Intrinsic)
	}
	rhs, err := lowerFieldExpr(other, s)
	if err != nil {
		return nil, true, err
	}
	left := chplan.Expr(&chplan.ColumnRef{Name: col})
	left, rhs = coerceNumericFieldAccess(op, left, rhs)
	return &chplan.Binary{Op: op, Left: left, Right: rhs}, true, nil
}

// rootSpanNestedSetParent is the nestedSetParent value Tempo assigns to a
// root span (no parent); every non-root span has a position >= 1.
const rootSpanNestedSetParent int64 = -1

// rootnessReduction returns the cheap ParentSpanId-based predicate for a
// `nestedSetParent <op> <int>` comparison whose result is constant
// across the non-root position domain (positions >= 1), or ok=false
// when the comparison genuinely depends on the position value (and must
// therefore go through the annotation pass).
func rootnessReduction(op chplan.BinaryOp, lit traceql.Static, s schema.Traces) (chplan.Expr, bool) {
	v64, _ := lit.Int()
	v := int64(v64)
	root, err := evalIntCmp(rootSpanNestedSetParent, op, v)
	if err != nil {
		return nil, false
	}
	nonRoot, constant := nonRootCmpConstant(op, v)
	if !constant {
		return nil, false
	}
	parentCol := &chplan.ColumnRef{Name: s.ParentSpanIDColumn}
	empty := &chplan.LitString{V: ""}
	switch {
	case root && !nonRoot:
		return &chplan.Binary{Op: chplan.OpEq, Left: parentCol, Right: empty}, true
	case !root && nonRoot:
		return &chplan.Binary{Op: chplan.OpNe, Left: parentCol, Right: empty}, true
	default:
		return &chplan.LitBool{V: root}, true
	}
}

// nestedSetIntrinsicAttr returns the attribute when e references one of
// the nested-set intrinsics.
func nestedSetIntrinsicAttr(e traceql.FieldExpression) (traceql.Attribute, bool) {
	a, ok := fieldExprAttribute(e)
	if !ok {
		return traceql.Attribute{}, false
	}
	switch a.Intrinsic {
	case traceql.IntrinsicNestedSetParent, traceql.IntrinsicNestedSetLeft, traceql.IntrinsicNestedSetRight:
		return a, true
	}
	return traceql.Attribute{}, false
}

// fieldExprStatic unwraps a FieldExpression into its Static literal
// (pointer or value form).
func fieldExprStatic(e traceql.FieldExpression) (traceql.Static, bool) {
	switch v := e.(type) {
	case *traceql.Static:
		if v == nil {
			return traceql.Static{}, false
		}
		return *v, true
	case traceql.Static:
		return v, true
	}
	return traceql.Static{}, false
}

// evalIntCmp evaluates `a op v` for two int64s. Errors on non-comparison
// ops (arithmetic / regex / logical never reach the nested-set path
// with a valid TraceQL parse, but fail loudly rather than guess).
func evalIntCmp(a int64, op chplan.BinaryOp, v int64) (bool, error) {
	switch op {
	case chplan.OpEq:
		return a == v, nil
	case chplan.OpNe:
		return a != v, nil
	case chplan.OpLt:
		return a < v, nil
	case chplan.OpLe:
		return a <= v, nil
	case chplan.OpGt:
		return a > v, nil
	case chplan.OpGe:
		return a >= v, nil
	}
	return false, fmt.Errorf("traceql: operator %s is unsupported on nestedSetParent", op)
}

// nonRootCmpConstant reports whether `p op v` has the same truth value
// for every possible non-root nested-set parent position p (p >= 1),
// and what that value is. When the result varies with p the comparison
// needs real nested-set positions and cannot be lowered.
func nonRootCmpConstant(op chplan.BinaryOp, v int64) (value, constant bool) {
	switch op {
	case chplan.OpEq:
		if v < 1 {
			return false, true
		}
	case chplan.OpNe:
		if v < 1 {
			return true, true
		}
	case chplan.OpLt:
		if v <= 1 {
			return false, true
		}
	case chplan.OpLe:
		if v < 1 {
			return false, true
		}
	case chplan.OpGt:
		if v < 1 {
			return true, true
		}
	case chplan.OpGe:
		if v <= 1 {
			return true, true
		}
	}
	return false, false
}

// lowerNestedIntrinsicBinary intercepts comparisons against the nested
// intrinsics (`event:name` / `link:traceID` / `link:spanID`), which map
// to direct subfields of the OTel-CH Nested columns — Events.Name,
// Links.TraceId, Links.SpanId — rather than to a flat span column or
// the per-row Attributes map. The lowering is a chplan.NestedArrayExists
// with an empty Key: the emitter compares each Nested-array element
// directly (`arrayExists(x -> x <op> <literal>, Events.Name)`).
//
// Returns handled=false when neither side references a nested intrinsic
// (the caller continues with the next interception / generic lowering).
func lowerNestedIntrinsicBinary(b *traceql.BinaryOperation, op chplan.BinaryOp, s schema.Traces) (chplan.Expr, bool, error) {
	build := func(a traceql.Attribute, valueSide traceql.FieldExpression, valueOp chplan.BinaryOp) (chplan.Expr, bool, error) {
		col, sub, ok := nestedIntrinsicTarget(a, s)
		if !ok {
			return nil, false, nil
		}
		val, err := lowerFieldExpr(valueSide, s)
		if err != nil {
			return nil, true, err
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: sub,
			Op:       valueOp,
			Value:    val,
		}, true, nil
	}
	if a, ok := fieldExprAttribute(b.LHS); ok {
		if expr, handled, err := build(a, b.RHS, op); handled {
			return expr, true, err
		}
	}
	if a, ok := fieldExprAttribute(b.RHS); ok {
		if expr, handled, err := build(a, b.LHS, flipComparisonOp(op)); handled {
			return expr, true, err
		}
	}
	return nil, false, nil
}

// nestedIntrinsicTarget maps a nested intrinsic to (Nested column,
// subfield). Returns ok=false for every other attribute, or when the
// configured schema has no column for the scope.
func nestedIntrinsicTarget(a traceql.Attribute, s schema.Traces) (col, sub string, ok bool) {
	switch a.Intrinsic {
	case traceql.IntrinsicEventName:
		if s.EventsColumn == "" {
			return "", "", false
		}
		return s.EventsColumn, "Name", true
	case traceql.IntrinsicLinkTraceID:
		if s.LinksColumn == "" {
			return "", "", false
		}
		return s.LinksColumn, "TraceId", true
	case traceql.IntrinsicLinkSpanID:
		if s.LinksColumn == "" {
			return "", "", false
		}
		return s.LinksColumn, "SpanId", true
	}
	return "", "", false
}

// lowerNestedAttrBinary recognises the
//
//	<link|event>.<name> <op> <literal>
//
// shape and returns a chplan.NestedArrayExists. The LHS/RHS may be in
// either order in upstream TraceQL ASTs; we normalise so the attribute
// reference is always the implicit `x[?]` and the literal is the RHS
// of the comparison. Returns ok=false when neither side is a link- or
// event-scoped attribute (the caller falls back to flat Binary lowering).
func lowerNestedAttrBinary(b *traceql.BinaryOperation, op chplan.BinaryOp, s schema.Traces) (chplan.Expr, bool) {
	if lhsAttr, ok := nestedScopedAttr(b.LHS); ok {
		col, key, ok := nestedAttrTarget(lhsAttr, s)
		if !ok {
			return nil, false
		}
		val, err := lowerFieldExpr(b.RHS, s)
		if err != nil {
			return nil, false
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Op:       op,
			Value:    val,
		}, true
	}
	if rhsAttr, ok := nestedScopedAttr(b.RHS); ok {
		col, key, ok := nestedAttrTarget(rhsAttr, s)
		if !ok {
			return nil, false
		}
		val, err := lowerFieldExpr(b.LHS, s)
		if err != nil {
			return nil, false
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Op:       flipComparisonOp(op),
			Value:    val,
		}, true
	}
	return nil, false
}

// nestedScopedAttr returns the attribute if e is a link- or event-scoped
// attribute reference (pointer or value form), so callers can branch
// without re-running the same type-switch twice.
func nestedScopedAttr(e traceql.FieldExpression) (traceql.Attribute, bool) {
	switch v := e.(type) {
	case *traceql.Attribute:
		if v == nil {
			return traceql.Attribute{}, false
		}
		if v.Scope == traceql.AttributeScopeLink || v.Scope == traceql.AttributeScopeEvent {
			return *v, true
		}
	case traceql.Attribute:
		if v.Scope == traceql.AttributeScopeLink || v.Scope == traceql.AttributeScopeEvent {
			return v, true
		}
	}
	return traceql.Attribute{}, false
}

// nestedAttrTarget maps a link- / event-scoped attribute to the Nested
// parent column it lives under (LinksColumn or EventsColumn) plus the
// attribute key to look up inside each row's Attributes map. Returns
// ok=false when the configured schema has no column for that scope —
// the caller falls back to the generic lowering and the resulting SQL
// will error at emit time (better than silently writing the wrong
// column name).
func nestedAttrTarget(a traceql.Attribute, s schema.Traces) (col, key string, ok bool) {
	switch a.Scope {
	case traceql.AttributeScopeLink:
		if s.LinksColumn == "" {
			return "", "", false
		}
		return s.LinksColumn, a.Name, true
	case traceql.AttributeScopeEvent:
		if s.EventsColumn == "" {
			return "", "", false
		}
		return s.EventsColumn, a.Name, true
	}
	return "", "", false
}

// flipComparisonOp swaps the direction of an asymmetric comparison so
// `<literal> <op> <attr>` rewrites cleanly to `<attr> <flipped> <literal>`.
// = / != / AND / OR are symmetric and pass through unchanged.
func flipComparisonOp(op chplan.BinaryOp) chplan.BinaryOp {
	switch op {
	case chplan.OpLt:
		return chplan.OpGt
	case chplan.OpLe:
		return chplan.OpGe
	case chplan.OpGt:
		return chplan.OpLt
	case chplan.OpGe:
		return chplan.OpLe
	}
	return op
}

// lowerAttribute resolves a TraceQL attribute reference to a chplan
// expression against the appropriate carrier column.
//
// Scope mapping:
//   - .name (no prefix), span.name → SpanAttributes['name']
//   - resource.name        → ResourceAttributes['name']
//   - intrinsic duration   → Duration
//   - intrinsic name       → SpanName
//   - intrinsic kind       → SpanKind
//   - intrinsic status     → StatusCode
//   - intrinsic statusMessage → StatusMessage
//   - intrinsic traceID    → TraceId
//   - intrinsic spanID     → SpanId
//   - intrinsic parent     → ParentSpanId
//   - intrinsic instrumentation:name    → ScopeName
//   - intrinsic instrumentation:version → ScopeVersion
//
// Intrinsics / scopes with no OTel-CH backing column resolve to the
// reference StaticNil cell — an empty string — when used in a value
// position (`| select(rootName)`, `| by(traceDuration)`, an aggregate
// operand). Reference Tempo executes the missing field to StaticNil,
// which renders as an empty/absent cell and (for by()) collapses every
// span into one nil-keyed group; `/api/search` returns 2xx, so cerberus
// must not 422. The earlier loud-rejection posture was itself the
// wrong_rejection the rejection-parity layer exists to catch. A nested
// intrinsic (event:name / link:traceID / link:spanID) in value position
// is handled by the dedicated group / select paths before reaching
// here; a bare reference that still arrives resolves to the same empty
// cell. Comparisons never reach this path — lowerAbsentFieldBinary /
// lowerTraceScopedBinary / lowerNestedSetBinary / lowerNestedIntrinsicBinary
// intercept them.
func lowerAttribute(a traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	if a.Intrinsic != traceql.IntrinsicNone {
		if col := intrinsicColumn(a.Intrinsic, s); col != "" {
			return &chplan.ColumnRef{Name: col}, nil
		}
		// Unbacked intrinsic in value position: the missing-cell empty
		// string mirrors reference's StaticNil render.
		return &chplan.LitString{V: ""}, nil
	}
	carrier := s.AttributesColumn
	switch a.Scope {
	case traceql.AttributeScopeResource:
		carrier = s.ResourceAttributesColumn
	case traceql.AttributeScopeSpan:
		carrier = s.AttributesColumn
	case traceql.AttributeScopeInstrumentation:
		// The upstream OTel-CH traces schema materialises ScopeName /
		// ScopeVersion but no scope-attributes map; a custom
		// instrumentation.<key> is absent on every span. Reference
		// resolves it to StaticNil — the empty missing-key cell — so
		// resolve to '' rather than reading SpanAttributes or rejecting.
		if s.ScopeAttributesColumn == "" {
			return &chplan.LitString{V: ""}, nil
		}
		carrier = s.ScopeAttributesColumn
	}
	return &chplan.FieldAccess{
		Source: &chplan.ColumnRef{Name: carrier},
		Path:   a.Name,
	}, nil
}

func intrinsicColumn(i traceql.Intrinsic, s schema.Traces) string {
	switch i {
	case traceql.IntrinsicDuration:
		return s.DurationColumn
	case traceql.IntrinsicName:
		return s.SpanNameColumn
	case traceql.IntrinsicKind:
		return s.SpanKindColumn
	case traceql.IntrinsicStatus:
		return s.StatusCodeColumn
	case traceql.IntrinsicStatusMessage:
		return s.StatusMessageColumn
	case traceql.IntrinsicTraceID:
		return s.TraceIDColumn
	case traceql.IntrinsicSpanID:
		return s.SpanIDColumn
	case traceql.IntrinsicParent:
		return s.ParentSpanIDColumn
	case traceql.IntrinsicInstrumentationName:
		return s.ScopeNameColumn
	case traceql.IntrinsicInstrumentationVersion:
		return s.ScopeVersionColumn
	}
	return ""
}

// lowerStatic turns a TraceQL Static literal into a chplan literal.
//
// TypeStatus and TypeKind map to the TitleCase string the OTel-CH
// exporter writes into StatusCode / SpanKind. Tempo's Status.String() /
// Kind.String() emits lowercase ("error", "client", …); we re-case
// here so `{ status = error }` matches CH's `'Error'` row.
func lowerStatic(st traceql.Static) (chplan.Expr, error) {
	switch st.Type {
	case traceql.TypeString:
		return &chplan.LitString{V: st.EncodeToString(false)}, nil
	case traceql.TypeInt:
		i, _ := st.Int()
		return &chplan.LitInt{V: int64(i)}, nil
	case traceql.TypeFloat:
		return &chplan.LitFloat{V: st.Float()}, nil
	case traceql.TypeBoolean:
		b, _ := st.Bool()
		return &chplan.LitBool{V: b}, nil
	case traceql.TypeDuration:
		// Durations encode as nanoseconds; emit as int64 since the
		// Duration column in OTel-CH is Int64 ns.
		d, _ := st.Duration()
		return &chplan.LitInt{V: d.Nanoseconds()}, nil
	case traceql.TypeStatus:
		s, ok := st.Status()
		if !ok {
			return nil, fmt.Errorf("traceql: static status literal has no Status() value")
		}
		return &chplan.LitString{V: statusString(s)}, nil
	case traceql.TypeKind:
		k, ok := st.Kind()
		if !ok {
			return nil, fmt.Errorf("traceql: static kind literal has no Kind() value")
		}
		return &chplan.LitString{V: kindString(k)}, nil
	}
	return nil, fmt.Errorf("traceql: static literal type %s is unsupported", st.Type)
}

// statusString maps Tempo's Status enum to the StatusCode string the
// OTel-CH exporter writes. Tempo's Status.String() is lowercase; CH
// rows carry TitleCase ("Unset" / "Ok" / "Error").
func statusString(s traceql.Status) string {
	switch s {
	case traceql.StatusError:
		return "Error"
	case traceql.StatusOk:
		return "Ok"
	case traceql.StatusUnset:
		return "Unset"
	}
	// Defensive: future enum values surface as-is rather than silently
	// producing an empty filter.
	return s.String()
}

// kindString maps Tempo's Kind enum to the SpanKind string the OTel-CH
// exporter writes. Tempo's Kind.String() is lowercase; CH rows carry
// TitleCase ("Internal" / "Client" / "Server" / "Producer" / "Consumer";
// "Unspecified" is the conventional unset value).
func kindString(k traceql.Kind) string {
	switch k {
	case traceql.KindUnspecified:
		return "Unspecified"
	case traceql.KindInternal:
		return "Internal"
	case traceql.KindClient:
		return "Client"
	case traceql.KindServer:
		return "Server"
	case traceql.KindProducer:
		return "Producer"
	case traceql.KindConsumer:
		return "Consumer"
	}
	return k.String()
}

func mapBinaryOp(op traceql.Operator) (chplan.BinaryOp, error) {
	switch op {
	case traceql.OpEqual:
		return chplan.OpEq, nil
	case traceql.OpNotEqual:
		return chplan.OpNe, nil
	case traceql.OpLess:
		return chplan.OpLt, nil
	case traceql.OpLessEqual:
		return chplan.OpLe, nil
	case traceql.OpGreater:
		return chplan.OpGt, nil
	case traceql.OpGreaterEqual:
		return chplan.OpGe, nil
	case traceql.OpRegex:
		return chplan.OpMatch, nil
	case traceql.OpNotRegex:
		return chplan.OpNotMatch, nil
	case traceql.OpAnd:
		return chplan.OpAnd, nil
	case traceql.OpOr:
		return chplan.OpOr, nil
	case traceql.OpAdd:
		return chplan.OpAdd, nil
	case traceql.OpSub:
		return chplan.OpSub, nil
	case traceql.OpMult:
		return chplan.OpMul, nil
	case traceql.OpDiv:
		return chplan.OpDiv, nil
	case traceql.OpMod:
		return chplan.OpMod, nil
	case traceql.OpPower:
		return chplan.OpPow, nil
	}
	return "", fmt.Errorf("traceql: operator %s is unsupported", op)
}
