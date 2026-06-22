// Package traceql lowers Tempo TraceQL queries into the shared cerberus
// chplan IR. Covers the SpansetFilter form (attribute matchers like
// `{ .service.name = "x" }`, `{ duration > 100ms }`,
// `{ span.http.status_code >= 500 }`), structural operators
// (`>>`/`>`), aggregators, time filters, and `| select(...)`.
package traceql

import (
	"context"
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

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
	// Bound the nested-set numbering walk to the N traces /api/search will
	// return (no-op unless the request set a limit AND the plan is a
	// select() over the Drilldown structure shape — see search_limit.go).
	plan = stampNestedSetTraceLimit(plan, searchTraceLimit(ctx), s)
	// Push the response trace limit + request window into the plain-search
	// row source (a bare Scan or Filter(Scan)) so /api/search drains only the
	// N newest traces in the window instead of buffering every matching span
	// (the summaries-drain OOM). No-op unless the request set a limit AND the
	// plan is a plain search — metrics / structural / set-op plans are left
	// unchanged.
	start, end := searchWindowFromCtx(ctx)
	plan = stampSearchTraceLimit(plan, searchTraceLimit(ctx), start, end, s)
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

	plan, err := lowerPipeline(expr.Pipeline, s)
	if err != nil {
		return nil, err
	}

	if expr.MetricsPipeline != nil {
		plan, err = lowerMetricsPipeline(plan, expr.MetricsPipeline, s)
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
// The IR + chsql emit foundation landed in PR #437. This lowering
// became wireable once tsouza/tempo v0.0.3-cerberus-accessors
// exposed Op() / Limit() / Value() / Elements() / Separators()
// accessors on the upstream-unexported SecondStageElement variants
// (mirrors the MetricsAggregate accessor pattern from #143).
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
// etc. Lowers as Aggregate (LHS) wrapped in a Filter on the output
// Value column.
func lowerScalarFilter(prev chplan.Node, sf traceql.ScalarFilter, s schema.Traces) (chplan.Node, error) {
	aggNode, err := lowerScalarExpr(prev, sf.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerScalarExpr(prev, sf.RHS, s)
	if err != nil {
		return nil, err
	}

	op, err := mapBinaryOp(sf.Op)
	if err != nil {
		return nil, err
	}

	// rhs is expected to be a chplan.Expr from a Static literal; the
	// LHS recursed back as a chplan.Node (Aggregate). For the typical
	// `count() > 0` shape, wrap aggNode with a Filter. The Tempo parser
	// happily accepts pathological forms like `{} | 0 > 0` (no aggregate
	// on either side) — type-assert before dereferencing so we return a
	// structured error instead of panicking.
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
		Predicate: &chplan.Binary{Op: op, Left: &chplan.ColumnRef{Name: "Value"}, Right: rhsExpr},
	}, nil
}

// lowerScalarExpr converts a TraceQL ScalarExpression into either a
// chplan.Node (when the expression aggregates / produces a series) or
// a chplan.Expr (when it's a literal). Returns `any`; callers
// type-assert based on context.
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
	// emitter renders an INNER JOIN (intersect) or UNION DISTINCT (union)
	// keyed on (TraceID, SpanID).
	if setOp, ok := mapSetOp(op.Op); ok {
		if setOp == chplan.SetUnion {
			// UNION DISTINCT matches arm columns positionally and
			// errors (CH code 258) when the counts differ. Structural
			// arms expose the narrow span envelope (3 keys + the
			// structuralExtraProjectionColumns list) while plain
			// filter arms expose `SELECT *`; mixing them — the exact
			// shape of Grafana Traces Drilldown's structure-tab query
			// `({...} &>> {...}) || ({...})` — needs the wide arm
			// projected down to the same ordered column list.
			left, right = alignUnionArms(left, right, s)
		}
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
	}, nil
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
func structuralExtraProjectionColumns(s schema.Traces) []string {
	cols := make([]string, 0, 10)
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
// exactly the narrow list so ClickHouse's positional UNION DISTINCT
// matches column-for-column. Same-shape pairs pass through untouched
// (two wide arms keep the legacy full-row dedup semantics).
func alignUnionArms(left, right chplan.Node, s schema.Traces) (chplan.Node, chplan.Node) {
	ln, rn := isNarrowSpanArm(left), isNarrowSpanArm(right)
	switch {
	case ln && !rn:
		return left, narrowSpanProjection(right, s)
	case !ln && rn:
		return narrowSpanProjection(left, s), right
	default:
		return left, right
	}
}

// isNarrowSpanArm reports whether n's output is the narrow span
// envelope rather than the full `SELECT *` table shape. SetOperation
// output mirrors its arms (alignUnionArms keeps them consistent, and
// the intersect emitter projects L.*), so recurse left. Project arms
// only arise from narrowSpanProjection itself within spanset
// expressions — `| select(...)` is a pipeline stage, never a set-op
// operand.
func isNarrowSpanArm(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.StructuralJoin:
		return true
	case *chplan.Project:
		return true
	case *chplan.SetOperation:
		return isNarrowSpanArm(v.Left)
	}
	return false
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
		return lowerPipeline(*v, s)
	case traceql.Pipeline:
		return lowerPipeline(v, s)
	}
	return nil, fmt.Errorf("traceql: spanset expression %T is unsupported", e)
}

// mapStructuralOp translates Tempo's structural Operator enum to the
// chplan.StructuralOp this emitter understands. Covers the positive
// relations (`>` / `<` / `>>` / `<<` / `~`), their negated variants
// (`!>` / `!<` / `!>>` / `!<<` / `!~`), and the union-prefixed
// variants (`&>` / `&<` / `&>>` / `&<<` / `&~`). The negated forms
// lower to the same StructuralJoin shape with a `Not…` Op constant;
// the emitter swaps the outer INNER JOIN for a LEFT ANTI JOIN. The
// union forms lower to a `Union…` Op constant; the emitter emits the
// positive relation twice (once projecting each side) glued with
// UNION DISTINCT.
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
	pred, err := lowerFieldExpr(f.Expression, s)
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
		return lowerUnaryOperation(*v, s)
	case traceql.UnaryOperation:
		return lowerUnaryOperation(v, s)
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
// Reference semantics (tsouza/tempo fork, pkg/traceql):
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
func lowerUnaryOperation(u traceql.UnaryOperation, s schema.Traces) (chplan.Expr, error) {
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
		return lowerUnaryNot(u, s)
	case traceql.OpSub:
		return lowerUnaryMinus(u, s)
	}
	return nil, fmt.Errorf("traceql: unary operator %s is unsupported", u.Op)
}

// lowerUnaryMinus lowers the arithmetic negation `-<numeric-expr>`
// (UnaryOperation{OpSub}) — e.g. `{ -span.foo > 0 }`,
// `{ -(span.a + span.b) = -5 }`, `{ -span.duration < 0ns }`.
//
// Reference semantics (tsouza/tempo fork, ast_execute.go
// UnaryOperation.execute, OpSub branch): the operand executes to a
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

// lowerUnaryNot lowers the boolean negation `!( <bool-expr> )`. Tempo's
// validator (ast_validate.go UnaryOperation.validate -> unaryTypesValid)
// requires the operand to type to a boolean — a parenthesised
// comparison such as `!(span.foo = 1)` or `!(kind = server)` — so the
// inner FieldExpression always lowers to a boolean chplan predicate. We
// wrap it in `not(...)`, matching reference execution
// (UnaryOperation.execute OpNot: `!b`). An absent attribute inside the
// inner comparison already folds to constant-false (lowerAbsentFieldBinary
// / coerce paths), and `not(false)` is true — the same value reference
// computes when the comparison evaluates StaticFalse on a missing span.
func lowerUnaryNot(u traceql.UnaryOperation, s schema.Traces) (chplan.Expr, error) {
	inner, err := lowerFieldExpr(u.Expression, s)
	if err != nil {
		return nil, err
	}
	return &chplan.FuncCall{Name: "not", Args: []chplan.Expr{inner}}, nil
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
	// materialise (instrumentation-scoped attributes; the trace-scoped /
	// per-event intrinsics rootName / rootServiceName / traceDuration /
	// childCount / event:timeSinceStart) resolve to StaticNil in
	// reference execution, so the comparison is constant-false (the
	// isMatchingOperand guard never matches a nil operand). Reference
	// `/api/search` accepts these — fold them to a constant predicate
	// instead of rejecting. Equality/inequality both collapse to false:
	// `nil = x` and `nil != x` are both StaticFalse upstream.
	if expr, handled := lowerAbsentFieldBinary(b, s); handled {
		return expr, nil
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
	lhs, err := lowerFieldExpr(b.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerFieldExpr(b.RHS, s)
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
	return &chplan.Binary{Op: op, Left: lhs, Right: rhs}, nil
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
// trace-scoped / per-event intrinsics with no per-span column. Span /
// resource attributes and intrinsics that DO map to a column
// (Duration, SpanName, StatusCode, …) return absent=false so the
// caller lowers them against their real carrier. Nested-set intrinsics
// are handled by their own dedicated path (lowerNestedSetBinary) and
// are not classified here.
func absentAttributePredicate(attr traceql.Attribute, s schema.Traces, negated bool) (chplan.Expr, bool) {
	if !attributeHasNoBacking(attr, s) {
		return nil, false
	}
	return &chplan.LitBool{V: negated}, true
}

// attributeHasNoBacking reports whether attr names a carrier the OTel-CH
// traces schema does not materialise. Instrumentation-scoped attributes
// have no scope-attributes map; the trace-scoped and per-event
// intrinsics (rootName / rootServiceName / traceDuration / traceStart /
// childCount / event:timeSinceStart) have no per-span column. Every
// other attribute (span / resource maps, intrinsics with a column)
// has a real backing.
func attributeHasNoBacking(attr traceql.Attribute, s schema.Traces) bool {
	if attr.Intrinsic == traceql.IntrinsicNone {
		return attr.Scope == traceql.AttributeScopeInstrumentation && s.ScopeAttributesColumn == ""
	}
	switch attr.Intrinsic {
	case traceql.IntrinsicTraceRootService, traceql.IntrinsicTraceRootSpan,
		traceql.IntrinsicTraceDuration, traceql.ScopedIntrinsicTraceRootName,
		traceql.ScopedIntrinsicTraceRootService, traceql.ScopedIntrinsicTraceDuration,
		traceql.IntrinsicTraceStartTime, traceql.IntrinsicChildCount,
		traceql.IntrinsicEventTimeSinceStart:
		return true
	}
	return false
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
	return lhs, rhs
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

// rootnessReduction returns the cheap ParentSpanId-based predicate for a
// `nestedSetParent <op> <int>` comparison whose result is constant
// across the non-root position domain (positions >= 1), or ok=false
// when the comparison genuinely depends on the position value (and must
// therefore go through the annotation pass).
func rootnessReduction(op chplan.BinaryOp, lit traceql.Static, s schema.Traces) (chplan.Expr, bool) {
	v64, _ := lit.Int()
	v := int64(v64)
	root, err := evalIntCmp(-1, op, v)
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
// lowerNestedSetBinary / lowerNestedIntrinsicBinary intercept them.
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
